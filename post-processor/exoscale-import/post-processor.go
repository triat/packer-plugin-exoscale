package exoscaleimport

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	s3manager "github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	egoscale "github.com/exoscale/egoscale/v2"
	exoapi "github.com/exoscale/egoscale/v2/api"
	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/hashicorp/packer-plugin-sdk/multistep/commonsteps"
	"github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/version"
)

const (
	qemuBuilderID     = "transcend.qemu"
	fileBuilderID     = "packer.file"
	artificeBuilderID = "packer.post-processor.artifice"
)

func init() {
	egoscale.UserAgent = fmt.Sprintf("Exoscale-Packer-Post-Processor/%s %s",
		version.SDKVersion.FormattedVersion(), egoscale.UserAgent)
}

type exoscaleClient interface {
	CopyTemplate(context.Context, string, *egoscale.Template, string) (*egoscale.Template, error)
	DeleteTemplate(context.Context, string, *egoscale.Template) error
	RegisterTemplate(context.Context, string, *egoscale.Template) (*egoscale.Template, error)
}

type s3Client interface {
	s3manager.UploadAPIClient

	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

type PostProcessor struct {
	config *Config
	runner multistep.Runner
	exo    exoscaleClient
	sos    s3Client
}

func (p *PostProcessor) Configure(raws ...interface{}) error {
	config, err := NewConfig(raws...)
	if err != nil {
		return err
	}
	p.config = config

	packer.LogSecretFilter.Set(p.config.APIKey, p.config.APISecret)

	return nil
}

func (p *PostProcessor) PostProcess(
	ctx context.Context,
	ui packer.Ui,
	a packer.Artifact,
) (packer.Artifact, bool, bool, error) {
	switch a.BuilderId() {
	case qemuBuilderID, fileBuilderID, artificeBuilderID:
		break
	default:
		err := fmt.Errorf("unsupported artifact type %q: this post-processor only supports "+
			"artifacts from QEMU/file builders and Artifice post-processor", a.BuilderId())
		return nil, false, false, err
	}

	exo, err := egoscale.NewClient(
		p.config.APIKey,
		p.config.APISecret,
		egoscale.ClientOptWithTimeout(time.Duration(p.config.APITimeout*int64(time.Second))),
	)
	if err != nil {
		return nil, false, false, fmt.Errorf("unable to initialize Exoscale client: %w", err)
	}
	p.exo = exo

	cfg, err := awsconfig.LoadDefaultConfig(
		ctx,
		awsconfig.WithRegion(p.config.ImageZone),

		awsconfig.WithEndpointResolver(aws.EndpointResolverFunc(
			func(service, region string) (aws.Endpoint, error) {
				return aws.Endpoint{
					URL:           p.config.SOSEndpoint,
					SigningRegion: p.config.ImageZone,
				}, nil
			})),

		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			p.config.APIKey,
			p.config.APISecret,
			"")),
	)
	if err != nil {
		return nil, false, false, fmt.Errorf("unable to initialize SOS client: %w", err)
	}

	p.sos = s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
	})

	state := new(multistep.BasicStateBag)
	state.Put("ui", ui)
	state.Put("artifact", a)
	// A single template is registered (once) in the first zone and then copied as many times
	// as they are additional zones. We must keep track of each template-zone, as final artifact
	state.Put("templates", []*egoscale.Template{})

	steps := []multistep.Step{
		&stepUploadImage{postProcessor: p},
		&stepRegisterTemplate{postProcessor: p},
		&stepCopyTemplate{postProcessor: p},
	}

	ctx = exoapi.WithEndpoint(ctx, exoapi.NewReqEndpoint(p.config.APIEnvironment, p.config.ImageZone))

	p.runner = commonsteps.NewRunnerWithPauseFn(steps, p.config.PackerConfig, ui, state)
	p.runner.Run(ctx, state)

	if rawErr, ok := state.GetOk("error"); ok {
		return nil, false, false, rawErr.(error)
	}

	if _, ok := state.GetOk(multistep.StateCancelled); ok {
		return nil, false, false, errors.New("post-processing cancelled")
	}

	if _, ok := state.GetOk(multistep.StateHalted); ok {
		return nil, false, false, errors.New("post-processing halted")
	}

	templates, ok := state.GetOk("templates")
	if !ok {
		return nil, false, false, errors.New("unable to find templates in state")
	}

	return &Artifact{
		StateData: map[string]interface{}{"generated_data": state.Get("generated_data")},

		postProcessor: p,
		state:         state,
		templates:     templates.([]*egoscale.Template),
	}, false, false, nil
}
