package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelpack/model-csi-driver/pkg/status"
	modelspec "github.com/modelpack/model-spec/specs-go/v1"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

func makeDesc(t *testing.T, dgst digest.Digest, file string) ocispec.Descriptor {
	t.Helper()
	return ocispec.Descriptor{
		Digest:    dgst,
		MediaType: "application/octet-stream",
		Size:      int64(len(file)),
		Annotations: map[string]string{
			modelspec.AnnotationFilepath: file,
		},
	}
}

func TestCombinedHook_NilLayerCache_NoSkip(t *testing.T) {
	statusHook := status.NewHook(context.Background())
	h := &combinedHook{status: statusHook, lc: nil}
	skip := h.BeforePullLayer(makeDesc(t, digest.FromString("a"), "a.bin"), ocispec.Manifest{})
	require.False(t, skip)
	h.AfterPullLayer(makeDesc(t, digest.FromString("a"), "a.bin"), false, nil)
}

func TestCombinedHook_NoFilepath_NoSkip(t *testing.T) {
	c, _ := newTestCache(t)
	statusHook := status.NewHook(context.Background())
	h := &combinedHook{status: statusHook, lc: newLayerCacheHook(context.Background(), c, "/tmp")}

	desc := ocispec.Descriptor{Digest: digest.FromString("nofp"), MediaType: "x"}
	require.False(t, h.BeforePullLayer(desc, ocispec.Manifest{}))
	h.AfterPullLayer(desc, false, nil)
}

func TestCombinedHook_HitPath_PullerOwnsThenAnotherSkips(t *testing.T) {
	c, tmp := newTestCache(t)

	dir1 := filepath.Join(tmp, "volumes/pvc-1/model")
	dir2 := filepath.Join(tmp, "volumes/pvc-2/model")
	desc := makeDesc(t, digest.FromString("hit"), "f.bin")

	// Puller A: own + write + publish.
	hookA := &combinedHook{status: status.NewHook(context.Background()), lc: newLayerCacheHook(context.Background(), c, dir1)}
	skipA := hookA.BeforePullLayer(desc, ocispec.Manifest{})
	require.False(t, skipA)
	writeFile(t, filepath.Join(dir1, "f.bin"), []byte("data"))
	hookA.AfterPullLayer(desc, false, nil)

	// Puller B: target in different dir; should skip and have file
	// hardlinked already.
	hookB := &combinedHook{status: status.NewHook(context.Background()), lc: newLayerCacheHook(context.Background(), c, dir2)}
	skipB := hookB.BeforePullLayer(desc, ocispec.Manifest{})
	require.True(t, skipB)
	_, err := os.Stat(filepath.Join(dir2, "f.bin"))
	require.NoError(t, err)
	hookB.AfterPullLayer(desc, true, nil)

	c.FlushPersist()

	// Both volumes must persist their own layers.json so dedup survives
	// owner-volume deletion and daemonset restarts.
	_, err = os.Stat(filepath.Join(tmp, "volumes/pvc-1", LayersFileName))
	require.NoError(t, err, "owner volume must have layers.json")
	_, err = os.Stat(filepath.Join(tmp, "volumes/pvc-2", LayersFileName))
	require.NoError(t, err, "hit volume must also have layers.json")

	// The cache must know about both paths.
	c.mu.Lock()
	entry := c.items[desc.Digest]
	c.mu.Unlock()
	entry.mu.Lock()
	defer entry.mu.Unlock()
	require.ElementsMatch(t,
		[]string{filepath.Join(dir1, "f.bin"), filepath.Join(dir2, "f.bin")},
		entry.paths,
	)
}

// Deleting the owner volume must not lose dedup state, because the hit
// volume registered itself in the cache.
func TestCombinedHook_DedupSurvivesOwnerDeletion(t *testing.T) {
	c, tmp := newTestCache(t)

	dir1 := filepath.Join(tmp, "volumes/pvc-1/model")
	dir2 := filepath.Join(tmp, "volumes/pvc-2/model")
	dir3 := filepath.Join(tmp, "volumes/pvc-3/model")
	desc := makeDesc(t, digest.FromString("survive"), "f.bin")

	hookA := &combinedHook{status: status.NewHook(context.Background()), lc: newLayerCacheHook(context.Background(), c, dir1)}
	require.False(t, hookA.BeforePullLayer(desc, ocispec.Manifest{}))
	writeFile(t, filepath.Join(dir1, "f.bin"), []byte("data"))
	hookA.AfterPullLayer(desc, false, nil)

	hookB := &combinedHook{status: status.NewHook(context.Background()), lc: newLayerCacheHook(context.Background(), c, dir2)}
	require.True(t, hookB.BeforePullLayer(desc, ocispec.Manifest{}))
	hookB.AfterPullLayer(desc, true, nil)

	// Simulate owner-volume removal: clear A's path from the cache and
	// delete A's file. B's hardlink (independent inode reference) and
	// cache registration must keep dedup alive.
	c.OnVolumeRemoved(filepath.Join(tmp, "volumes/pvc-1"))
	require.NoError(t, os.Remove(filepath.Join(dir1, "f.bin")))

	hookC := &combinedHook{status: status.NewHook(context.Background()), lc: newLayerCacheHook(context.Background(), c, dir3)}
	skipC := hookC.BeforePullLayer(desc, ocispec.Manifest{})
	require.True(t, skipC, "C should still hit via B's path")
	_, err := os.Stat(filepath.Join(dir3, "f.bin"))
	require.NoError(t, err)
	hookC.AfterPullLayer(desc, true, nil)
}

func TestCombinedHook_OwnerFailureClearsOwnership(t *testing.T) {
	c, tmp := newTestCache(t)
	dir := filepath.Join(tmp, "volumes/pvc-x/model")
	desc := makeDesc(t, digest.FromString("fail"), "f.bin")

	h := &combinedHook{status: status.NewHook(context.Background()), lc: newLayerCacheHook(context.Background(), c, dir)}
	require.False(t, h.BeforePullLayer(desc, ocispec.Manifest{}))
	h.AfterPullLayer(desc, false, os.ErrPermission)

	// Entry should have been Fail'd; another puller can now become owner.
	hookB := &combinedHook{status: status.NewHook(context.Background()), lc: newLayerCacheHook(context.Background(), c, dir)}
	require.False(t, hookB.BeforePullLayer(desc, ocispec.Manifest{}))
}

func TestLayerCacheHook_TakeOwnedReturnsEmptyForUntracked(t *testing.T) {
	c, _ := newTestCache(t)
	h := newLayerCacheHook(context.Background(), c, "/tmp")
	require.Equal(t, "", h.takeOwned(digest.FromString("untracked")))
}

// Covers BeforePullLayer's Acquire-error branch: when Acquire returns an
// error (e.g. ctx cancelled while waiting), the hook must still markOwned
// the target and return false so the caller falls back to pulling.
func TestCombinedHook_AcquireError_FallsBackAndOwns(t *testing.T) {
	c, tmp := newTestCache(t)
	dir := filepath.Join(tmp, "volumes/pvc-a/model")
	desc := makeDesc(t, digest.FromString("acq-err"), "f.bin")

	// First hook becomes the pulling owner so the second one will wait.
	hookA := &combinedHook{lc: newLayerCacheHook(context.Background(), c, dir)}
	require.False(t, hookA.BeforePullLayer(desc, ocispec.Manifest{}))

	// Second hook uses an already-cancelled context: Acquire will return
	// ctx.Err() immediately on entering the wait loop.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir2 := filepath.Join(tmp, "volumes/pvc-b/model")
	hookB := &combinedHook{lc: newLayerCacheHook(ctx, c, dir2)}
	skip := hookB.BeforePullLayer(desc, ocispec.Manifest{})
	require.False(t, skip)

	// markOwned must have stored the target.
	hookB.lc.mu.Lock()
	got := hookB.lc.owned[desc.Digest]
	hookB.lc.mu.Unlock()
	require.Equal(t, filepath.Join(dir2, "f.bin"), got)
}

// Covers targetForDesc when annotations are present but neither the
// modern nor the legacy filepath annotation has a value.
func TestLayerCacheHook_TargetForDesc_AnnotationsWithoutFilepath(t *testing.T) {
	c, _ := newTestCache(t)
	h := newLayerCacheHook(context.Background(), c, "/tmp")
	desc := ocispec.Descriptor{
		Digest:      digest.FromString("no-fp"),
		Annotations: map[string]string{"other": "x"},
	}
	require.Equal(t, "", h.targetForDesc(desc))
}

func TestLayerCacheHook_TargetForDesc_LegacyAnnotation(t *testing.T) {
	c, _ := newTestCache(t)
	h := newLayerCacheHook(context.Background(), c, "/tmp")
	desc := ocispec.Descriptor{
		Digest: digest.FromString("legacy"),
		Annotations: map[string]string{
			"org.cnai.model.filepath": "weights/x.bin",
		},
	}
	got := h.targetForDesc(desc)
	require.Equal(t, "/tmp/weights/x.bin", got)
}

func TestLayerCacheHook_TargetForDesc_RejectsPathTraversal(t *testing.T) {
	c, tmp := newTestCache(t)
	targetDir := filepath.Join(tmp, "volumes/pvc-a/model")
	h := newLayerCacheHook(context.Background(), c, targetDir)
	desc := makeDesc(t, digest.FromString("traversal"), "../escape.bin")

	require.Equal(t, "", h.targetForDesc(desc))
}
