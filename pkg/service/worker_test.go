package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/status"
	"github.com/stretchr/testify/require"
)

// ─── ContextMap ───────────────────────────────────────────────────────────────

func TestContextMap_SetAndGet(t *testing.T) {
	cm := NewContextMap()

	var cancel context.CancelFunc
	_, cancel = context.WithCancel(context.Background())

	cm.Set("key1", &cancel)
	got := cm.Get("key1")
	require.NotNil(t, got)
}

func TestContextMap_DeleteByNil(t *testing.T) {
	cm := NewContextMap()

	var cancel context.CancelFunc
	_, cancel = context.WithCancel(context.Background())
	cm.Set("key1", &cancel)

	// setting nil deletes the entry
	cm.Set("key1", nil)
	require.Nil(t, cm.Get("key1"))
}

func TestContextMap_GetMissing(t *testing.T) {
	cm := NewContextMap()
	require.Nil(t, cm.Get("nonexistent"))
}

// ─── Worker ───────────────────────────────────────────────────────────────────

func TestNewWorker(t *testing.T) {
	tmpDir := t.TempDir()
	rawCfg := &config.RawConfig{ServiceName: "test", RootDir: tmpDir}
	cfg := config.NewWithRaw(rawCfg)
	sm, err := status.NewStatusManager()
	require.NoError(t, err)

	worker, err := NewWorker(cfg, sm)
	require.NoError(t, err)
	require.NotNil(t, worker)
}

// ─── isModelExisted ───────────────────────────────────────────────────────────

func TestIsModelExisted_EmptyDir(t *testing.T) {
	tmpDir := t.TempDir()
	rawCfg := &config.RawConfig{ServiceName: "test", RootDir: tmpDir}
	cfg := config.NewWithRaw(rawCfg)
	sm, err := status.NewStatusManager()
	require.NoError(t, err)

	worker, err := NewWorker(cfg, sm)
	require.NoError(t, err)

	// volumes dir doesn't exist yet, should return false without error.
	exists := worker.isModelExisted(context.Background(), "registry/model:v1")
	require.False(t, exists)
}

func TestIsModelExisted_StaticVolumeMatch(t *testing.T) {
	tmpDir := t.TempDir()
	rawCfg := &config.RawConfig{ServiceName: "test", RootDir: tmpDir}
	cfg := config.NewWithRaw(rawCfg)
	sm, err := status.NewStatusManager()
	require.NoError(t, err)

	worker, err := NewWorker(cfg, sm)
	require.NoError(t, err)

	// Create a static volume with a matching reference and model directory.
	volumeName := "pvc-static-vol-1"
	volumeDir := cfg.Get().GetVolumeDir(volumeName)
	modelDir := cfg.Get().GetModelDir(volumeName)
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	statusPath := filepath.Join(volumeDir, "status.json")
	_, err = sm.Set(statusPath, status.Status{
		VolumeName: volumeName,
		Reference:  "registry/model:v1",
		State:      status.StatePullSucceeded,
	})
	require.NoError(t, err)

	exists := worker.isModelExisted(context.Background(), "registry/model:v1")
	require.True(t, exists)
}

func TestIsModelExisted_StaticVolume_NoMatch(t *testing.T) {
	tmpDir := t.TempDir()
	rawCfg := &config.RawConfig{ServiceName: "test", RootDir: tmpDir}
	cfg := config.NewWithRaw(rawCfg)
	sm, err := status.NewStatusManager()
	require.NoError(t, err)

	worker, err := NewWorker(cfg, sm)
	require.NoError(t, err)

	volumeName := "pvc-static-vol-2"
	volumeDir := cfg.Get().GetVolumeDir(volumeName)
	modelDir := cfg.Get().GetModelDir(volumeName)
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	statusPath := filepath.Join(volumeDir, "status.json")
	_, err = sm.Set(statusPath, status.Status{
		VolumeName: volumeName,
		Reference:  "registry/other-model:v2",
		State:      status.StatePullSucceeded,
	})
	require.NoError(t, err)

	// Looking for a different reference.
	exists := worker.isModelExisted(context.Background(), "registry/model:v1")
	require.False(t, exists)
}

func TestIsModelExisted_DynamicVolume(t *testing.T) {
	tmpDir := t.TempDir()
	rawCfg := &config.RawConfig{ServiceName: "test", RootDir: tmpDir}
	cfg := config.NewWithRaw(rawCfg)
	sm, err := status.NewStatusManager()
	require.NoError(t, err)

	worker, err := NewWorker(cfg, sm)
	require.NoError(t, err)

	// Dynamic volume: csi-<id>/models/<mountID>/model
	volumeName := "csi-dyn-vol-1"
	mountID := "mount-abc"
	modelDir := cfg.Get().GetModelDirForDynamic(volumeName, mountID)
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	mountIDDir := cfg.Get().GetMountIDDirForDynamic(volumeName, mountID)
	statusPath := filepath.Join(mountIDDir, "status.json")
	_, err = sm.Set(statusPath, status.Status{
		Reference: "registry/model:dyn",
		State:     status.StatePullSucceeded,
	})
	require.NoError(t, err)

	exists := worker.isModelExisted(context.Background(), "registry/model:dyn")
	require.True(t, exists)
}

// ─── DeleteModel ──────────────────────────────────────────────────────────────

func TestDeleteModel_NonExistentDir(t *testing.T) {
	tmpDir := t.TempDir()
	rawCfg := &config.RawConfig{ServiceName: "test", RootDir: tmpDir}
	cfg := config.NewWithRaw(rawCfg)
	sm, err := status.NewStatusManager()
	require.NoError(t, err)

	worker, err := NewWorker(cfg, sm)
	require.NoError(t, err)

	// DeleteModel on a non-existent dir should succeed (RemoveAll is idempotent).
	err = worker.DeleteModel(context.Background(), true, "pvc-nonexistent", "")
	require.NoError(t, err)
}

func TestDeleteModel_ExistingDir(t *testing.T) {
	tmpDir := t.TempDir()
	rawCfg := &config.RawConfig{ServiceName: "test", RootDir: tmpDir}
	cfg := config.NewWithRaw(rawCfg)
	sm, err := status.NewStatusManager()
	require.NoError(t, err)

	worker, err := NewWorker(cfg, sm)
	require.NoError(t, err)

	volumeName := "pvc-del-test"
	volumeDir := cfg.Get().GetVolumeDir(volumeName)
	require.NoError(t, os.MkdirAll(volumeDir, 0755))

	err = worker.DeleteModel(context.Background(), true, volumeName, "")
	require.NoError(t, err)

	_, statErr := os.Stat(volumeDir)
	require.True(t, os.IsNotExist(statErr))
}
