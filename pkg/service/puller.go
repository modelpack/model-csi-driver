package service

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	oldModelspec "github.com/dragonflyoss/model-spec/specs-go/v1"
	"github.com/modelpack/modctl/pkg/backend"
	"github.com/modelpack/modctl/pkg/backend/remote"
	pkgcodec "github.com/modelpack/modctl/pkg/codec"
	modctlConfig "github.com/modelpack/modctl/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/config/auth"
	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/modelpack/model-csi-driver/pkg/status"
	modelspec "github.com/modelpack/model-spec/specs-go/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
)

type PullHook interface {
	LayerCached(desc ocispec.Descriptor, manifest ocispec.Manifest)
	BeforePullLayer(desc ocispec.Descriptor, manifest ocispec.Manifest)
	AfterPullLayer(desc ocispec.Descriptor, err error)
}

type Puller interface {
	Pull(ctx context.Context, reference, targetDir string, excludeModelWeights bool, excludeFilePatterns []string) error
}

var NewPuller = func(ctx context.Context, pullCfg *config.PullConfig, hook *status.Hook, diskQuotaChecker *DiskQuotaChecker) Puller {
	return &puller{
		pullCfg:          pullCfg,
		hook:             hook,
		diskQuotaChecker: diskQuotaChecker,
	}
}

// NewLayerAwarePuller creates a puller that leverages the LayerCache for
// layer-level deduplication. It resolves the manifest ahead of time,
// hardlinks cached layers, and only pulls missing layers.
var NewLayerAwarePuller = func(ctx context.Context, pullCfg *config.PullConfig, hook *status.Hook, diskQuotaChecker *DiskQuotaChecker, lc *LayerCache) Puller {
	return &layerAwarePuller{
		pullCfg:          pullCfg,
		hook:             hook,
		diskQuotaChecker: diskQuotaChecker,
		layerCache:       lc,
	}
}

type puller struct {
	pullCfg          *config.PullConfig
	hook             *status.Hook
	diskQuotaChecker *DiskQuotaChecker
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
		pullConfig.Hooks = p.hook
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

// layerAwarePuller wraps the pull flow with layer-level deduplication.
// It resolves the manifest first, checks the LayerCache for each layer,
// hardlinks cached layers, and only pulls missing ones via the Fetch path.
type layerAwarePuller struct {
	pullCfg          *config.PullConfig
	hook             *status.Hook
	diskQuotaChecker *DiskQuotaChecker
	layerCache       *LayerCache
}

func (p *layerAwarePuller) Pull(ctx context.Context, reference, targetDir string, excludeModelWeights bool, excludeFilePatterns []string) error {
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

	// If filtering is active, fall back to the standard puller (no dedup
	// for partial pulls since layer content is subset-based).
	if excludeModelWeights || len(excludeFilePatterns) > 0 {
		return p.pullWithFiltering(ctx, b, modelArtifact, reference, targetDir, plainHTTP, excludeModelWeights, excludeFilePatterns)
	}

	// Inspect the model to get layer descriptors.
	artifact, err := modelArtifact.Inspect(ctx, reference)
	if err != nil {
		logger.WithContext(ctx).WithError(err).Warnf("layer-aware pull: failed to inspect model, falling back to standard pull")
		return p.standardPull(ctx, b, reference, targetDir, plainHTTP)
	}

	return p.layerAwarePull(ctx, b, artifact, reference, targetDir, plainHTTP)
}

// layerAwarePull performs the deduplication-aware pull flow.
func (p *layerAwarePuller) layerAwarePull(
	ctx context.Context,
	b backend.Backend,
	artifact *backend.InspectedModelArtifact,
	reference, targetDir string,
	plainHTTP bool,
) error {
	if p.layerCache == nil {
		return p.standardPull(ctx, b, reference, targetDir, plainHTTP)
	}

	// 1. Resolve remote manifest directly to get full descriptors.
	ref, err := backend.ParseReference(reference)
	if err != nil {
		return errors.Wrap(err, "parse reference")
	}

	client, err := remote.New(ref.Repository(), remote.WithPlainHTTP(plainHTTP), remote.WithInsecure(true))
	if err != nil {
		return errors.Wrap(err, "create remote client")
	}

	_, manifestReader, err := client.Manifests().FetchReference(ctx, ref.Tag())
	if err != nil {
		return errors.Wrap(err, "fetch manifest")
	}
	defer func() { _ = manifestReader.Close() }()

	var manifest ocispec.Manifest
	if err := json.NewDecoder(manifestReader).Decode(&manifest); err != nil {
		return errors.Wrap(err, "decode manifest")
	}

	// 2. Classify layers as cached or uncached.
	var uncachedLayers []ocispec.Descriptor
	var cachedLayers []cachedLayerInfo
	var metadataEntries []LayerMetadataEntry

	for _, layer := range manifest.Layers {
		fp := getLayerFilePath(layer)
		if fp == "" {
			continue // skip layers without filepaths
		}

		metadataEntries = append(metadataEntries, LayerMetadataEntry{
			Digest:   layer.Digest.String(),
			FilePath: fp,
			Size:     layer.Size,
		})

		sourcePath, found := p.layerCache.Lookup(layer.Digest)
		if found {
			cachedLayers = append(cachedLayers, cachedLayerInfo{
				desc:       layer,
				sourcePath: sourcePath,
				filePath:   fp,
			})
		} else {
			uncachedLayers = append(uncachedLayers, layer)
		}
	}

	logger.WithContext(ctx).Infof(
		"layer-aware pull: %s — %d cached, %d to pull (total %d layers)",
		reference, len(cachedLayers), len(uncachedLayers), len(manifest.Layers),
	)

	p.hook.SetTotal(len(manifest.Layers))

	// Step 3: Hardlink cached layers.
	for _, cl := range cachedLayers {
		destPath := filepath.Join(targetDir, cl.filePath)
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			logger.WithContext(ctx).WithError(err).Warnf(
				"layer-aware pull: failed to create dir for hardlink %s, will pull instead", cl.filePath,
			)
			uncachedLayers = append(uncachedLayers, cl.desc)
			continue
		}

		if err := os.Link(cl.sourcePath, destPath); err != nil {
			logger.WithContext(ctx).WithError(err).Warnf(
				"layer-aware pull: hardlink failed for %s (src=%s), will pull instead",
				cl.filePath, cl.sourcePath,
			)
			uncachedLayers = append(uncachedLayers, cl.desc)
			continue
		}

		logger.WithContext(ctx).Infof(
			"layer-aware pull: reused layer %s via hardlink (%s → %s)",
			cl.desc.Digest, cl.sourcePath, destPath,
		)

		// Immediately register the new path so others can use it.
		p.layerCache.Register(cl.desc.Digest, destPath)
		// Record progress silently for cached layers.
		p.hook.LayerCached(cl.desc, manifest)
	}

	// Step 4: Pull uncached layers concurrently, synchronized by singleflight.
	if len(uncachedLayers) > 0 {
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(int(p.pullCfg.Concurrency))
		sfg := p.layerCache.SflightGroup()
		sem := p.layerCache.Semaphore()

		for _, layerDesc := range uncachedLayers {
			desc := layerDesc // capture for goroutine
			g.Go(func() error {
				for attempt := 0; attempt < 3; attempt++ {
					select {
					case <-gctx.Done():
						return gctx.Err()
					default:
					}

					// The singleflight key is the digest, guaranteeing only one download across all pods.
					_, sfgErr, _ := sfg.Do(desc.Digest.String(), func() (interface{}, error) {
						// Apply node-level flow control securely around network operations.
						if sem != nil {
							if err := sem.Acquire(gctx, 1); err != nil {
								return nil, err
							}
							defer sem.Release(1)
						}

						p.hook.BeforePullLayer(desc, manifest)

						// Defer the after hook to guarantee it runs on panic or error.
						var pullErr error
						defer func() {
							p.hook.AfterPullLayer(desc, pullErr)
						}()

						pullErr = func() error {
							// Open network stream to registry.
							reader, err := client.Fetch(gctx, desc)
							if err != nil {
								return errors.Wrap(err, "fetch blob from remote")
							}
							defer func() { _ = reader.Close() }()

							// Create codec to decode stream to disk.
							codec, err := pkgcodec.New(pkgcodec.TypeFromMediaType(desc.MediaType))
							if err != nil {
								return errors.Wrapf(err, "create codec for media type %s", desc.MediaType)
							}

							fp := getLayerFilePath(desc)
							if err := codec.Decode(targetDir, fp, reader, desc); err != nil {
								// Check if another concurrent process (outside our driver) wrote it.
								if errors.Is(err, pkgcodec.ErrAlreadyUpToDate) {
									return nil
								}
								return errors.Wrap(err, "decode layer")
							}

							// Registration only occurs on successful completion.
							fullPath := filepath.Join(targetDir, fp)
							if _, statErr := os.Stat(fullPath); statErr == nil {
								p.layerCache.Register(desc.Digest, fullPath)
							}

							return nil
						}()

						return nil, pullErr
					})

					// If the singleflight function returned an error, retry the loop.
					if sfgErr != nil {
						logger.WithContext(ctx).WithError(sfgErr).Warnf("layer-aware pull: network fetch failed for %s, retrying", desc.Digest)
						time.Sleep(1 * time.Second)
						continue
					}

					// If another goroutine successfully downloaded the file via singleflight,
					// we still need to hardlink it into OUR target directory since we bypassed
					// our own download logic.
					fp := getLayerFilePath(desc)
					destPath := filepath.Join(targetDir, fp)

					// Verify if the file is already at our destination (e.g. if WE were the downloader).
					if _, statErr := os.Stat(destPath); statErr == nil {
						return nil // We did the download, or it's already there.
					}

					// We were a waiting caller. We must hardlink from the newly cached location.
					sourcePath, found := p.layerCache.Lookup(desc.Digest)
					if !found {
						logger.WithContext(ctx).Warnf("layer-aware pull: singleflight completed but digest %s not in cache, retrying", desc.Digest)
						time.Sleep(1 * time.Second)
						continue
					}

					if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
						return err
					}
					if err := os.Link(sourcePath, destPath); err != nil {
						// Hardlink failed (EXDEV or source was deleted). Remove stale entry and retry fetch.
						logger.WithContext(ctx).WithError(err).Warnf("layer-aware pull: hardlink after singleflight failed for %s, retrying", desc.Digest)
						p.layerCache.RemoveByPrefix(sourcePath)
						time.Sleep(1 * time.Second)
						continue
					}
					p.layerCache.Register(desc.Digest, destPath)

					// Record progress silently for waiting pods.
					p.hook.LayerCached(desc, manifest)

					return nil
				}
				return errors.Errorf("layer-aware pull: failed to pull layer %s after 3 attempts", desc.Digest)
			})
		}

		if err := g.Wait(); err != nil {
			logger.WithContext(ctx).WithError(err).Errorf("layer-aware pull: failed to fetch uncached layers for %s", reference)
			return err
		}
	}

	// Step 5: Save layer metadata for restart rebuild.
	if len(metadataEntries) > 0 {
		metadataPath := filepath.Join(filepath.Dir(targetDir), "layer_digests.json")
		if err := saveLayerMetadata(metadataPath, metadataEntries); err != nil {
			logger.WithContext(ctx).WithError(err).Warnf("layer-aware pull: failed to save layer metadata")
		}
	}

	return nil
}

// standardPull falls back to the normal full pull path.
func (p *layerAwarePuller) standardPull(ctx context.Context, b backend.Backend, reference, targetDir string, plainHTTP bool) error {
	trackingHook := &layerTrackingHook{
		inner:     p.hook,
		cache:     p.layerCache,
		targetDir: targetDir,
		sem:       p.layerCache.Semaphore(),
	}

	pullConfig := modctlConfig.NewPull()
	pullConfig.Concurrency = int(p.pullCfg.Concurrency)
	pullConfig.PlainHTTP = plainHTTP
	pullConfig.Proxy = p.pullCfg.ProxyURL
	pullConfig.DragonflyEndpoint = p.pullCfg.DragonflyEndpoint
	pullConfig.Insecure = true
	pullConfig.ExtractDir = targetDir
	pullConfig.ExtractFromRemote = true
	pullConfig.Hooks = trackingHook
	pullConfig.ProgressWriter = io.Discard
	pullConfig.DisableProgress = true

	if err := b.Pull(ctx, reference, pullConfig); err != nil {
		logger.WithContext(ctx).WithError(err).Errorf("failed to pull model image: %s", reference)
		return errors.Wrap(err, "pull model image")
	}

	return nil
}

// pullWithFiltering handles pulls with weight/file-pattern exclusions.
func (p *layerAwarePuller) pullWithFiltering(
	ctx context.Context,
	b backend.Backend,
	modelArtifact *ModelArtifact,
	reference, targetDir string,
	plainHTTP bool,
	excludeModelWeights bool,
	excludeFilePatterns []string,
) error {
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

// cachedLayerInfo holds info about a layer found in the cache.
type cachedLayerInfo struct {
	desc       ocispec.Descriptor
	sourcePath string
	filePath   string
}

// layerTrackingHook wraps the status.Hook to additionally register pulled
// layers in the LayerCache after successful pull, and enforce node-level
// concurrency via the semaphore.
type layerTrackingHook struct {
	inner     *status.Hook
	cache     *LayerCache
	targetDir string
	sem       *semaphore.Weighted
}

func (h *layerTrackingHook) BeforePullLayer(desc ocispec.Descriptor, manifest ocispec.Manifest) {
	// Enforce node-level flow control by acquiring a semaphore slot.
	// We use 1 slot per layer (count-based, not size-based) to keep it simple
	// and avoid potential issues with very large layers blocking all slots.
	if h.sem != nil {
		// Use a background context so we don't fail the pull if the parent
		// context is cancelled while waiting — the pull itself handles cancellation.
		_ = h.sem.Acquire(context.Background(), 1)
	}

	h.inner.BeforePullLayer(desc, manifest)
}

func (h *layerTrackingHook) AfterPullLayer(desc ocispec.Descriptor, err error) {
	// Release the semaphore slot.
	if h.sem != nil {
		h.sem.Release(1)
	}

	h.inner.AfterPullLayer(desc, err)

	// Only register on successful pull.
	if err != nil || h.cache == nil {
		return
	}

	// Determine the file path from the layer descriptor annotations.
	filePath := getLayerFilePath(desc)
	if filePath == "" {
		return
	}

	fullPath := filepath.Join(h.targetDir, filePath)
	if _, statErr := os.Stat(fullPath); statErr != nil {
		return // file doesn't exist, cannot register
	}

	h.cache.Register(desc.Digest, fullPath)
}

// getLayerFilePath extracts the file path from a layer descriptor's annotations.
func getLayerFilePath(desc ocispec.Descriptor) string {
	if desc.Annotations == nil {
		return ""
	}
	// Try the current model-spec annotation first.
	if fp := desc.Annotations[modelspec.AnnotationFilepath]; fp != "" {
		return fp
	}
	// Fall back to legacy annotation.
	if fp := desc.Annotations[oldModelspec.AnnotationFilepath]; fp != "" {
		return fp
	}
	return ""
}
