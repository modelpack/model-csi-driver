package service

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/status"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
)

// fakePuller is a test double for Puller that records Pull calls and lets
// tests simulate delays / failures without hitting a real registry.
type fakePuller struct {
	pullFunc func(ctx context.Context, reference, targetDir string) error
}

func (f *fakePuller) Pull(ctx context.Context, reference, targetDir string, excludeModelWeights bool, excludeFilePatterns []string) error {
	return f.pullFunc(ctx, reference, targetDir)
}

// newCacheWorker builds a Worker rooted at a temp dir and stubs the package
// level ResolveCacheDigest so tests run fully offline. The returned cleanup
// function MUST be deferred by the caller; it restores ResolveCacheDigest to
// its original value so tests don't leak state to each other.
func newCacheWorker(t *testing.T, digest string) (worker *Worker, cfg *config.Config, cleanup func()) {
	t.Helper()

	cfg = config.NewWithRaw(&config.RawConfig{
		ServiceName: "test.csi.example.com",
		RootDir:     t.TempDir(),
	})
	sm, err := status.NewStatusManager()
	require.NoError(t, err)
	worker, err = NewWorker(cfg, sm)
	require.NoError(t, err)

	origResolve := ResolveCacheDigest
	ResolveCacheDigest = func(_ context.Context, _ string) (string, error) {
		if digest == "" {
			return "", errors.New("empty manifest digest")
		}
		return digest, nil
	}

	// Default puller: writes a sentinel file so callers can assert that the
	// cache content was materialized. Tests can override this.
	worker.newPuller = func(context.Context, *config.PullConfig, *status.Hook, *DiskQuotaChecker) Puller {
		return &fakePuller{pullFunc: func(_ context.Context, _, targetDir string) error {
			if err := os.MkdirAll(targetDir, 0755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(targetDir, "model.bin"), []byte("x"), 0644)
		}}
	}

	cleanup = func() { ResolveCacheDigest = origResolve }
	return worker, cfg, cleanup
}

// TestEnsureCachedModel covers the main branches of EnsureCachedModel: digest
// resolver failure, first pull populating the cache, and a second caller
// reusing the existing entry without re-pulling.
func TestEnsureCachedModel(t *testing.T) {
	t.Run("resolver error propagates", func(t *testing.T) {
		worker, _, cleanup := newCacheWorker(t, "")
		defer cleanup()
		worker.newPuller = func(context.Context, *config.PullConfig, *status.Hook, *DiskQuotaChecker) Puller {
			return &fakePuller{pullFunc: func(context.Context, string, string) error {
				t.Fatal("pull must not run when resolver fails")
				return nil
			}}
		}

		_, err := worker.EnsureCachedModel(context.Background(), "r/m:v1", "pvc-a")
		require.Error(t, err)
		require.Contains(t, err.Error(), "empty manifest digest")
	})

	t.Run("first pull and reuse", func(t *testing.T) {
		digest := "sha256:reuse"
		worker, cfg, cleanup := newCacheWorker(t, digest)
		defer cleanup()

		var pulls atomic.Int32
		worker.newPuller = func(context.Context, *config.PullConfig, *status.Hook, *DiskQuotaChecker) Puller {
			return &fakePuller{pullFunc: func(_ context.Context, _, targetDir string) error {
				pulls.Add(1)
				require.NoError(t, os.MkdirAll(targetDir, 0755))
				return os.WriteFile(filepath.Join(targetDir, "model.bin"), []byte("x"), 0644)
			}}
		}

		// First caller triggers the pull and registers pvc-a.
		got, err := worker.EnsureCachedModel(context.Background(), "r/m:v1", "pvc-a")
		require.NoError(t, err)
		require.Equal(t, digest, got)

		// Second caller must reuse the cache without re-pulling.
		_, err = worker.EnsureCachedModel(context.Background(), "r/m:v1", "pvc-b")
		require.NoError(t, err)
		require.Equal(t, int32(1), pulls.Load())

		// Cache content + ready marker + both refs exist.
		_, err = os.Stat(filepath.Join(cfg.Get().GetCacheContentDir(digest), "model.bin"))
		require.NoError(t, err)
		refsDir := cfg.Get().GetCacheRefsDir(digest)
		_, err = os.Stat(filepath.Join(refsDir, ".ready"))
		require.NoError(t, err)
		_, err = os.Stat(filepath.Join(refsDir, "pvc-a"))
		require.NoError(t, err)
		_, err = os.Stat(filepath.Join(refsDir, "pvc-b"))
		require.NoError(t, err)
	})
}

// TestEnsureCachedModel_ConcurrentDedup verifies that N concurrent callers
// for the same reference collapse into a single physical pull while each
// still registering its own ref file.
func TestEnsureCachedModel_ConcurrentDedup(t *testing.T) {
	digest := "sha256:concurrent"
	worker, cfg, cleanup := newCacheWorker(t, digest)
	defer cleanup()

	var pulls atomic.Int32
	worker.newPuller = func(context.Context, *config.PullConfig, *status.Hook, *DiskQuotaChecker) Puller {
		return &fakePuller{pullFunc: func(_ context.Context, _, targetDir string) error {
			time.Sleep(100 * time.Millisecond) // encourage overlap
			pulls.Add(1)
			require.NoError(t, os.MkdirAll(targetDir, 0755))
			return os.WriteFile(filepath.Join(targetDir, "model.bin"), []byte("x"), 0644)
		}}
	}

	const N = 10
	var wg sync.WaitGroup
	errs := make([]error, N)
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			refName := "pvc-" + string(rune('a'+idx))
			_, errs[idx] = worker.EnsureCachedModel(context.Background(), "r/m:v1", refName)
		}(i)
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, int32(1), pulls.Load(), "10 callers must collapse into 1 pull")

	refsDir := cfg.Get().GetCacheRefsDir(digest)
	entries, err := os.ReadDir(refsDir)
	require.NoError(t, err)
	names := map[string]bool{}
	for _, entry := range entries {
		names[entry.Name()] = true
	}
	require.True(t, names[".ready"])
	for i := 0; i < N; i++ {
		require.True(t, names["pvc-"+string(rune('a'+i))])
	}
}

// TestEnsureCachedModel_PullFailureCleansUp verifies that a failed pull
// cleans up the half-populated content dir and does NOT write the ready
// marker, so a subsequent retry can succeed.
func TestEnsureCachedModel_PullFailureCleansUp(t *testing.T) {
	digest := "sha256:boom"
	worker, cfg, cleanup := newCacheWorker(t, digest)
	defer cleanup()

	// First attempt: simulate a half-populated dir plus a failure.
	worker.newPuller = func(context.Context, *config.PullConfig, *status.Hook, *DiskQuotaChecker) Puller {
		return &fakePuller{pullFunc: func(_ context.Context, _, targetDir string) error {
			require.NoError(t, os.MkdirAll(targetDir, 0755))
			_ = os.WriteFile(filepath.Join(targetDir, "half.bin"), []byte("x"), 0644)
			return errors.New("pull exploded")
		}}
	}
	_, err := worker.EnsureCachedModel(context.Background(), "r/m:v1", "pvc-a")
	require.Error(t, err)

	_, statErr := os.Stat(cfg.Get().GetCacheContentDir(digest))
	require.True(t, os.IsNotExist(statErr), "content dir must be cleaned up after failure")
	_, statErr = os.Stat(filepath.Join(cfg.Get().GetCacheRefsDir(digest), ".ready"))
	require.True(t, os.IsNotExist(statErr), "ready marker must not exist after failure")

	// Retry with a working puller must succeed.
	worker.newPuller = func(context.Context, *config.PullConfig, *status.Hook, *DiskQuotaChecker) Puller {
		return &fakePuller{pullFunc: func(_ context.Context, _, targetDir string) error {
			require.NoError(t, os.MkdirAll(targetDir, 0755))
			return os.WriteFile(filepath.Join(targetDir, "model.bin"), []byte("x"), 0644)
		}}
	}
	got, err := worker.EnsureCachedModel(context.Background(), "r/m:v1", "pvc-a")
	require.NoError(t, err)
	require.Equal(t, digest, got)
}

// TestReleaseCachedModel exercises the full release state machine: empty
// digest / unknown digest no-op, keep-if-other-refs, GC-when-last-ref, and
// idempotent double-release.
func TestReleaseCachedModel(t *testing.T) {
	digest := "sha256:rel"
	worker, cfg, cleanup := newCacheWorker(t, digest)
	defer cleanup()

	// No-op paths first: empty / unknown digest must not error and must not
	// touch the filesystem.
	require.NoError(t, worker.ReleaseCachedModel(context.Background(), "", "pvc-a"))
	require.NoError(t, worker.ReleaseCachedModel(context.Background(), "sha256:ghost", "pvc-a"))

	// Populate the cache with two refs.
	_, err := worker.EnsureCachedModel(context.Background(), "r/m:v1", "pvc-a")
	require.NoError(t, err)
	_, err = worker.EnsureCachedModel(context.Background(), "r/m:v1", "pvc-b")
	require.NoError(t, err)

	// Release pvc-a: cache must stay alive because pvc-b is still there.
	require.NoError(t, worker.ReleaseCachedModel(context.Background(), digest, "pvc-a"))
	_, statErr := os.Stat(cfg.Get().GetCacheContentDir(digest))
	require.NoError(t, statErr, "content must persist while pvc-b holds a ref")
	_, statErr = os.Stat(filepath.Join(cfg.Get().GetCacheRefsDir(digest), "pvc-a"))
	require.True(t, os.IsNotExist(statErr))
	_, statErr = os.Stat(filepath.Join(cfg.Get().GetCacheRefsDir(digest), "pvc-b"))
	require.NoError(t, statErr)

	// Release pvc-b: last ref → GC.
	require.NoError(t, worker.ReleaseCachedModel(context.Background(), digest, "pvc-b"))
	_, statErr = os.Stat(cfg.Get().GetCacheContentDir(digest))
	require.True(t, os.IsNotExist(statErr), "content must be GC'd when the last ref is gone")
	_, statErr = os.Stat(cfg.Get().GetCacheRefsDir(digest))
	require.True(t, os.IsNotExist(statErr), "refs dir must be GC'd when the last ref is gone")

	// Releasing again after GC is a no-op, not an error.
	require.NoError(t, worker.ReleaseCachedModel(context.Background(), digest, "pvc-b"))
}

// TestFindCacheDigestByRef exercises the reverse lookup: missing refs root,
// no match, a valid match, and verifies the .ready sentinel never shadows a
// real ref entry.
func TestFindCacheDigestByRef(t *testing.T) {
	cfg := config.NewWithRaw(&config.RawConfig{ServiceName: "test", RootDir: t.TempDir()})
	sm, err := status.NewStatusManager()
	require.NoError(t, err)
	worker, err := NewWorker(cfg, sm)
	require.NoError(t, err)

	// Missing refs root → no result, no error.
	got, err := worker.FindCacheDigestByRef("pvc-a")
	require.NoError(t, err)
	require.Equal(t, "", got)

	// Digest1 only has the ready marker; digest2 has our ref.
	digest1 := "sha256:only-ready"
	digest2 := "sha256:has-ref"
	require.NoError(t, os.MkdirAll(cfg.Get().GetCacheRefsDir(digest1), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(cfg.Get().GetCacheRefsDir(digest1), ".ready"), nil, 0644))
	require.NoError(t, os.MkdirAll(cfg.Get().GetCacheRefsDir(digest2), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(cfg.Get().GetCacheRefsDir(digest2), ".ready"), nil, 0644))
	require.NoError(t, os.WriteFile(filepath.Join(cfg.Get().GetCacheRefsDir(digest2), "pvc-target"), nil, 0644))

	// Unknown ref → no result.
	got, err = worker.FindCacheDigestByRef("pvc-missing")
	require.NoError(t, err)
	require.Equal(t, "", got)

	// Existing ref → digest returned.
	got, err = worker.FindCacheDigestByRef("pvc-target")
	require.NoError(t, err)
	require.Equal(t, digest2, got)

	// Stray non-directory entries under refs root / algo dir must be skipped
	// gracefully instead of aborting the scan.
	refsRoot := cfg.Get().GetCacheRefsRootDir()
	require.NoError(t, os.WriteFile(filepath.Join(refsRoot, "stray-file"), nil, 0644))
	algoDir := filepath.Join(refsRoot, "sha256")
	require.NoError(t, os.WriteFile(filepath.Join(algoDir, "stray-under-algo"), nil, 0644))

	got, err = worker.FindCacheDigestByRef("pvc-target")
	require.NoError(t, err)
	require.Equal(t, digest2, got)

	// A miss-lookup after the strays are in place forces the scan to walk
	// past the non-directory entries under both refsRoot and algoDir,
	// exercising the "skip non-directory" branches end to end.
	got, err = worker.FindCacheDigestByRef("pvc-absent")
	require.NoError(t, err)
	require.Equal(t, "", got)
}

// TestFindCacheDigestByRef_ReadRootError covers the error branch where the
// refs root is not a directory (e.g. someone created a file at that path),
// which makes os.ReadDir return a non-IsNotExist error.
func TestFindCacheDigestByRef_ReadRootError(t *testing.T) {
	cfg := config.NewWithRaw(&config.RawConfig{ServiceName: "test", RootDir: t.TempDir()})
	sm, err := status.NewStatusManager()
	require.NoError(t, err)
	worker, err := NewWorker(cfg, sm)
	require.NoError(t, err)

	// Plant a regular file where refs root is expected so that ReadDir fails
	// with a "not a directory" error instead of IsNotExist.
	refsRoot := cfg.Get().GetCacheRefsRootDir()
	require.NoError(t, os.MkdirAll(filepath.Dir(refsRoot), 0755))
	require.NoError(t, os.WriteFile(refsRoot, []byte("not a dir"), 0644))

	got, err := worker.FindCacheDigestByRef("pvc-any")
	require.Error(t, err)
	require.Contains(t, err.Error(), "read cache refs root")
	require.Equal(t, "", got)
}

// TestReleaseCachedModel_RemoveRefError covers the branch where os.Remove on
// the ref file returns a non-IsNotExist error. We trigger this by making the
// "ref file" actually be a non-empty directory: os.Remove on a non-empty
// directory fails with ENOTEMPTY (not ENOENT), exercising the error path.
func TestReleaseCachedModel_RemoveRefError(t *testing.T) {
	digest := "sha256:bad-remove"
	worker, cfg, cleanup := newCacheWorker(t, digest)
	defer cleanup()

	refsDir := cfg.Get().GetCacheRefsDir(digest)
	require.NoError(t, os.MkdirAll(refsDir, 0755))

	// Plant a non-empty directory at the ref file path so os.Remove fails
	// with ENOTEMPTY instead of being tolerated as ENOENT.
	refAsDir := filepath.Join(refsDir, "pvc-a")
	require.NoError(t, os.MkdirAll(refAsDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(refAsDir, "blocker"), nil, 0644))

	err := worker.ReleaseCachedModel(context.Background(), digest, "pvc-a")
	require.Error(t, err)
	require.Contains(t, err.Error(), "remove cache ref file")
}

// TestEnsureCachedModel_WriteRefFileError covers the branch where writing
// this caller's ref file fails. We trigger this by pre-creating a directory
// at the ref file path, so os.WriteFile fails with EISDIR.
func TestEnsureCachedModel_WriteRefFileError(t *testing.T) {
	digest := "sha256:bad-ref-write"
	worker, cfg, cleanup := newCacheWorker(t, digest)
	defer cleanup()

	// Pre-populate the cache so the pull side returns immediately and we go
	// straight to the ref-registration step.
	contentDir := cfg.Get().GetCacheContentDir(digest)
	refsDir := cfg.Get().GetCacheRefsDir(digest)
	require.NoError(t, os.MkdirAll(contentDir, 0755))
	require.NoError(t, os.MkdirAll(refsDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(refsDir, ".ready"), nil, 0644))

	// Plant a directory where the ref file is expected so WriteFile fails.
	refName := "pvc-bad-ref"
	require.NoError(t, os.MkdirAll(filepath.Join(refsDir, refName), 0755))

	_, err := worker.EnsureCachedModel(context.Background(), "r/m:v1", refName)
	require.Error(t, err)
	require.Contains(t, err.Error(), "write cache ref file")
}

// TestEnsureCachedModel_CreateRefsDirError covers the branch where the refs
// dir cannot be created because a regular file already occupies that path.
func TestEnsureCachedModel_CreateRefsDirError(t *testing.T) {
	digest := "sha256:bad-mkdir"
	worker, cfg, cleanup := newCacheWorker(t, digest)
	defer cleanup()

	// Plant a regular file where the refs dir is expected so MkdirAll fails.
	refsDir := cfg.Get().GetCacheRefsDir(digest)
	require.NoError(t, os.MkdirAll(filepath.Dir(refsDir), 0755))
	require.NoError(t, os.WriteFile(refsDir, []byte("not a dir"), 0644))

	worker.newPuller = func(context.Context, *config.PullConfig, *status.Hook, *DiskQuotaChecker) Puller {
		return &fakePuller{pullFunc: func(context.Context, string, string) error {
			t.Fatal("pull must not run when refs dir cannot be created")
			return nil
		}}
	}

	_, err := worker.EnsureCachedModel(context.Background(), "r/m:v1", "pvc-a")
	require.Error(t, err)
	require.Contains(t, err.Error(), "create cache refs dir")
}
