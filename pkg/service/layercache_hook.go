package service

import (
	"context"
	"path/filepath"
	"strings"
	"sync"

	oldModelspec "github.com/dragonflyoss/model-spec/specs-go/v1"
	modctlConfig "github.com/modelpack/modctl/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/modelpack/model-csi-driver/pkg/status"
	modelspec "github.com/modelpack/model-spec/specs-go/v1"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// combinedHook composes the progress-tracking status.Hook with an optional
// layerCacheHook. The layerCacheHook controls whether the layer is skipped;
// status.Hook only observes events.
type combinedHook struct {
	status *status.Hook
	lc     *layerCacheHook
}

var _ modctlConfig.PullHooks = (*combinedHook)(nil)

func (h *combinedHook) BeforePullLayer(desc ocispec.Descriptor, manifest ocispec.Manifest) bool {
	if h.status != nil {
		// Always record the layer in progress so that "skipped" events still
		// surface in the status report.
		h.status.BeforePullLayer(desc, manifest)
	}
	if h.lc == nil {
		return false
	}
	return h.lc.BeforePullLayer(desc, manifest)
}

func (h *combinedHook) AfterPullLayer(desc ocispec.Descriptor, skipped bool, err error) {
	if h.lc != nil {
		h.lc.AfterPullLayer(desc, skipped, err)
	}
	if h.status != nil {
		h.status.AfterPullLayer(desc, skipped, err)
	}
}

// layerCacheHook bridges the modctl PullHooks interface to LayerCache. It
// is constructed per pull operation because the hardlink target depends on
// the per-pull output directory.
type layerCacheHook struct {
	ctx       context.Context
	cache     *LayerCache
	targetDir string

	mu    sync.Mutex
	owned map[digest.Digest]string // digests this puller is responsible for publishing
}

func newLayerCacheHook(ctx context.Context, cache *LayerCache, targetDir string) *layerCacheHook {
	return &layerCacheHook{
		ctx:       ctx,
		cache:     cache,
		targetDir: targetDir,
		owned:     make(map[digest.Digest]string),
	}
}

func (h *layerCacheHook) BeforePullLayer(desc ocispec.Descriptor, _ ocispec.Manifest) bool {
	target := h.targetForDesc(desc)
	if target == "" {
		// Layer has no filepath annotation (e.g. config blobs); cannot
		// hardlink, so let modctl handle it normally.
		return false
	}

	action, err := h.cache.Acquire(h.ctx, desc.Digest, target)
	if err != nil {
		logger.WithContext(h.ctx).WithError(err).Debugf(
			"layer cache acquire failed for %s, falling back to pull", desc.Digest,
		)
		h.markOwned(desc.Digest, target)
		return false
	}
	// Track the target regardless of hit/pull so AfterPullLayer can record
	// it in this volume's layers.json. Without this, a volume that fully
	// hits the cache would never write its own layers.json, which would
	// (a) make Rebuild miss its paths after a restart and (b) cause cache
	// state loss when the original owner volume is deleted.
	h.markOwned(desc.Digest, target)
	return action == ActionHit
}

func (h *layerCacheHook) AfterPullLayer(desc ocispec.Descriptor, skipped bool, err error) {
	target := h.takeOwned(desc.Digest)
	if target == "" {
		// We weren't tracking this layer (e.g. no filepath annotation).
		return
	}
	if skipped {
		// Cache hit: the file was hardlinked into target by Acquire. Register
		// the path so this volume contributes to the index and persists its
		// own layers.json reflecting what is actually on disk.
		logger.WithContext(h.ctx).Debugf("layer %s served from cache (reused)", desc.Digest)
		h.cache.Publish(desc.Digest, target)
		return
	}
	if err != nil {
		h.cache.Fail(desc.Digest)
		return
	}
	logger.WithContext(h.ctx).Debugf("layer %s pulled from remote", desc.Digest)
	h.cache.Publish(desc.Digest, target)
}

// targetForDesc derives the absolute on-disk path that modctl will produce
// for desc. Returns empty string when no filepath annotation is available.
func (h *layerCacheHook) targetForDesc(desc ocispec.Descriptor) string {
	if desc.Annotations == nil {
		return ""
	}
	rel := desc.Annotations[modelspec.AnnotationFilepath]
	if rel == "" {
		rel = desc.Annotations[oldModelspec.AnnotationFilepath]
	}
	if rel == "" {
		return ""
	}
	joined := filepath.Join(h.targetDir, rel)
	// Resolve to absolute and verify it stays within targetDir to prevent path traversal.
	absTarget, err := filepath.Abs(joined)
	if err != nil {
		return ""
	}
	absBase, err := filepath.Abs(h.targetDir)
	if err != nil {
		return ""
	}
	// Ensure the resolved path is under the target directory.
	if !strings.HasPrefix(absTarget, absBase+string(filepath.Separator)) && absTarget != absBase {
		return ""
	}
	return absTarget
}

func (h *layerCacheHook) markOwned(d digest.Digest, target string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.owned[d] = target
}

func (h *layerCacheHook) takeOwned(d digest.Digest) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.owned[d]
	delete(h.owned, d)
	return t
}
