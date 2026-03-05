package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/status"
	"github.com/stretchr/testify/require"
)

func TestNewCacheManager(t *testing.T) {
	tmpDir := t.TempDir()
	rawCfg := &config.RawConfig{
		ServiceName: "test.csi.extra.com",
		RootDir:     tmpDir,
	}
	cfg := config.NewWithRaw(rawCfg)
	sm, err := status.NewStatusManager()
	require.NoError(t, err)

	// Do not modify CacheScanInterval here: the background goroutine started by
	// NewCacheManager reads it concurrently, so writing and then restoring the
	// global would cause a data race under -race.
	cm, err := NewCacheManager(cfg, sm)
	require.NoError(t, err)
	require.NotNil(t, cm)
}

func TestCacheManager_getCacheSize(t *testing.T) {
	tmpDir := t.TempDir()
	rawCfg := &config.RawConfig{
		ServiceName: "test.csi.extra.com",
		RootDir:     tmpDir,
	}
	cfg := config.NewWithRaw(rawCfg)
	sm, err := status.NewStatusManager()
	require.NoError(t, err)

	cm := &CacheManager{cfg: cfg, sm: sm}
	size, err := cm.getCacheSize()
	require.NoError(t, err)
	require.GreaterOrEqual(t, size, int64(0))
}

func TestCacheManager_Scan_EmptyDir(t *testing.T) {
	tmpDir := t.TempDir()
	rawCfg := &config.RawConfig{
		ServiceName: "test.csi.extra.com",
		RootDir:     tmpDir,
	}
	cfg := config.NewWithRaw(rawCfg)
	sm, err := status.NewStatusManager()
	require.NoError(t, err)

	cm := &CacheManager{cfg: cfg, sm: sm}
	err = cm.Scan()
	require.NoError(t, err)
}

// --- localListVolumes ---

func TestLocalListVolumes_EmptyVolumesDir(t *testing.T) {
	svc, tmpDir := newNodeService(t)
	ctx := context.Background()
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, "volumes"), 0750))
	_, err := svc.localListVolumes(ctx, &csi.ListVolumesRequest{})
	require.NoError(t, err)
}

func TestLocalListVolumes_NoVolumesDir(t *testing.T) {
	svc, _ := newNodeService(t)
	ctx := context.Background()
	_, err := svc.localListVolumes(ctx, &csi.ListVolumesRequest{})
	require.Error(t, err)
}

// --- localCreateVolume dynamic path ---

func TestLocalCreateVolume_DynamicPath_VolumeDirNotExist(t *testing.T) {
	svc, _ := newNodeService(t)
	ctx := context.Background()
	_, _, err := svc.localCreateVolume(ctx, &csi.CreateVolumeRequest{
		Name: "csi-vol",
		Parameters: map[string]string{
			svc.cfg.Get().ParameterKeyType():      "image",
			svc.cfg.Get().ParameterKeyReference(): "test/model:latest",
			svc.cfg.Get().ParameterKeyMountID():   "mount-1",
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "volume directory does not exist")
}
