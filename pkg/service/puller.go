package service

import (
	"context"
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
	Pull(ctx context.Context, reference, targetDir string, excludeModelWeights bool, excludeFilePatterns []string) error
}

var NewPuller = func(ctx context.Context, pullCfg *config.PullConfig, hook *status.Hook, diskQuotaChecker *DiskQuotaChecker, excludeFilePatterns []string) Puller {
	return &puller{
		pullCfg:            pullCfg,
		hook:               hook,
		diskQuotaChecker:   diskQuotaChecker,
		excludeFilePatterns: excludeFilePatterns,
	}
}

type puller struct {
	pullCfg            *config.PullConfig
	hook               *status.Hook
	diskQuotaChecker   *DiskQuotaChecker
	excludeFilePatterns []string
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
		if err := p.diskQuotaChecker.Check(ctx, modelArtifact, excludeModelWeights); err != nil {
			return errors.Wrap(err, "check disk quota")
		}
	}

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return errors.Wrapf(err, "create model dir: %s", targetDir)
	}

	// Determine which files to fetch/pull based on patterns
	var fetchPatterns []string
	if len(excludeFilePatterns) > 0 {
		// Apply exclude patterns to all available files
		// First, get all layers (files) from the model
		patterns, err := modelArtifact.GetPatterns(ctx, excludeModelWeights)
		if err != nil {
			return errors.Wrap(err, "get all model layers")
		}

		// Create matcher from user-provided patterns
		matcher, err := NewFilePatternMatcher(excludeFilePatterns)
		if err != nil {
			return errors.Wrap(err, "create file pattern matcher")
		}

		// Filter files: include only those NOT matched by exclusion patterns
		for _, pattern := range patterns {
			// Check if this file should be included (not matched by exclusion patterns)
			if !matcher.Match(pattern) {
				fetchPatterns = append(fetchPatterns, pattern)
				logger.WithContext(ctx).Infof("Including file from fetch: %s", pattern)
			} else {
				logger.WithContext(ctx).Infof("Excluding file from fetch: %s", pattern)
			}
		}

		if len(fetchPatterns) == 0 {
			logger.WithContext(ctx).Warn("No files matched include patterns, all files would be excluded")
		}
	} else {
		// No exclude patterns, fetch all non-weight files (original behavior)
		patterns, err := modelArtifact.GetPatterns(ctx, excludeModelWeights)
		if err != nil {
			return errors.Wrap(err, "get model file patterns without weights")
		}

		logger.WithContext(ctx).Infof(
			"fetching model without weights: %s, file patterns: %s",
			reference, strings.Join(patterns, ", "),
		)

		fetchPatterns = patterns
	}

	// Fetch files
	fetchConfig := modctlConfig.NewFetch()
	fetchConfig.Concurrency = int(p.pullCfg.Concurrency)
	fetchConfig.PlainHTTP = plainHTTP
	fetchConfig.Proxy = p.pullCfg.ProxyURL
	fetchConfig.Insecure = true
	fetchConfig.Output = targetDir
	fetchConfig.Patterns = fetchPatterns

	if err := b.Fetch(ctx, reference, fetchConfig); err != nil {
		logger.WithContext(ctx).WithError(err).Errorf("failed to fetch model: %s", reference)
		return errors.Wrap(err, "fetch model")
	}

	// Apply file pattern filtering if exclude_file_patterns are provided
	if len(excludeFilePatterns) > 0 {
		matcher, err := NewFilePatternMatcher(excludeFilePatterns)
		if err != nil {
			return errors.Wrap(err, "create file pattern matcher")
		}

		logger.WithContext(ctx).Infof("Applying file exclusion patterns: %v", excludeFilePatterns)

		_, err = filterFilesByPatterns(targetDir, matcher)
		if err != nil {
			return errors.Wrap(err, "filter files by patterns")
		}
	}

	return nil
}
