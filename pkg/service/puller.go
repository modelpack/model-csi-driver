package service

import (
	"context"
	"io"
	"os"
	"strings"

	"github.com/modelpack/modctl/pkg/backend"
	modctlConfig "github.com/modelpack/modctl/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/config/auth"
	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/modelpack/model-csi-driver/pkg/status"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

type PullHook interface {
	BeforePullLayer(desc ocispec.Descriptor, manifest ocispec.Manifest)
	AfterPullLayer(desc ocispec.Descriptor, err error)
}

type Puller interface {
	Pull(ctx context.Context, reference, targetDir string, excludeModelWeights bool) error
}

var NewPuller = func(ctx context.Context, pullCfg *config.PullConfig, hook *status.Hook, diskQuotaChecker *DiskQuotaChecker) Puller {
	return &puller{
		pullCfg:          pullCfg,
		hook:             hook,
		diskQuotaChecker: diskQuotaChecker,
	}
}

type puller struct {
	pullCfg          *config.PullConfig
	hook             *status.Hook
	diskQuotaChecker *DiskQuotaChecker
}

func (p *puller) Pull(ctx context.Context, reference, targetDir string, excludeModelWeights bool) error {
	keyChain, err := auth.GetKeyChainByRef(reference)
	if err != nil {
		return errors.Wrapf(err, "get auth for model: %s", reference)
	}
	plainHTTP := keyChain.ServerScheme == "http"

	b, err := backend.New("")
	if err != nil {
		return errors.Wrap(err, "create modctl backend")
	}

	modelArtifact := NewModelArtifact(b, reference, plainHTTP)

	if p.diskQuotaChecker != nil {
		if err := p.diskQuotaChecker.Check(ctx, modelArtifact, excludeModelWeights); err != nil {
			return errors.Wrap(err, "check disk quota")
		}
	}

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return errors.Wrapf(err, "create model dir: %s", targetDir)
	}

	if !excludeModelWeights {
		pullConfig := modctlConfig.NewPull()
		pullConfig.Concurrency = int(p.pullCfg.Concurrency)
		pullConfig.PlainHTTP = plainHTTP
		pullConfig.Proxy = p.pullCfg.ProxyURL
		pullConfig.DragonflyEndpoint = p.pullCfg.DragonflyEndpoint
		pullConfig.Insecure = true
		pullConfig.ExtractDir = targetDir
		pullConfig.ExtractFromRemote = true
		pullConfig.Hooks = p.hook
		pullConfig.ProgressWriter = io.Discard
		pullConfig.DisableProgress = true

		if err := b.Pull(ctx, reference, pullConfig); err != nil {
			logger.WithContext(ctx).WithError(err).Errorf("failed to pull model image: %s", reference)
			return errors.Wrap(err, "pull model image")
		}

		return nil
	}

	patterns, total, err := modelArtifact.GetPatterns(ctx, excludeModelWeights)
	if err != nil {
		return errors.Wrap(err, "get model file patterns without weights")
	}

	logger.WithContext(ctx).Infof(
		"fetching partial files from model: %s, files: %s (%d/%d)",
		reference, strings.Join(patterns, ", "), len(patterns), total,
	)
	p.hook.SetTotal(len(patterns))

	fetchConfig := modctlConfig.NewFetch()
	fetchConfig.Concurrency = int(p.pullCfg.Concurrency)
	fetchConfig.PlainHTTP = plainHTTP
	fetchConfig.Proxy = p.pullCfg.ProxyURL
	fetchConfig.DragonflyEndpoint = p.pullCfg.DragonflyEndpoint
	fetchConfig.Insecure = true
	fetchConfig.Output = targetDir
	fetchConfig.Hooks = p.hook
	fetchConfig.ProgressWriter = io.Discard
	fetchConfig.DisableProgress = true
	fetchConfig.Patterns = patterns

	if err := b.Fetch(ctx, reference, fetchConfig); err != nil {
		logger.WithContext(ctx).WithError(err).Errorf("failed to fetch model: %s", reference)
		return errors.Wrap(err, "fetch model")
	}

	return nil
}
