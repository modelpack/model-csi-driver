package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

func newTestStore(t *testing.T) (*ModelStore, string) {
	t.Helper()
	root := t.TempDir()
	return NewModelStore(root), root
}

func writePayload(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		full := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0755))
		require.NoError(t, os.WriteFile(full, []byte(body), 0644))
	}
}

func samplePullFn(t *testing.T) func(context.Context, string) error {
	t.Helper()
	return func(_ context.Context, dst string) error {
		writePayload(t, dst, map[string]string{
			"weights.bin":      "weights",
			"sub/config.json":  "config",
			"sub/deep/leaf.md": "leaf",
		})
		return os.Symlink("weights.bin", filepath.Join(dst, "alias"))
	}
}

const (
	testDigest       = "sha256:abc"
	testDigestAlt    = "sha256:def"
	testDigestShared = "sha256:shared"
)

// ─── splitDigest ───────────────────────────────────────────────────────────────────

func TestSplitDigest(t *testing.T) {
	algo, hex, err := splitDigest("sha256:abc")
	require.NoError(t, err)
	require.Equal(t, "sha256", algo)
	require.Equal(t, "abc", hex)
}

func TestSplitDigest_Invalid(t *testing.T) {
	cases := []string{
		"",
		"plain",
		":abc",
		"sha256:",
		"sha/256:abc",
		"sha256:ab/c",
	}
	for _, c := range cases {
		_, _, err := splitDigest(c)
		require.Errorf(t, err, "expected error for %q", c)
	}
}

// ─── Materialize happy paths ──────────────────────────────────────────────────

func TestModelStore_Materialize_FirstTime(t *testing.T) {
	store, root := newTestStore(t)
	dst := filepath.Join(t.TempDir(), "model")

	called := 0
	err := store.Materialize(context.Background(), testDigest, dst, func(_ context.Context, p string) error {
		called++
		return samplePullFn(t)(context.Background(), p)
	})
	require.NoError(t, err)
	require.Equal(t, 1, called)

	// data/ exists, data.tmp/ is gone.
	storeDir := filepath.Join(root, "store", "sha256", "abc")
	require.DirExists(t, filepath.Join(storeDir, "data"))
	_, statErr := os.Stat(filepath.Join(storeDir, "data.tmp"))
	require.True(t, os.IsNotExist(statErr))

	// Files hardlinked into dst.
	body, err := os.ReadFile(filepath.Join(dst, "weights.bin"))
	require.NoError(t, err)
	require.Equal(t, "weights", string(body))
	body, err = os.ReadFile(filepath.Join(dst, "sub", "config.json"))
	require.NoError(t, err)
	require.Equal(t, "config", string(body))

	// nlink >= 2 because data/ also references the file.
	srcStat, err := os.Stat(filepath.Join(storeDir, "data", "weights.bin"))
	require.NoError(t, err)
	dstStat, err := os.Stat(filepath.Join(dst, "weights.bin"))
	require.NoError(t, err)
	require.True(t, os.SameFile(srcStat, dstStat))

	// Symlink preserved (not hardlinked).
	link, err := os.Readlink(filepath.Join(dst, "alias"))
	require.NoError(t, err)
	require.Equal(t, "weights.bin", link)
}

func TestModelStore_Materialize_ReusesCachedData(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	pulls := 0
	pullFn := func(_ context.Context, dst string) error {
		pulls++
		return samplePullFn(t)(ctx, dst)
	}

	dst1 := filepath.Join(t.TempDir(), "v1")
	require.NoError(t, store.Materialize(ctx, testDigest, dst1, pullFn))
	dst2 := filepath.Join(t.TempDir(), "v2")
	require.NoError(t, store.Materialize(ctx, testDigest, dst2, pullFn))

	require.Equal(t, 1, pulls, "second call must not invoke pullFn")

	// Both dst trees see the same inodes.
	a, err := os.Stat(filepath.Join(dst1, "weights.bin"))
	require.NoError(t, err)
	b, err := os.Stat(filepath.Join(dst2, "weights.bin"))
	require.NoError(t, err)
	require.True(t, os.SameFile(a, b))
}

// ─── Materialize error paths ──────────────────────────────────────────────

func TestModelStore_Materialize_InvalidDigest(t *testing.T) {
	store, _ := newTestStore(t)
	require.Error(t, store.Materialize(context.Background(), "", t.TempDir(),
		func(context.Context, string) error { return nil }))
	require.Error(t, store.Materialize(context.Background(), "plain", t.TempDir(),
		func(context.Context, string) error { return nil }))
}

func TestModelStore_Materialize_EmptyDst(t *testing.T) {
	store, _ := newTestStore(t)
	require.Error(t, store.Materialize(context.Background(), testDigest, "",
		func(context.Context, string) error { return nil }))
}

func TestModelStore_Materialize_PullFnError(t *testing.T) {
	store, root := newTestStore(t)
	wantErr := errors.New("network down")

	err := store.Materialize(context.Background(), testDigest, filepath.Join(t.TempDir(), "out"),
		func(context.Context, string) error { return wantErr })
	require.ErrorIs(t, err, wantErr)

	storeDir := filepath.Join(root, "store", "sha256", "abc")
	_, statErr := os.Stat(filepath.Join(storeDir, "data"))
	require.True(t, os.IsNotExist(statErr))
	_, statErr = os.Stat(filepath.Join(storeDir, "data.tmp"))
	require.True(t, os.IsNotExist(statErr))
}

func TestModelStore_Materialize_ContextCanceled(t *testing.T) {
	store, _ := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, store.Materialize(ctx, testDigest, t.TempDir(),
		func(context.Context, string) error { return nil }))
}

func TestModelStore_Materialize_ResumesAfterPriorTmpCrash(t *testing.T) {
	store, root := newTestStore(t)
	storeDir := filepath.Join(root, "store", "sha256", "abc")
	staleTmp := filepath.Join(storeDir, "data.tmp")
	require.NoError(t, os.MkdirAll(staleTmp, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(staleTmp, "garbage"), []byte("g"), 0644))

	dst := filepath.Join(t.TempDir(), "out")
	require.NoError(t, store.Materialize(context.Background(), testDigest, dst, samplePullFn(t)))

	require.DirExists(t, filepath.Join(storeDir, "data"))
	_, statErr := os.Stat(filepath.Join(storeDir, "data.tmp"))
	require.True(t, os.IsNotExist(statErr))
}

func TestModelStore_Materialize_LinkTreeError_HardlinkConflict(t *testing.T) {
	store, _ := newTestStore(t)
	dst := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dst, "weights.bin"), []byte("old"), 0644))

	err := store.Materialize(context.Background(), testDigest, dst, samplePullFn(t))
	require.Error(t, err)
}

func TestModelStore_Materialize_DstBlockedByFile(t *testing.T) {
	store, _ := newTestStore(t)
	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0644))

	err := store.Materialize(context.Background(), testDigest, filepath.Join(blocker, "sub"), samplePullFn(t))
	require.Error(t, err)
}

// ─── MaybeGC ───────────────────────────────────────────────────────────────────

func TestModelStore_MaybeGC_NoData(t *testing.T) {
	store, root := newTestStore(t)
	require.NoError(t, store.MaybeGC(context.Background(), testDigestAlt))

	storeDir := filepath.Join(root, "store", "sha256", "abc")
	require.NoError(t, os.MkdirAll(storeDir, 0755))
	require.NoError(t, store.MaybeGC(context.Background(), testDigest))
	_, err := os.Stat(storeDir)
	require.True(t, os.IsNotExist(err))
}

func TestModelStore_MaybeGC_NlinkOne_Reclaims(t *testing.T) {
	store, root := newTestStore(t)
	dst := filepath.Join(t.TempDir(), "out")
	require.NoError(t, store.Materialize(context.Background(), testDigest, dst, samplePullFn(t)))

	require.NoError(t, os.RemoveAll(dst))

	require.NoError(t, store.MaybeGC(context.Background(), testDigest))
	_, err := os.Stat(filepath.Join(root, "store", "sha256", "abc"))
	require.True(t, os.IsNotExist(err), "store dir should be reclaimed")
}

func TestModelStore_MaybeGC_NlinkAboveOne_Keeps(t *testing.T) {
	store, root := newTestStore(t)
	dst := filepath.Join(t.TempDir(), "out")
	require.NoError(t, store.Materialize(context.Background(), testDigest, dst, samplePullFn(t)))

	require.NoError(t, store.MaybeGC(context.Background(), testDigest))
	require.DirExists(t, filepath.Join(root, "store", "sha256", "abc", "data"))
}

func TestModelStore_MaybeGC_InvalidDigest(t *testing.T) {
	store, _ := newTestStore(t)
	require.Error(t, store.MaybeGC(context.Background(), ""))
	require.Error(t, store.MaybeGC(context.Background(), "plain"))
}

func TestModelStore_MaybeGC_ContextCanceled(t *testing.T) {
	store, _ := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, store.MaybeGC(ctx, testDigest))
}

func TestModelStore_MaybeGC_NoRegularFiles_Reclaims(t *testing.T) {
	store, root := newTestStore(t)
	dataDir := filepath.Join(root, "store", "sha256", "abc", "data")
	require.NoError(t, os.MkdirAll(dataDir, 0755))
	require.NoError(t, os.Symlink("nowhere", filepath.Join(dataDir, "alias")))

	require.NoError(t, store.MaybeGC(context.Background(), testDigest))
	_, err := os.Stat(filepath.Join(root, "store", "sha256", "abc"))
	require.True(t, os.IsNotExist(err))
}

// ─── linkTree direct error coverage ───────────────────────────────────────────

func TestLinkTree_MissingSource(t *testing.T) {
	require.Error(t, linkTree(filepath.Join(t.TempDir(), "missing"), filepath.Join(t.TempDir(), "out")))
}

func TestLinkTree_DstBlockedByFile(t *testing.T) {
	parent := t.TempDir()
	dst := filepath.Join(parent, "blocker")
	require.NoError(t, os.WriteFile(dst, []byte("x"), 0644))

	src := t.TempDir()
	writePayload(t, src, map[string]string{"a": "1"})
	require.Error(t, linkTree(src, dst))
}

func TestLinkTree_SymlinkConflict(t *testing.T) {
	src := t.TempDir()
	writePayload(t, src, map[string]string{"target": "t"})
	require.NoError(t, os.Symlink("target", filepath.Join(src, "alias")))

	dst := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dst, "alias"), []byte("old"), 0644))
	require.Error(t, linkTree(src, dst))
}

// linkTree must propagate MkdirAll errors on subdirectories.
func TestLinkTree_SubdirMkdirFails(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, "nested"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "nested", "f"), []byte("x"), 0644))

	dst := t.TempDir()
	// Pre-place a regular file where the "nested" subdir should be created.
	require.NoError(t, os.WriteFile(filepath.Join(dst, "nested"), []byte("blocker"), 0644))

	require.Error(t, linkTree(src, dst))
}

func TestLinkTree_SymlinkSuccess(t *testing.T) {
	src := t.TempDir()
	writePayload(t, src, map[string]string{"target": "t"})
	require.NoError(t, os.Symlink("target", filepath.Join(src, "alias")))

	dst := t.TempDir()
	require.NoError(t, linkTree(src, dst))

	link, err := os.Readlink(filepath.Join(dst, "alias"))
	require.NoError(t, err)
	require.Equal(t, "target", link)
}

// ─── Concurrency ──────────────────────────────────────────────────────────────

func TestModelStore_Materialize_ConcurrentSerializes(t *testing.T) {
	store, root := newTestStore(t)
	ctx := context.Background()

	const callers = 16
	var pulls atomic.Int32
	pullFn := func(_ context.Context, dst string) error {
		pulls.Add(1)
		// Slow pull so callers pile up on keyMu.
		time.Sleep(50 * time.Millisecond)
		return samplePullFn(t)(ctx, dst)
	}

	dsts := make([]string, callers)
	parent := t.TempDir()
	for i := range dsts {
		dsts[i] = filepath.Join(parent, "v"+string(rune('a'+i)))
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			errs[i] = store.Materialize(ctx, testDigestShared, dsts[i], pullFn)
		}()
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, int32(1), pulls.Load(), "pullFn must run only once across concurrent callers")

	// Every dst observed the same payload.
	for _, d := range dsts {
		body, err := os.ReadFile(filepath.Join(d, "weights.bin"))
		require.NoError(t, err)
		require.Equal(t, "weights", string(body))
	}
	require.DirExists(t, filepath.Join(root, "store", "sha256", "shared", "data"))
}

func TestModelStore_Materialize_GC_RaceFree(t *testing.T) {
	// Interleave Materialize + RemoveAll(volume) + MaybeGC many times under
	// the same key to exercise the nlink-based contract.
	store, root := newTestStore(t)
	ctx := context.Background()

	const n = 32
	parent := t.TempDir()
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			dst := filepath.Join(parent, fmt.Sprintf("v-%d", i))
			if err := store.Materialize(ctx, testDigest, dst, samplePullFn(t)); err != nil {
				t.Errorf("Materialize: %v", err)
				return
			}
			_ = os.RemoveAll(dst)
			if err := store.MaybeGC(ctx, testDigest); err != nil {
				t.Errorf("MaybeGC: %v", err)
				return
			}
		}()
	}
	wg.Wait()

	// Final state: all volumes gone, store dir GC'd.
	finalGCErr := store.MaybeGC(ctx, testDigest)
	require.NoError(t, finalGCErr)
	_, err := os.Stat(filepath.Join(root, "store", "sha256", "abc"))
	require.True(t, os.IsNotExist(err))
}

// ─── hasLiveLinks direct ──────────────────────────────────────────────────────

func TestHasLiveLinks_MissingDir(t *testing.T) {
	_, err := hasLiveLinks(filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
}

func TestHasLiveLinks_NoRegularFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Symlink("nowhere", filepath.Join(dir, "alias")))
	live, err := hasLiveLinks(dir)
	require.NoError(t, err)
	require.False(t, live)
}

func TestHasLiveLinks_DetectsExternalHardlink(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	src := filepath.Join(a, "f")
	require.NoError(t, os.WriteFile(src, []byte("x"), 0644))
	require.NoError(t, os.Link(src, filepath.Join(b, "f")))

	live, err := hasLiveLinks(a)
	require.NoError(t, err)
	require.True(t, live)
}

// withFsHooks swaps fsRemoveAll/fsMkdirAll/fsRename for the test
// duration. Pass nil to keep the default.
func withFsHooks(t *testing.T, removeAll func(string) error, mkdirAll func(string, os.FileMode) error, rename func(string, string) error) {
	t.Helper()
	origRemoveAll, origMkdirAll, origRename := fsRemoveAll, fsMkdirAll, fsRename
	if removeAll != nil {
		fsRemoveAll = removeAll
	}
	if mkdirAll != nil {
		fsMkdirAll = mkdirAll
	}
	if rename != nil {
		fsRename = rename
	}
	t.Cleanup(func() {
		fsRemoveAll, fsMkdirAll, fsRename = origRemoveAll, origMkdirAll, origRename
	})
}

// ─── Error injection via fs hooks ────────────────────────────────────────────────

func TestModelStore_PullLocked_StagingMkdirError(t *testing.T) {
	store, _ := newTestStore(t)
	wantErr := errors.New("mkdir boom")
	withFsHooks(t, nil, func(string, os.FileMode) error { return wantErr }, nil)

	err := store.Materialize(context.Background(), testDigest, filepath.Join(t.TempDir(), "out"), samplePullFn(t))
	require.ErrorIs(t, err, wantErr)
}

func TestModelStore_PullLocked_RemoveStaleTmpError(t *testing.T) {
	store, _ := newTestStore(t)
	wantErr := errors.New("rm boom")
	withFsHooks(t, func(string) error { return wantErr }, nil, nil)

	err := store.Materialize(context.Background(), testDigest, filepath.Join(t.TempDir(), "out"), samplePullFn(t))
	require.ErrorIs(t, err, wantErr)
}

func TestModelStore_PullLocked_RenameError(t *testing.T) {
	store, _ := newTestStore(t)
	wantErr := errors.New("rename boom")
	withFsHooks(t, nil, nil, func(string, string) error { return wantErr })

	err := store.Materialize(context.Background(), testDigest, filepath.Join(t.TempDir(), "out"), samplePullFn(t))
	require.ErrorIs(t, err, wantErr)
}

func TestModelStore_MaybeGC_RemoveAllError_NoData(t *testing.T) {
	store, root := newTestStore(t)
	storeDir := filepath.Join(root, "store", "sha256", "abc")
	require.NoError(t, os.MkdirAll(storeDir, 0755))

	wantErr := errors.New("rm boom")
	withFsHooks(t, func(string) error { return wantErr }, nil, nil)

	require.ErrorIs(t, store.MaybeGC(context.Background(), testDigest), wantErr)
}

func TestModelStore_MaybeGC_RemoveAllError_AfterReclaim(t *testing.T) {
	store, root := newTestStore(t)
	dataDir := filepath.Join(root, "store", "sha256", "abc", "data")
	require.NoError(t, os.MkdirAll(dataDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "f"), []byte("x"), 0644)) // nlink=1

	wantErr := errors.New("rm boom")
	withFsHooks(t, func(string) error { return wantErr }, nil, nil)

	require.ErrorIs(t, store.MaybeGC(context.Background(), testDigest), wantErr)
}

func TestModelStore_MaybeGC_HasLiveLinksError(t *testing.T) {
	store, root := newTestStore(t)
	dataDir := filepath.Join(root, "store", "sha256", "abc", "data")
	require.NoError(t, os.MkdirAll(dataDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "f"), []byte("x"), 0644))

	wantErr := errors.New("walk fail")
	patch := gomonkey.ApplyFunc(filepath.WalkDir, func(string, fs.WalkDirFunc) error { return wantErr })
	defer patch.Reset()

	require.ErrorIs(t, store.MaybeGC(context.Background(), testDigest), wantErr)
}

func TestLinkTree_WalkPropagatesError(t *testing.T) {
	src := t.TempDir()
	writePayload(t, src, map[string]string{"a": "1"})

	wantErr := errors.New("info fail")
	patch := gomonkey.ApplyFunc(filepath.WalkDir, func(_ string, fn fs.WalkDirFunc) error {
		return fn("x", nil, wantErr)
	})
	defer patch.Reset()

	require.ErrorIs(t, linkTree(src, t.TempDir()), wantErr)
}

func TestHasLiveLinks_WalkPropagatesError(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0644))

	wantErr := errors.New("walk err")
	patch := gomonkey.ApplyFunc(filepath.WalkDir, func(_ string, fn fs.WalkDirFunc) error {
		return fn("x", nil, wantErr)
	})
	defer patch.Reset()

	_, err := hasLiveLinks(dir)
	require.ErrorIs(t, err, wantErr)
}
