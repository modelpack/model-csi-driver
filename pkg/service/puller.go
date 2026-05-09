package service

import (
	"context"
	"io"
	"os"
	"strings"

	"github.com/modelpack/modctl/pkg/backend"
	modctlConfig "github.com/modelpack/modctl/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/cas"
	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/config/auth"
	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/modelpack/model-csi-driver/pkg/status"
	"github.com/pkg/errors"
)

type Puller interface {
	Pull(ctx context.Context, reference, targetDir string, excludeModelWeights bool, excludeFilePatterns []string) error
}

// PullerDeps bundles optional dependencies that the worker plumbs into each
// puller instance.
type PullerDeps struct {
	CAS      *cas.Store
	OwnerKey string
}

var NewPuller = func(ctx context.Context, pullCfg *config.PullConfig, hook *status.Hook, diskQuotaChecker *DiskQuotaChecker, deps PullerDeps) Puller {
	return &puller{
		pullCfg:          pullCfg,
		hook:             hook,
		diskQuotaChecker: diskQuotaChecker,
		deps:             deps,
	}
}

type puller struct {
	pullCfg          *config.PullConfig
	hook             *status.Hook
	diskQuotaChecker *DiskQuotaChecker
	deps             PullerDeps
}

// wrapHooks composes the inner status.Hook with the CAS hook adapter when a
// store is configured. The returned value implements modctl's PullHooks
// interface and so satisfies both pull / fetch configurations.
func (p *puller) wrapHooks(ctx context.Context, extractDir string) cas.PullHooks {
	if p.deps.CAS == nil || p.deps.OwnerKey == "" {
		return p.hook
	}
	return cas.NewPullHook(ctx, p.deps.CAS, p.hook, p.deps.OwnerKey, extractDir)
}

func (p *puller) Pull(ctx context.Context, reference, targetDir string, excludeModelWeights bool, excludeFilePatterns []string) error {
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
		if err := p.diskQuotaChecker.Check(ctx, modelArtifact, excludeModelWeights, excludeFilePatterns); err != nil {
			return errors.Wrap(err, "check disk quota")
		}
	}

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return errors.Wrapf(err, "create model dir: %s", targetDir)
	}

	if !excludeModelWeights && len(excludeFilePatterns) == 0 {
		pullConfig := modctlConfig.NewPull()
		pullConfig.Concurrency = int(p.pullCfg.Concurrency)
		pullConfig.PlainHTTP = plainHTTP
		pullConfig.Proxy = p.pullCfg.ProxyURL
		pullConfig.DragonflyEndpoint = p.pullCfg.DragonflyEndpoint
		pullConfig.Insecure = true
		pullConfig.ExtractDir = targetDir
		pullConfig.ExtractFromRemote = true
		pullConfig.Hooks = p.wrapHooks(ctx, targetDir)
		pullConfig.ProgressWriter = io.Discard
		pullConfig.DisableProgress = true

		if err := b.Pull(ctx, reference, pullConfig); err != nil {
			logger.WithContext(ctx).WithError(err).Errorf("failed to pull model image: %s", reference)
			return errors.Wrap(err, "pull model image")
		}

		return nil
	}

	patterns, total, err := modelArtifact.GetPatterns(ctx, excludeModelWeights, excludeFilePatterns)
	if err != nil {
		return errors.Wrap(err, "get model file patterns without weights")
	}

	if len(patterns) == 0 {
		logger.WithContext(ctx).Infof("no files to fetch from model: %s", reference)
		return nil
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
	fetchConfig.Hooks = p.wrapHooks(ctx, targetDir)
	fetchConfig.ProgressWriter = io.Discard
	fetchConfig.DisableProgress = true
	fetchConfig.Patterns = patterns

	if err := b.Fetch(ctx, reference, fetchConfig); err != nil {
		logger.WithContext(ctx).WithError(err).Errorf("failed to fetch model: %s", reference)
		return errors.Wrap(err, "fetch model")
	}

	return nil
}
