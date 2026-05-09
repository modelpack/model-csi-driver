package cas

import (
	"context"
	"path/filepath"
	"sync"

	legacymodelspec "github.com/dragonflyoss/model-spec/specs-go/v1"
	modelspec "github.com/modelpack/model-spec/specs-go/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// PullHooks mirrors modctl.config.PullHooks. Duplicated here to avoid
// importing modctl from this package.
type PullHooks interface {
	BeforePullLayer(desc ocispec.Descriptor, manifest ocispec.Manifest) (skip bool)
	AfterPullLayer(desc ocispec.Descriptor, skipped bool, err error)
}

// PullHook integrates the CAS with modctl's per-layer pull lifecycle:
// Before claims a ref (and short-circuits the download on cache hit), After
// imports the freshly extracted file. Layers without an AnnotationFilepath
// (e.g. tar layers) are passed through unchanged.
type PullHook struct {
	inner      PullHooks
	store      *Store
	ownerKey   string
	extractDir string
	ctx        context.Context

	mu  sync.Mutex
	dst map[string]string // digest -> resolved destination path
}

// NewPullHook wraps inner with CAS integration. ownerKey identifies the
// volume/mount; extractDir is modctl's output directory.
func NewPullHook(ctx context.Context, store *Store, inner PullHooks, ownerKey, extractDir string) *PullHook {
	if inner == nil {
		inner = noopHooks{}
	}
	return &PullHook{
		inner:      inner,
		store:      store,
		ownerKey:   ownerKey,
		extractDir: extractDir,
		ctx:        ctx,
		dst:        make(map[string]string),
	}
}

// BeforePullLayer claims a CAS ref and returns skip=true on cache hit.
func (h *PullHook) BeforePullLayer(desc ocispec.Descriptor, manifest ocispec.Manifest) bool {
	// Always invoke inner so progress / tracing is preserved.
	innerSkip := h.inner.BeforePullLayer(desc, manifest)

	if h.store == nil {
		return innerSkip
	}
	dst := h.layerDestination(desc)
	if dst == "" {
		return innerSkip
	}
	h.mu.Lock()
	h.dst[desc.Digest.String()] = dst
	h.mu.Unlock()

	hit, err := h.store.EnsureLink(h.ctx, h.ownerKey, desc.Digest.String(), dst)
	if err != nil {
		// CAS error: degrade to a normal pull.
		return innerSkip
	}
	if hit {
		return true
	}
	// We're the leader. If modctl is going to skip the download anyway,
	// AfterPullLayer fires with skipped=true and won't wake followers, so
	// release the slot now.
	if innerSkip {
		h.store.AbortDownload(desc.Digest.String())
		return true
	}
	return false
}

// AfterPullLayer imports the extract on success, or aborts the in-flight
// slot on failure so a follower can retry.
func (h *PullHook) AfterPullLayer(desc ocispec.Descriptor, skipped bool, err error) {
	defer h.inner.AfterPullLayer(desc, skipped, err)

	if h.store == nil || skipped {
		return
	}
	h.mu.Lock()
	dst, ok := h.dst[desc.Digest.String()]
	h.mu.Unlock()
	if !ok || dst == "" {
		return
	}
	if err != nil {
		h.store.AbortDownload(desc.Digest.String())
		return
	}
	// Best-effort: an Import failure leaves the extracted file in place,
	// so the volume still works; we just miss the cache opportunity.
	// Import wakes followers internally on both paths.
	_ = h.store.Import(h.ctx, desc.Digest.String(), dst)
}

// layerDestination is the absolute path modctl will write this raw layer to,
// or "" if the layer carries no filepath annotation (tar layer / passthrough).
func (h *PullHook) layerDestination(desc ocispec.Descriptor) string {
	if h.extractDir == "" {
		return ""
	}
	rel := layerFilepath(desc)
	if rel == "" {
		return ""
	}
	return filepath.Join(h.extractDir, rel)
}

func layerFilepath(desc ocispec.Descriptor) string {
	if desc.Annotations == nil {
		return ""
	}
	if v := desc.Annotations[modelspec.AnnotationFilepath]; v != "" {
		return v
	}
	return desc.Annotations[legacymodelspec.AnnotationFilepath]
}

type noopHooks struct{}

func (noopHooks) BeforePullLayer(ocispec.Descriptor, ocispec.Manifest) bool { return false }
func (noopHooks) AfterPullLayer(ocispec.Descriptor, bool, error)            {}
