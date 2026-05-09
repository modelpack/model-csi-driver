package cas

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	legacymodelspec "github.com/dragonflyoss/model-spec/specs-go/v1"
	modelspec "github.com/modelpack/model-spec/specs-go/v1"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

// recordingInner is an in-memory PullHooks implementation that records every
// call so tests can assert that wrapping preserved the inner contract.
type recordingInner struct {
	mu      sync.Mutex
	beforeN int32
	afterN  int32
	skipped []bool
	errs    []error
	skip    bool // skip flag returned by BeforePullLayer
}

func (r *recordingInner) BeforePullLayer(ocispec.Descriptor, ocispec.Manifest) bool {
	atomic.AddInt32(&r.beforeN, 1)
	return r.skip
}
func (r *recordingInner) AfterPullLayer(_ ocispec.Descriptor, skipped bool, err error) {
	r.mu.Lock()
	r.skipped = append(r.skipped, skipped)
	r.errs = append(r.errs, err)
	r.mu.Unlock()
	atomic.AddInt32(&r.afterN, 1)
}

func rawDescriptor(dg, fp string) ocispec.Descriptor {
	return ocispec.Descriptor{
		Digest:    digest.Digest(dg),
		Size:      int64(len(dg)),
		MediaType: "application/vnd.cnai.model.weight.raw",
		Annotations: map[string]string{
			modelspec.AnnotationFilepath: fp,
		},
	}
}

func TestLayerFilepath_LegacyAndNew(t *testing.T) {
	require.Equal(t, "foo", layerFilepath(ocispec.Descriptor{
		Annotations: map[string]string{modelspec.AnnotationFilepath: "foo"},
	}))
	require.Equal(t, "bar", layerFilepath(ocispec.Descriptor{
		Annotations: map[string]string{legacymodelspec.AnnotationFilepath: "bar"},
	}))
	require.Equal(t, "", layerFilepath(ocispec.Descriptor{}))
}

func TestNoopHooks(t *testing.T) {
	h := noopHooks{}
	require.False(t, h.BeforePullLayer(ocispec.Descriptor{}, ocispec.Manifest{}))
	h.AfterPullLayer(ocispec.Descriptor{}, false, nil) // must not panic
}

// On a cold cache, BeforePullLayer should return false (no skip) and the
// inner hook should still be invoked. After the caller "downloads" the
// file, AfterPullLayer should import it into the CAS.
func TestPullHook_MissThenImport(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	require.NoError(t, err)

	extractDir := t.TempDir()
	inner := &recordingInner{}
	h := NewPullHook(context.Background(), store, inner, "ownerA", extractDir)

	desc := rawDescriptor("sha256:dead", "weights/model.bin")
	skip := h.BeforePullLayer(desc, ocispec.Manifest{})
	require.False(t, skip)
	require.Equal(t, int32(1), atomic.LoadInt32(&inner.beforeN))

	// "Download" by writing to the destination path.
	dst := filepath.Join(extractDir, "weights/model.bin")
	writeFile(t, dst, "BLOB")

	h.AfterPullLayer(desc, false, nil)

	require.Equal(t, int32(1), atomic.LoadInt32(&inner.afterN))
	require.True(t, store.Has(desc.Digest.String()))
	// dst should now be a hardlink to the canonical blob.
	stA, _ := os.Stat(dst)
	stB, _ := os.Stat(store.blobPath(desc.Digest.String()))
	require.True(t, os.SameFile(stA, stB))
}

// On a warm cache, BeforePullLayer should hardlink the file in place and
// return skip=true to short-circuit modctl.
func TestPullHook_HitSkips(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	require.NoError(t, err)

	// Seed the blob via owner A.
	extractA := t.TempDir()
	hA := NewPullHook(context.Background(), store, &recordingInner{}, "ownerA", extractA)
	desc := rawDescriptor("sha256:dead", "weights/model.bin")
	require.False(t, hA.BeforePullLayer(desc, ocispec.Manifest{}))
	writeFile(t, filepath.Join(extractA, "weights/model.bin"), "BLOB")
	hA.AfterPullLayer(desc, false, nil)
	require.True(t, store.Has(desc.Digest.String()))

	// Owner B: cache hit.
	extractB := t.TempDir()
	innerB := &recordingInner{}
	hB := NewPullHook(context.Background(), store, innerB, "ownerB", extractB)
	skip := hB.BeforePullLayer(desc, ocispec.Manifest{})
	require.True(t, skip)
	require.Equal(t, int32(1), atomic.LoadInt32(&innerB.beforeN))

	// File should already exist as a hardlink to the canonical blob.
	dst := filepath.Join(extractB, "weights/model.bin")
	stB, err := os.Stat(dst)
	require.NoError(t, err)
	stCanonical, _ := os.Stat(store.blobPath(desc.Digest.String()))
	require.True(t, os.SameFile(stB, stCanonical))

	// AfterPullLayer with skipped=true must NOT re-import.
	hB.AfterPullLayer(desc, true, nil)
	require.Equal(t, int32(1), atomic.LoadInt32(&innerB.afterN))

	// Refcount = 2.
	c, err := store.RefCount(desc.Digest.String())
	require.NoError(t, err)
	require.Equal(t, 2, c)
}

// Layers without a filepath annotation (e.g. tarballs) are passthrough:
// hook does not consult the CAS regardless of media type.
func TestPullHook_TarLikeLayerIsPassthrough(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	require.NoError(t, err)

	inner := &recordingInner{skip: false}
	h := NewPullHook(context.Background(), store, inner, "owner", t.TempDir())

	// No AnnotationFilepath -> layerDestination returns "" -> passthrough.
	desc := ocispec.Descriptor{
		Digest:    digest.Digest("sha256:tar"),
		MediaType: "application/x.foo.tar",
	}
	require.False(t, h.BeforePullLayer(desc, ocispec.Manifest{}))
	h.AfterPullLayer(desc, false, nil)

	// CAS must remain untouched.
	require.False(t, store.Has(desc.Digest.String()))
	c, _ := store.RefCount(desc.Digest.String())
	require.Equal(t, 0, c)
}

// When the inner hook requests skip and the layer carries no filepath
// annotation, the wrapper must still propagate the skip verdict.
func TestPullHook_InnerSkipPropagated(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	require.NoError(t, err)

	inner := &recordingInner{skip: true}
	h := NewPullHook(context.Background(), store, inner, "owner", t.TempDir())

	desc := ocispec.Descriptor{
		Digest:    digest.Digest("sha256:tar"),
		MediaType: "application/x.foo.tar",
	}
	require.True(t, h.BeforePullLayer(desc, ocispec.Manifest{}))
}

// AfterPullLayer with err != nil must NOT import.
func TestPullHook_AfterError_NoImport(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	require.NoError(t, err)

	extractDir := t.TempDir()
	inner := &recordingInner{}
	h := NewPullHook(context.Background(), store, inner, "owner", extractDir)
	desc := rawDescriptor("sha256:e", "f.bin")

	require.False(t, h.BeforePullLayer(desc, ocispec.Manifest{}))
	writeFile(t, filepath.Join(extractDir, "f.bin"), "x")

	h.AfterPullLayer(desc, false, errors.New("download failed"))
	require.False(t, store.Has(desc.Digest.String()))
}

// A descriptor without a filepath annotation is unmanageable; the hook is a
// passthrough and does not touch the CAS, even for raw media types.
func TestPullHook_MissingFilepathAnnotation(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	require.NoError(t, err)

	inner := &recordingInner{}
	h := NewPullHook(context.Background(), store, inner, "owner", t.TempDir())

	desc := ocispec.Descriptor{
		Digest:    digest.Digest("sha256:nofp"),
		MediaType: "application/x.foo.raw",
	}
	require.False(t, h.BeforePullLayer(desc, ocispec.Manifest{}))
	h.AfterPullLayer(desc, false, nil)
	require.False(t, store.Has(desc.Digest.String()))
}

// nil store disables the integration entirely.
func TestPullHook_NilStorePassthrough(t *testing.T) {
	inner := &recordingInner{skip: true}
	h := NewPullHook(context.Background(), nil, inner, "owner", t.TempDir())

	desc := rawDescriptor("sha256:x", "f.bin")
	require.True(t, h.BeforePullLayer(desc, ocispec.Manifest{}))
	h.AfterPullLayer(desc, true, nil)
}

// nil inner is replaced by a noop so calls don't panic.
func TestPullHook_NilInner(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	require.NoError(t, err)

	h := NewPullHook(context.Background(), store, nil, "owner", t.TempDir())
	desc := rawDescriptor("sha256:x", "f.bin")
	require.False(t, h.BeforePullLayer(desc, ocispec.Manifest{}))
}

// EnsureLink errors must not break the pull. The wrapper falls back to the
// inner hook's verdict.
func TestPullHook_EnsureLinkError_FallsBackToInner(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	require.NoError(t, err)

	// Force EnsureLink to fail by pre-creating the refs path as a regular file.
	desc := rawDescriptor("sha256:bad", "f.bin")
	writeFile(t, store.refsDir(desc.Digest.String()), "x")

	inner := &recordingInner{skip: false}
	h := NewPullHook(context.Background(), store, inner, "owner", t.TempDir())
	require.False(t, h.BeforePullLayer(desc, ocispec.Manifest{}))
}

// layerDestination returns "" when extractDir is empty.
func TestPullHook_EmptyExtractDir(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	require.NoError(t, err)

	inner := &recordingInner{}
	h := NewPullHook(context.Background(), store, inner, "owner", "")
	desc := rawDescriptor("sha256:x", "f.bin")
	require.False(t, h.BeforePullLayer(desc, ocispec.Manifest{}))
	require.False(t, store.Has(desc.Digest.String()))
}

// When inner asks to skip and we are elected leader, the wrapper must
// abort the in-flight slot so followers don't deadlock waiting for an
// import that will never happen.
func TestPullHook_LeaderInnerSkip_AbortsSlot(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	require.NoError(t, err)

	inner := &recordingInner{skip: true}
	h := NewPullHook(context.Background(), store, inner, "leader", t.TempDir())
	desc := rawDescriptor("sha256:abc", "f.bin")
	require.True(t, h.BeforePullLayer(desc, ocispec.Manifest{}))

	// A follower should not block: the slot was released by AbortDownload.
	done := make(chan struct{})
	go func() {
		_, err := store.EnsureLink(context.Background(), "follower", desc.Digest.String(), "")
		require.NoError(t, err)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("follower blocked despite leader's inner-skip")
	}
}
