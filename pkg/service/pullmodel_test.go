package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	modctlBackend "github.com/modelpack/modctl/pkg/backend"
	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/config/auth"
	"github.com/modelpack/model-csi-driver/pkg/status"
	pkgerrors "github.com/pkg/errors"
	"github.com/stretchr/testify/require"
)

type mockPuller struct {
	err error
}

func (m *mockPuller) Pull(ctx context.Context, reference, targetDir string, excludeModelWeights bool, excludeFilePatterns []string) error {
	if m.err != nil {
		return m.err
	}
	// Write a placeholder so the model store has a regular file to hardlink.
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(targetDir, "weights.bin"), []byte("payload"), 0644)
}

func newWorkerWithMockPuller(t *testing.T, pullErr error) *Worker {
	t.Helper()
	tmpDir := t.TempDir()
	rawCfg := &config.RawConfig{ServiceName: "test", RootDir: tmpDir}
	cfg := config.NewWithRaw(rawCfg)
	sm, err := status.NewStatusManager()
	require.NoError(t, err)

	worker, err := NewWorker(cfg, sm)
	require.NoError(t, err)

	worker.newPuller = func(ctx context.Context, pullCfg *config.PullConfig, hook *status.Hook, diskQuotaChecker *DiskQuotaChecker) Puller {
		return &mockPuller{err: pullErr}
	}
	// Deterministic digest from ref to avoid hitting a real registry.
	worker.resolveDigest = func(_ context.Context, reference string) (string, error) {
		sum := sha256.Sum256([]byte(reference))
		return "sha256:" + hex.EncodeToString(sum[:]), nil
	}
	return worker
}

func TestPullModel_Success(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	ctx := context.Background()
	volumeName := "pvc-pull-test"
	modelDir := filepath.Join(worker.cfg.Get().GetVolumeDir(volumeName), "model")

	err := worker.PullModel(ctx, true, volumeName, "", "test/model:latest", modelDir, false, false, nil)
	require.NoError(t, err)
}

func TestPullModel_Failure(t *testing.T) {
	worker := newWorkerWithMockPuller(t, pkgerrors.New("pull failed"))
	ctx := context.Background()
	volumeName := "pvc-pull-fail"
	modelDir := filepath.Join(worker.cfg.Get().GetVolumeDir(volumeName), "model")

	err := worker.PullModel(ctx, true, volumeName, "", "test/model:latest", modelDir, false, false, nil)
	require.Error(t, err)
}

func TestPullModel_DynamicVolume_Success(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	ctx := context.Background()
	volumeName := "csi-dyn-pull"
	mountID := "mount-1"
	modelDir := worker.cfg.Get().GetModelDirForDynamic(volumeName, mountID)

	err := worker.PullModel(ctx, false, volumeName, mountID, "test/model:latest", modelDir, false, false, nil)
	require.NoError(t, err)
}

func TestPullModel_DynamicVolume_Failure(t *testing.T) {
	worker := newWorkerWithMockPuller(t, pkgerrors.New("network error"))
	ctx := context.Background()
	volumeName := "csi-dyn-fail"
	mountID := "mount-2"
	modelDir := worker.cfg.Get().GetModelDirForDynamic(volumeName, mountID)

	err := worker.PullModel(ctx, false, volumeName, mountID, "test/model:latest", modelDir, false, false, nil)
	require.Error(t, err)
}

// ─── Worker + ModelStore integration ─────────────────────────────────────────

func TestPullModel_StoreMaterializesAndStatusCarriesCacheKey(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	ctx := context.Background()
	volumeName := "pvc-store-1"
	reference := "test/model:v1"
	modelDir := worker.cfg.Get().GetModelDir(volumeName)

	require.NoError(t, worker.PullModel(ctx, true, volumeName, "", reference, modelDir, false, false, nil))

	// status.json must record the cache key.
	statusPath := filepath.Join(worker.cfg.Get().GetVolumeDir(volumeName), "status.json")
	st, err := worker.sm.Get(statusPath)
	require.NoError(t, err)
	require.NotEmpty(t, st.CacheKey)
	require.Equal(t, status.StatePullSucceeded, st.State)

	// modelDir is hardlinked to the store payload.
	volStat, err := os.Stat(filepath.Join(modelDir, "weights.bin"))
	require.NoError(t, err)

	algo, hex, _ := splitTestDigest(t, st.CacheKey)
	storeFile := filepath.Join(worker.cfg.Get().RootDir, "store", algo, hex, "data", "weights.bin")
	storeStat, err := os.Stat(storeFile)
	require.NoError(t, err)
	require.True(t, os.SameFile(volStat, storeStat))
}

func TestPullModel_StoreSharedAcrossVolumes(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	ctx := context.Background()
	reference := "test/model:shared"

	modelDirA := worker.cfg.Get().GetModelDir("pvc-share-a")
	modelDirB := worker.cfg.Get().GetModelDir("pvc-share-b")
	require.NoError(t, worker.PullModel(ctx, true, "pvc-share-a", "", reference, modelDirA, false, false, nil))
	require.NoError(t, worker.PullModel(ctx, true, "pvc-share-b", "", reference, modelDirB, false, false, nil))

	a, err := os.Stat(filepath.Join(modelDirA, "weights.bin"))
	require.NoError(t, err)
	b, err := os.Stat(filepath.Join(modelDirB, "weights.bin"))
	require.NoError(t, err)
	require.True(t, os.SameFile(a, b), "both volumes must hardlink the same store inode")
}

func TestPullModel_ExcludeBypassesStore(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	ctx := context.Background()
	volumeName := "pvc-exclude"
	modelDir := worker.cfg.Get().GetModelDir(volumeName)

	require.NoError(t, worker.PullModel(ctx, true, volumeName, "", "test/model:exc", modelDir, false, true, nil))

	statusPath := filepath.Join(worker.cfg.Get().GetVolumeDir(volumeName), "status.json")
	st, err := worker.sm.Get(statusPath)
	require.NoError(t, err)
	require.Empty(t, st.CacheKey, "exclude pull must not register a cache key")

	// Store dir must not exist.
	_, err = os.Stat(filepath.Join(worker.cfg.Get().RootDir, "store"))
	require.True(t, os.IsNotExist(err))
}

func TestPullModel_ResolveDigestFailure(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	wantErr := pkgerrors.New("resolve boom")
	worker.resolveDigest = func(context.Context, string) (string, error) { return "", wantErr }

	ctx := context.Background()
	volumeName := "pvc-resolve-fail"
	modelDir := worker.cfg.Get().GetModelDir(volumeName)

	err := worker.PullModel(ctx, true, volumeName, "", "test/model:bad", modelDir, false, false, nil)
	require.Error(t, err)
}

func TestDeleteModel_TriggersStoreGC(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	ctx := context.Background()
	volumeName := "pvc-gc"
	reference := "test/model:gc"
	modelDir := worker.cfg.Get().GetModelDir(volumeName)

	require.NoError(t, worker.PullModel(ctx, true, volumeName, "", reference, modelDir, false, false, nil))

	statusPath := filepath.Join(worker.cfg.Get().GetVolumeDir(volumeName), "status.json")
	st, err := worker.sm.Get(statusPath)
	require.NoError(t, err)
	algo, hex, _ := splitTestDigest(t, st.CacheKey)
	storeDir := filepath.Join(worker.cfg.Get().RootDir, "store", algo, hex)
	require.DirExists(t, storeDir)

	require.NoError(t, worker.DeleteModel(ctx, true, volumeName, ""))
	_, err = os.Stat(storeDir)
	require.True(t, os.IsNotExist(err), "store dir should be GC'd after the last volume is deleted")
}

func TestDeleteModel_KeepsStoreWithSurvivingVolume(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	ctx := context.Background()
	reference := "test/model:survive"

	modelDirA := worker.cfg.Get().GetModelDir("pvc-survive-a")
	modelDirB := worker.cfg.Get().GetModelDir("pvc-survive-b")
	require.NoError(t, worker.PullModel(ctx, true, "pvc-survive-a", "", reference, modelDirA, false, false, nil))
	require.NoError(t, worker.PullModel(ctx, true, "pvc-survive-b", "", reference, modelDirB, false, false, nil))

	statusPath := filepath.Join(worker.cfg.Get().GetVolumeDir("pvc-survive-a"), "status.json")
	st, err := worker.sm.Get(statusPath)
	require.NoError(t, err)
	algo, hex, _ := splitTestDigest(t, st.CacheKey)
	storeDir := filepath.Join(worker.cfg.Get().RootDir, "store", algo, hex)

	require.NoError(t, worker.DeleteModel(ctx, true, "pvc-survive-a", ""))
	require.DirExists(t, storeDir, "surviving volume's hardlinks must keep store alive")

	require.NoError(t, worker.DeleteModel(ctx, true, "pvc-survive-b", ""))
	_, err = os.Stat(storeDir)
	require.True(t, os.IsNotExist(err))
}

func splitTestDigest(t *testing.T, digest string) (string, string, bool) {
	t.Helper()
	parts := []string{"", ""}
	for i, ch := range digest {
		if ch == ':' {
			parts[0] = digest[:i]
			parts[1] = digest[i+1:]
			return parts[0], parts[1], true
		}
	}
	return "", "", false
}

// ─── ReleaseVolumeTree ─────────────────────────────────────────────────────────────────

// Tear down the entire volume tree (static-inline / dynamic-root path)
// and ensure the store no longer has dangling entries.
func TestReleaseVolumeTree_GCsAllReferencedStores(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	ctx := context.Background()

	// Two mounts, same ref -> same cache key.
	volumeName := "csi-multi"
	refSame := "test/model:multi"
	dir1 := worker.cfg.Get().GetModelDirForDynamic(volumeName, "m1")
	dir2 := worker.cfg.Get().GetModelDirForDynamic(volumeName, "m2")
	require.NoError(t, worker.PullModel(ctx, false, volumeName, "m1", refSame, dir1, false, false, nil))
	require.NoError(t, worker.PullModel(ctx, false, volumeName, "m2", refSame, dir2, false, false, nil))

	// Third mount with a different ref -> different cache key.
	refOther := "test/model:other"
	dir3 := worker.cfg.Get().GetModelDirForDynamic(volumeName, "m3")
	require.NoError(t, worker.PullModel(ctx, false, volumeName, "m3", refOther, dir3, false, false, nil))

	storeRoot := filepath.Join(worker.cfg.Get().RootDir, "store")
	entries, err := os.ReadDir(filepath.Join(storeRoot, "sha256"))
	require.NoError(t, err)
	require.Len(t, entries, 2)

	volumeDir := worker.cfg.Get().GetVolumeDirForDynamic(volumeName)
	require.NoError(t, worker.ReleaseVolumeTree(ctx, volumeDir))

	_, statErr := os.Stat(volumeDir)
	require.True(t, os.IsNotExist(statErr))

	entries, err = os.ReadDir(filepath.Join(storeRoot, "sha256"))
	if err == nil {
		require.Empty(t, entries, "all store entries should be reclaimed")
	} else {
		require.True(t, os.IsNotExist(err))
	}
}

func TestReleaseVolumeTree_MissingDirIsNoOp(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	require.NoError(t, worker.ReleaseVolumeTree(context.Background(), filepath.Join(t.TempDir(), "missing")))
}

func TestReleaseVolumeTree_NoCacheKeysIsNoOp(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	dir := t.TempDir()
	// Create a status.json with empty CacheKey (e.g. exclude pull).
	st := status.Status{VolumeName: "x", State: status.StatePullSucceeded}
	_, err := worker.sm.Set(filepath.Join(dir, "status.json"), st)
	require.NoError(t, err)

	require.NoError(t, worker.ReleaseVolumeTree(context.Background(), dir))
	_, statErr := os.Stat(dir)
	require.True(t, os.IsNotExist(statErr))
}

func TestReleaseVolumeTree_RemoveAllError(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	volumeDir := t.TempDir()
	wantErr := pkgerrors.New("rm boom")
	withFsHooks(t, func(string) error { return wantErr }, nil, nil)

	require.ErrorIs(t, worker.ReleaseVolumeTree(context.Background(), volumeDir), wantErr)
}

// ─── defaultResolveDigest ──────────────────────────────────────────────────────────

func TestDefaultResolveDigest_Success(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	worker.resolveDigest = defaultResolveDigest

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(auth.GetKeyChainByRef, func(string) (*auth.PassKeyChain, error) {
		return &auth.PassKeyChain{ServerScheme: "https"}, nil
	})
	patches.ApplyFunc(modctlBackend.New, func(string) (modctlBackend.Backend, error) {
		return nil, nil
	})
	patches.ApplyMethod(reflect.TypeOf(&ModelArtifact{}), "Inspect", func(*ModelArtifact, context.Context, string) (*modctlBackend.InspectedModelArtifact, error) {
		return &modctlBackend.InspectedModelArtifact{Digest: "sha256:resolved"}, nil
	})

	digest, err := worker.resolveDigest(context.Background(), "example.com/m:1")
	require.NoError(t, err)
	require.Equal(t, "sha256:resolved", digest)
}

func TestDefaultResolveDigest_AuthError(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	worker.resolveDigest = defaultResolveDigest

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(auth.GetKeyChainByRef, func(string) (*auth.PassKeyChain, error) {
		return nil, pkgerrors.New("no auth")
	})

	_, err := worker.resolveDigest(context.Background(), "example.com/m:1")
	require.Error(t, err)
}

func TestDefaultResolveDigest_BackendError(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	worker.resolveDigest = defaultResolveDigest

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(auth.GetKeyChainByRef, func(string) (*auth.PassKeyChain, error) {
		return &auth.PassKeyChain{ServerScheme: "https"}, nil
	})
	patches.ApplyFunc(modctlBackend.New, func(string) (modctlBackend.Backend, error) {
		return nil, pkgerrors.New("backend init")
	})

	_, err := worker.resolveDigest(context.Background(), "example.com/m:1")
	require.Error(t, err)
}

func TestDefaultResolveDigest_InspectError(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	worker.resolveDigest = defaultResolveDigest

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(auth.GetKeyChainByRef, func(string) (*auth.PassKeyChain, error) {
		return &auth.PassKeyChain{ServerScheme: "https"}, nil
	})
	patches.ApplyFunc(modctlBackend.New, func(string) (modctlBackend.Backend, error) {
		return nil, nil
	})
	patches.ApplyMethod(reflect.TypeOf(&ModelArtifact{}), "Inspect", func(*ModelArtifact, context.Context, string) (*modctlBackend.InspectedModelArtifact, error) {
		return nil, pkgerrors.New("inspect failed")
	})

	_, err := worker.resolveDigest(context.Background(), "example.com/m:1")
	require.Error(t, err)
}

// ctxAwarePuller blocks on ctx so callers can trigger Canceled /
// DeadlineExceeded.
type ctxAwarePuller struct{}

func (ctxAwarePuller) Pull(ctx context.Context, _, _ string, _ bool, _ []string) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestPullModel_CanceledBranch(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	worker.newPuller = func(context.Context, *config.PullConfig, *status.Hook, *DiskQuotaChecker) Puller {
		return ctxAwarePuller{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { cancel() }()

	volumeName := "pvc-cancel-branch"
	modelDir := worker.cfg.Get().GetModelDir(volumeName)
	err := worker.PullModel(ctx, true, volumeName, "", "test/model:cancel", modelDir, false, false, nil)
	require.Error(t, err)

	statusPath := filepath.Join(worker.cfg.Get().GetVolumeDir(volumeName), "status.json")
	st, _ := worker.sm.Get(statusPath)
	if st != nil {
		require.Equal(t, status.StatePullCanceled, st.State)
	}
}

func TestPullModel_DeadlineBranch(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	worker.newPuller = func(context.Context, *config.PullConfig, *status.Hook, *DiskQuotaChecker) Puller {
		return ctxAwarePuller{}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	volumeName := "pvc-deadline-branch"
	modelDir := worker.cfg.Get().GetModelDir(volumeName)
	err := worker.PullModel(ctx, true, volumeName, "", "test/model:deadline", modelDir, false, false, nil)
	require.Error(t, err)

	statusPath := filepath.Join(worker.cfg.Get().GetVolumeDir(volumeName), "status.json")
	st, _ := worker.sm.Get(statusPath)
	if st != nil {
		require.Equal(t, status.StatePullTimeout, st.State)
	}
}

// ReleaseVolumeTree must swallow MaybeGC errors and still succeed.
func TestReleaseVolumeTree_MaybeGCErrorIsSwallowed(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	ctx := context.Background()

	volumeName := "csi-rgcerr"
	modelDir := worker.cfg.Get().GetModelDirForDynamic(volumeName, "m1")
	require.NoError(t, worker.PullModel(ctx, false, volumeName, "m1", "test/model:rg", modelDir, false, false, nil))

	storeRoot := filepath.Join(worker.cfg.Get().RootDir, "store")
	origRemoveAll := fsRemoveAll
	t.Cleanup(func() { fsRemoveAll = origRemoveAll })
	fsRemoveAll = func(path string) error {
		if strings.HasPrefix(path, storeRoot) {
			return pkgerrors.New("gc rm boom")
		}
		return origRemoveAll(path)
	}

	volumeDir := worker.cfg.Get().GetVolumeDirForDynamic(volumeName)
	require.NoError(t, worker.ReleaseVolumeTree(ctx, volumeDir))
}

// Patching StatusManager.Set exercises the setStatus error branches.
func TestPullModel_SetStatusError_Succeeded(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(worker.sm), "Set", func(*status.StatusManager, string, status.Status) (*status.Status, error) {
		return nil, pkgerrors.New("set status boom")
	})

	volumeName := "pvc-set-status-fail"
	modelDir := worker.cfg.Get().GetModelDir(volumeName)
	err := worker.PullModel(context.Background(), true, volumeName, "", "test/model:set", modelDir, false, false, nil)
	require.Error(t, err)
}

func TestPullModel_SetStatusError_AfterResolveDigestFailure(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	worker.resolveDigest = func(context.Context, string) (string, error) {
		return "", pkgerrors.New("resolve boom")
	}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(worker.sm), "Set", func(*status.StatusManager, string, status.Status) (*status.Status, error) {
		return nil, pkgerrors.New("set status boom")
	})

	volumeName := "pvc-resolve-set-fail"
	modelDir := worker.cfg.Get().GetModelDir(volumeName)
	err := worker.PullModel(context.Background(), true, volumeName, "", "test/model:set", modelDir, false, false, nil)
	require.Error(t, err)
}

func TestPullModel_SetStatusError_AfterSuccessfulPull(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	// 1st Set (PullRunning) succeeds, the rest fail.
	patches.ApplyMethodSeq(reflect.TypeOf(worker.sm), "Set", []gomonkey.OutputCell{
		{Values: gomonkey.Params{(*status.Status)(nil), nil}, Times: 1},
		{Values: gomonkey.Params{(*status.Status)(nil), pkgerrors.New("set status boom")}, Times: 100},
	})

	volumeName := "pvc-success-set-fail"
	modelDir := worker.cfg.Get().GetModelDir(volumeName)
	err := worker.PullModel(context.Background(), true, volumeName, "", "test/model:succ-set", modelDir, false, false, nil)
	require.Error(t, err)
}

func TestPullModel_SetStatusError_OnPullFailure(t *testing.T) {
	worker := newWorkerWithMockPuller(t, pkgerrors.New("pull boom"))
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethodSeq(reflect.TypeOf(worker.sm), "Set", []gomonkey.OutputCell{
		{Values: gomonkey.Params{(*status.Status)(nil), nil}, Times: 1},
		{Values: gomonkey.Params{(*status.Status)(nil), pkgerrors.New("set status boom")}, Times: 100},
	})

	volumeName := "pvc-pull-set-fail"
	modelDir := worker.cfg.Get().GetModelDir(volumeName)
	err := worker.PullModel(context.Background(), true, volumeName, "", "test/model:set", modelDir, false, false, nil)
	require.Error(t, err)
}

func TestPullModel_SetStatusError_OnCanceledPull(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	worker.newPuller = func(context.Context, *config.PullConfig, *status.Hook, *DiskQuotaChecker) Puller {
		return ctxAwarePuller{}
	}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethodSeq(reflect.TypeOf(worker.sm), "Set", []gomonkey.OutputCell{
		{Values: gomonkey.Params{(*status.Status)(nil), nil}, Times: 1},
		{Values: gomonkey.Params{(*status.Status)(nil), pkgerrors.New("set status boom")}, Times: 100},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	volumeName := "pvc-cancel-set-fail"
	modelDir := worker.cfg.Get().GetModelDir(volumeName)
	err := worker.PullModel(ctx, true, volumeName, "", "test/model:set", modelDir, false, false, nil)
	require.Error(t, err)
}

func TestPullModel_SetStatusError_OnDeadlinePull(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	worker.newPuller = func(context.Context, *config.PullConfig, *status.Hook, *DiskQuotaChecker) Puller {
		return ctxAwarePuller{}
	}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethodSeq(reflect.TypeOf(worker.sm), "Set", []gomonkey.OutputCell{
		{Values: gomonkey.Params{(*status.Status)(nil), nil}, Times: 1},
		{Values: gomonkey.Params{(*status.Status)(nil), pkgerrors.New("set status boom")}, Times: 100},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	volumeName := "pvc-deadline-set-fail"
	modelDir := worker.cfg.Get().GetModelDir(volumeName)
	err := worker.PullModel(ctx, true, volumeName, "", "test/model:set", modelDir, false, false, nil)
	require.Error(t, err)
}

// collectCacheKeys skips corrupted status files.
func TestCollectCacheKeys_SkipsBadStatus(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	dir := t.TempDir()

	good := filepath.Join(dir, "a", "status.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(good), 0755))
	_, err := worker.sm.Set(good, status.Status{VolumeName: "x", CacheKey: "sha256:dead", State: status.StatePullSucceeded})
	require.NoError(t, err)

	bad := filepath.Join(dir, "b", "status.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(bad), 0755))
	require.NoError(t, os.WriteFile(bad, []byte("not-json"), 0644))

	keys := worker.collectCacheKeys(context.Background(), dir)
	require.Contains(t, keys, "sha256:dead")
}

func TestDefaultResolveDigest_EmptyDigest(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	worker.resolveDigest = defaultResolveDigest

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(auth.GetKeyChainByRef, func(string) (*auth.PassKeyChain, error) {
		return &auth.PassKeyChain{ServerScheme: "http"}, nil
	})
	patches.ApplyFunc(modctlBackend.New, func(string) (modctlBackend.Backend, error) {
		return nil, nil
	})
	patches.ApplyMethod(reflect.TypeOf(&ModelArtifact{}), "Inspect", func(*ModelArtifact, context.Context, string) (*modctlBackend.InspectedModelArtifact, error) {
		return &modctlBackend.InspectedModelArtifact{Digest: ""}, nil
	})

	_, err := worker.resolveDigest(context.Background(), "example.com/m:1")
	require.Error(t, err)
}
