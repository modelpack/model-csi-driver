package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	modctlbackend "github.com/modelpack/modctl/pkg/backend"
	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/config/auth"
	"github.com/modelpack/model-csi-driver/pkg/mounter"
	"github.com/modelpack/model-csi-driver/pkg/status"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
)

// applyInlineCachePatches stubs the shared-cache publish path so it runs
// fully offline: ResolveCacheDigest returns the canned digest and the worker's
// puller writes a sentinel file into the cache content directory. When
// pullErr is non-nil the puller returns it instead. onPull (if non-nil) is
// invoked on every pull call so tests can count physical pulls.
//
// The returned *gomonkey.Patches is preserved purely for backwards
// compatibility with existing call sites that defer Reset() on it; the
// digest-resolver swap is undone via t.Cleanup so callers don't need to.
func applyInlineCachePatches(t *testing.T, svc *Service, digest string, pullErr error, onPull func()) *gomonkey.Patches {
	t.Helper()

	origResolve := ResolveCacheDigest
	ResolveCacheDigest = func(_ context.Context, _ string) (string, error) {
		if digest == "" {
			return "", errors.New("empty manifest digest")
		}
		return digest, nil
	}
	t.Cleanup(func() { ResolveCacheDigest = origResolve })

	svc.worker.newPuller = func(context.Context, *config.PullConfig, *status.Hook, *DiskQuotaChecker) Puller {
		return &fakePuller{pullFunc: func(_ context.Context, _, targetDir string) error {
			if onPull != nil {
				onPull()
			}
			if pullErr != nil {
				return pullErr
			}
			if err := os.MkdirAll(targetDir, 0755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(targetDir, "model.bin"), []byte("x"), 0644)
		}}
	}

	return gomonkey.NewPatches()
}

// TestNodePublishVolumeStaticInlineVolume_SharedCache covers the publish
// happy path: first mount triggers a pull, a second mount of the same
// reference reuses the shared cache without re-pulling, both volumes record
// CacheDigest in status.json, and the bind mount targets the shared content
// directory.
func TestNodePublishVolumeStaticInlineVolume_SharedCache(t *testing.T) {
	svc, _ := newNodeService(t)
	ctx := context.Background()

	digest := "sha256:sharedcache"
	pulls := 0
	patches := applyInlineCachePatches(t, svc, digest, nil, func() { pulls++ })
	defer patches.Reset()

	var mountCmds []string
	patchMount := gomonkey.ApplyFunc(mounter.Mount, func(_ context.Context, b mounter.Builder) error {
		cmd, err := b.Build()
		if err != nil {
			return err
		}
		mountCmds = append(mountCmds, cmd.String())
		return nil
	})
	defer patchMount.Reset()

	// First pod mounts.
	_, err := svc.nodePublishVolumeStaticInlineVolume(ctx, "pvc-a", t.TempDir(), "r/m:v1", false, nil)
	require.NoError(t, err)

	// Second pod mounts the same reference.
	_, err = svc.nodePublishVolumeStaticInlineVolume(ctx, "pvc-b", t.TempDir(), "r/m:v1", false, nil)
	require.NoError(t, err)

	require.Equal(t, 1, pulls, "second mount must reuse the shared cache")

	// Bind mount source must point at the shared cache content directory.
	expectedSrc := svc.cfg.Get().GetCacheContentDir(digest)
	require.Len(t, mountCmds, 2)
	for _, cmd := range mountCmds {
		require.Contains(t, cmd, "--bind")
		require.Contains(t, cmd, expectedSrc)
	}

	// Both refs should exist under the refs tree.
	refsDir := svc.cfg.Get().GetCacheRefsDir(digest)
	_, err = os.Stat(filepath.Join(refsDir, "pvc-a"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(refsDir, "pvc-b"))
	require.NoError(t, err)

	// status.json must record the digest for both volumes.
	for _, vol := range []string{"pvc-a", "pvc-b"} {
		statusPath := filepath.Join(svc.cfg.Get().GetVolumeDir(vol), "status.json")
		s, err := svc.sm.Get(statusPath)
		require.NoError(t, err)
		require.Equal(t, digest, s.CacheDigest)
		require.Equal(t, status.StateMounted, s.State)
		require.True(t, s.Inline)
	}
}

// TestNodePublishVolumeStaticInlineVolume_Failures covers the two error
// branches of the publish path:
//   - pull failure: status.json is persisted with PULL_FAILED and no digest.
//   - bind mount failure after successful pull: the ref we just registered
//     is rolled back so the cache is GC'd instead of leaking forever.
func TestNodePublishVolumeStaticInlineVolume_Failures(t *testing.T) {
	t.Run("pull failure persists status", func(t *testing.T) {
		svc, _ := newNodeService(t)
		patches := applyInlineCachePatches(t, svc, "sha256:pullfail", errors.New("net down"), nil)
		defer patches.Reset()

		volumeName := "pvc-pullfail"
		_, err := svc.nodePublishVolumeStaticInlineVolume(
			context.Background(), volumeName, t.TempDir(), "r/m:v1", false, nil,
		)
		require.Error(t, err)

		statusPath := filepath.Join(svc.cfg.Get().GetVolumeDir(volumeName), "status.json")
		s, err := svc.sm.Get(statusPath)
		require.NoError(t, err)
		require.Equal(t, status.StatePullFailed, s.State)
		require.Equal(t, "", s.CacheDigest)
	})

	t.Run("bind mount failure rolls back ref", func(t *testing.T) {
		svc, _ := newNodeService(t)
		digest := "sha256:mountfail"
		patches := applyInlineCachePatches(t, svc, digest, nil, nil)
		defer patches.Reset()

		patchMount := gomonkey.ApplyFunc(mounter.Mount, func(_ context.Context, _ mounter.Builder) error {
			return errors.New("bind failed")
		})
		defer patchMount.Reset()

		volumeName := "pvc-mountfail"
		_, err := svc.nodePublishVolumeStaticInlineVolume(
			context.Background(), volumeName, t.TempDir(), "r/m:v1", false, nil,
		)
		require.Error(t, err)

		// Ref must be rolled back, and since it was the only ref, GC removes
		// the entire cache entry.
		_, statErr := os.Stat(svc.cfg.Get().GetCacheContentDir(digest))
		require.True(t, os.IsNotExist(statErr))
		_, statErr = os.Stat(svc.cfg.Get().GetCacheRefsDir(digest))
		require.True(t, os.IsNotExist(statErr))
	})
}

// TestNodeUnPublishVolumeStaticInlineVolume covers the three unpublish
// branches:
//   - release by digest recorded in status.json (happy path),
//   - fallback reverse scan when status.json lacks CacheDigest,
//   - clean volume dir when there is neither status nor cache (degenerate).
func TestNodeUnPublishVolumeStaticInlineVolume(t *testing.T) {
	patchUMount := gomonkey.ApplyFunc(mounter.UMount, func(_ context.Context, _ string, _ bool) error {
		return nil
	})
	defer patchUMount.Reset()

	t.Run("release by digest from status", func(t *testing.T) {
		svc, _ := newNodeService(t)
		digest := "sha256:unpub-digest"
		patches := applyInlineCachePatches(t, svc, digest, nil, nil)
		defer patches.Reset()
		patchMount := gomonkey.ApplyFunc(mounter.Mount, func(_ context.Context, _ mounter.Builder) error {
			return nil
		})
		defer patchMount.Reset()

		volumeName := "pvc-unpub-digest"
		_, err := svc.nodePublishVolumeStaticInlineVolume(
			context.Background(), volumeName, t.TempDir(), "r/m:v1", false, nil,
		)
		require.NoError(t, err)

		_, err = svc.nodeUnPublishVolumeStaticInlineVolume(context.Background(), volumeName, t.TempDir(), true)
		require.NoError(t, err)

		// Last ref → cache GC'd, volume dir removed.
		_, statErr := os.Stat(svc.cfg.Get().GetCacheContentDir(digest))
		require.True(t, os.IsNotExist(statErr))
		_, statErr = os.Stat(svc.cfg.Get().GetVolumeDir(volumeName))
		require.True(t, os.IsNotExist(statErr))
	})

	t.Run("fallback scan when status has no digest", func(t *testing.T) {
		svc, _ := newNodeService(t)
		digest := "sha256:unpub-scan"
		volumeName := "pvc-unpub-scan"

		// Simulate an older on-disk layout: cache + refs populated by hand,
		// status.json has no CacheDigest field.
		contentDir := svc.cfg.Get().GetCacheContentDir(digest)
		refsDir := svc.cfg.Get().GetCacheRefsDir(digest)
		require.NoError(t, os.MkdirAll(contentDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(contentDir, "model.bin"), []byte("x"), 0644))
		require.NoError(t, os.MkdirAll(refsDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(refsDir, ".ready"), nil, 0644))
		require.NoError(t, os.WriteFile(filepath.Join(refsDir, volumeName), nil, 0644))

		volumeDir := svc.cfg.Get().GetVolumeDir(volumeName)
		require.NoError(t, os.MkdirAll(volumeDir, 0755))
		_, err := svc.sm.Set(filepath.Join(volumeDir, "status.json"), status.Status{
			VolumeName: volumeName,
			Reference:  "r/m:v1",
			Inline:     true,
			State:      status.StateMounted,
		})
		require.NoError(t, err)

		_, err = svc.nodeUnPublishVolumeStaticInlineVolume(context.Background(), volumeName, t.TempDir(), true)
		require.NoError(t, err)

		// Reverse scan must have located the digest and GC'd everything.
		_, statErr := os.Stat(contentDir)
		require.True(t, os.IsNotExist(statErr))
		_, statErr = os.Stat(refsDir)
		require.True(t, os.IsNotExist(statErr))
		_, statErr = os.Stat(volumeDir)
		require.True(t, os.IsNotExist(statErr))
	})

	t.Run("no cache no status still cleans volume", func(t *testing.T) {
		svc, _ := newNodeService(t)
		volumeName := "pvc-nothing"
		volumeDir := svc.cfg.Get().GetVolumeDir(volumeName)
		require.NoError(t, os.MkdirAll(volumeDir, 0755))

		_, err := svc.nodeUnPublishVolumeStaticInlineVolume(context.Background(), volumeName, t.TempDir(), false)
		require.NoError(t, err)

		_, statErr := os.Stat(volumeDir)
		require.True(t, os.IsNotExist(statErr))
	})

	t.Run("umount failure propagates", func(t *testing.T) {
		svc, _ := newNodeService(t)
		patchUMount := gomonkey.ApplyFunc(mounter.UMount, func(_ context.Context, _ string, _ bool) error {
			return errors.New("umount boom")
		})
		defer patchUMount.Reset()

		_, err := svc.nodeUnPublishVolumeStaticInlineVolume(context.Background(), "pvc-x", t.TempDir(), true)
		require.Error(t, err)
		require.Contains(t, err.Error(), "unmount target path")
	})
}

// legacyPuller is a Puller that captures the exclusion arguments passed to
// Pull so the legacy-path test can assert them.
type legacyPuller struct {
	targetDir           string
	excludeModelWeights bool
	excludeFilePatterns []string
}

func (p *legacyPuller) Pull(_ context.Context, _, targetDir string, excludeModelWeights bool, excludeFilePatterns []string) error {
	p.targetDir = targetDir
	p.excludeModelWeights = excludeModelWeights
	p.excludeFilePatterns = excludeFilePatterns
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(targetDir, "partial.bin"), []byte("x"), 0644)
}

// TestNodePublishVolumeStaticInlineVolumeLegacy covers the fallback per-volume
// pull path used when excludeModelWeights / excludeFilePatterns is set. Those
// partial-file variants intentionally do not go through the shared cache, so
// we validate the puller is invoked with the exclusion flags and the bind
// mount targets the per-volume model dir.
func TestNodePublishVolumeStaticInlineVolumeLegacy(t *testing.T) {
	svc, _ := newNodeService(t)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(auth.GetKeyChainByRef, func(string) (*auth.PassKeyChain, error) {
		return &auth.PassKeyChain{ServerScheme: "https"}, nil
	})
	patches.ApplyFunc(modctlbackend.New, func(string) (modctlbackend.Backend, error) {
		return nil, nil
	})

	captured := &legacyPuller{}
	svc.worker.newPuller = func(context.Context, *config.PullConfig, *status.Hook, *DiskQuotaChecker) Puller {
		return captured
	}

	var mountCmd string
	patchMount := gomonkey.ApplyFunc(mounter.Mount, func(_ context.Context, b mounter.Builder) error {
		cmd, err := b.Build()
		if err != nil {
			return err
		}
		mountCmd = cmd.String()
		return nil
	})
	defer patchMount.Reset()

	volumeName := "pvc-legacy"
	_, err := svc.nodePublishVolumeStaticInlineVolume(
		context.Background(), volumeName, t.TempDir(),
		"r/m:v1", true, []string{"*.ignore"},
	)
	require.NoError(t, err)

	// Pull target must be the per-volume model dir, not a shared cache dir.
	expectedModelDir := svc.cfg.Get().GetModelDir(volumeName)
	require.Equal(t, expectedModelDir, captured.targetDir)
	require.True(t, captured.excludeModelWeights)
	require.Equal(t, []string{"*.ignore"}, captured.excludeFilePatterns)
	require.Contains(t, mountCmd, expectedModelDir)

	// status.json must be marked inline and mounted, and carry no CacheDigest.
	statusPath := filepath.Join(svc.cfg.Get().GetVolumeDir(volumeName), "status.json")
	s, err := svc.sm.Get(statusPath)
	require.NoError(t, err)
	require.True(t, s.Inline)
	require.Equal(t, status.StateMounted, s.State)
	require.Equal(t, "", s.CacheDigest)
}

// TestNodePublishVolumeStaticInlineVolume_StatusSetErrors covers two error
// branches that depend on sm.Set failing at different points:
//   - initial PullRunning Set fails before any pull is attempted,
//   - final Mounted Set fails after a successful pull + mount, in which case
//     the function still returns an error.
func TestNodePublishVolumeStaticInlineVolume_StatusSetErrors(t *testing.T) {
	t.Run("initial set fails", func(t *testing.T) {
		svc, _ := newNodeService(t)

		// Make the very first sm.Set call fail by planting a regular file at
		// the volume dir path so MkdirAll inside the status manager errors.
		volumeName := "pvc-initset-fail"
		volumesDir := svc.cfg.Get().GetVolumesDir()
		require.NoError(t, os.MkdirAll(volumesDir, 0755))
		require.NoError(t, os.WriteFile(svc.cfg.Get().GetVolumeDir(volumeName), []byte("blocker"), 0644))

		_, err := svc.nodePublishVolumeStaticInlineVolume(
			context.Background(), volumeName, t.TempDir(), "r/m:v1", false, nil,
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "set initial volume status")
	})

	t.Run("final set fails after mount", func(t *testing.T) {
		svc, _ := newNodeService(t)
		digest := "sha256:finalset-fail"
		patches := applyInlineCachePatches(t, svc, digest, nil, nil)
		defer patches.Reset()

		patchMount := gomonkey.ApplyFunc(mounter.Mount, func(_ context.Context, _ mounter.Builder) error {
			return nil
		})
		defer patchMount.Reset()

		volumeName := "pvc-finalset-fail"
		// Force the second Set (Mounted) to fail by patching sm.Set.
		var setCalls int
		patchSet := gomonkey.ApplyMethod(svc.sm, "Set",
			func(sm *status.StatusManager, statusPath string, newStatus status.Status) (*status.Status, error) {
				setCalls++
				if setCalls >= 2 {
					return nil, errors.New("set boom")
				}
				return &newStatus, nil
			})
		defer patchSet.Reset()

		_, err := svc.nodePublishVolumeStaticInlineVolume(
			context.Background(), volumeName, t.TempDir(), "r/m:v1", false, nil,
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "set volume status")
	})
}

// TestNodePublishVolumeStaticInlineVolumeLegacy_Errors covers the error
// branches of the legacy (partial-file) publish path: pull failure, mount
// failure after a successful pull, and the status Get/Set failures at the
// tail of the function.
func TestNodePublishVolumeStaticInlineVolumeLegacy_Errors(t *testing.T) {
	t.Run("pull failure", func(t *testing.T) {
		svc, _ := newNodeService(t)

		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyFunc(auth.GetKeyChainByRef, func(string) (*auth.PassKeyChain, error) {
			return &auth.PassKeyChain{ServerScheme: "https"}, nil
		})
		patches.ApplyFunc(modctlbackend.New, func(string) (modctlbackend.Backend, error) {
			return nil, nil
		})
		svc.worker.newPuller = func(context.Context, *config.PullConfig, *status.Hook, *DiskQuotaChecker) Puller {
			return &fakePuller{pullFunc: func(context.Context, string, string) error {
				return errors.New("pull boom")
			}}
		}

		_, err := svc.nodePublishVolumeStaticInlineVolume(
			context.Background(), "pvc-legacy-pullfail", t.TempDir(),
			"r/m:v1", true, nil,
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "pull model")
	})

	t.Run("mount failure", func(t *testing.T) {
		svc, _ := newNodeService(t)

		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyFunc(auth.GetKeyChainByRef, func(string) (*auth.PassKeyChain, error) {
			return &auth.PassKeyChain{ServerScheme: "https"}, nil
		})
		patches.ApplyFunc(modctlbackend.New, func(string) (modctlbackend.Backend, error) {
			return nil, nil
		})
		svc.worker.newPuller = func(context.Context, *config.PullConfig, *status.Hook, *DiskQuotaChecker) Puller {
			return &legacyPuller{}
		}

		patchMount := gomonkey.ApplyFunc(mounter.Mount, func(_ context.Context, _ mounter.Builder) error {
			return errors.New("bind boom")
		})
		defer patchMount.Reset()

		_, err := svc.nodePublishVolumeStaticInlineVolume(
			context.Background(), "pvc-legacy-mountfail", t.TempDir(),
			"r/m:v1", true, nil,
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "bind mount")
	})

	t.Run("status get failure", func(t *testing.T) {
		svc, _ := newNodeService(t)

		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyFunc(auth.GetKeyChainByRef, func(string) (*auth.PassKeyChain, error) {
			return &auth.PassKeyChain{ServerScheme: "https"}, nil
		})
		patches.ApplyFunc(modctlbackend.New, func(string) (modctlbackend.Backend, error) {
			return nil, nil
		})
		svc.worker.newPuller = func(context.Context, *config.PullConfig, *status.Hook, *DiskQuotaChecker) Puller {
			return &legacyPuller{}
		}
		patchMount := gomonkey.ApplyFunc(mounter.Mount, func(_ context.Context, _ mounter.Builder) error {
			return nil
		})
		defer patchMount.Reset()

		// Force sm.Get to fail right after the mount.
		patchGet := gomonkey.ApplyMethod(svc.sm, "Get",
			func(*status.StatusManager, string) (*status.Status, error) {
				return nil, errors.New("get boom")
			})
		defer patchGet.Reset()

		_, err := svc.nodePublishVolumeStaticInlineVolume(
			context.Background(), "pvc-legacy-getfail", t.TempDir(),
			"r/m:v1", true, nil,
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "get volume status")
	})

}

// TestNodeUnPublishVolumeStaticInlineVolume_FindRefError verifies that when
// the reverse-scan path errors out (e.g. refs root is not a directory), the
// unpublish still proceeds, logs a warning, and successfully cleans up the
// volume directory.
func TestNodeUnPublishVolumeStaticInlineVolume_FindRefError(t *testing.T) {
	svc, _ := newNodeService(t)

	patchUMount := gomonkey.ApplyFunc(mounter.UMount, func(_ context.Context, _ string, _ bool) error {
		return nil
	})
	defer patchUMount.Reset()

	volumeName := "pvc-find-err"
	volumeDir := svc.cfg.Get().GetVolumeDir(volumeName)
	require.NoError(t, os.MkdirAll(volumeDir, 0755))

	// Plant a regular file where the refs root is expected so that
	// FindCacheDigestByRef returns a non-IsNotExist error and the warn
	// branch is exercised.
	refsRoot := svc.cfg.Get().GetCacheRefsRootDir()
	require.NoError(t, os.MkdirAll(filepath.Dir(refsRoot), 0755))
	require.NoError(t, os.WriteFile(refsRoot, []byte("not a dir"), 0644))

	_, err := svc.nodeUnPublishVolumeStaticInlineVolume(context.Background(), volumeName, t.TempDir(), true)
	require.NoError(t, err)

	_, statErr := os.Stat(volumeDir)
	require.True(t, os.IsNotExist(statErr))
}
