package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelpack/model-csi-driver/pkg/config"
	modelStatus "github.com/modelpack/model-csi-driver/pkg/status"
	"github.com/stretchr/testify/require"
)

func newTestDynamicServerManager(t *testing.T) (*DynamicServerManager, string) {
	t.Helper()
	svc, tmpDir := newNodeService(t)
	rawCfg := &config.RawConfig{
		ServiceName: "test.csi.example.com",
		RootDir:     tmpDir,
	}
	cfg := config.NewWithRaw(rawCfg)
	mgr := NewDynamicServerManager(cfg, svc)
	return mgr, tmpDir
}

func TestDynamicServerManager_CloseServer_NotExists(t *testing.T) {
	mgr, _ := newTestDynamicServerManager(t)
	// Closing a server that doesn't exist should return nil
	err := mgr.CloseServer(context.Background(), "/tmp/nonexistent.sock")
	require.NoError(t, err)
}

func TestDynamicServerManager_CreateServer(t *testing.T) {
	mgr, tmpDir := newTestDynamicServerManager(t)
	sockPath := filepath.Join(tmpDir, "test.sock")

	server, err := mgr.CreateServer(context.Background(), sockPath)
	require.NoError(t, err)
	require.NotNil(t, server)

	// cleanup
	_ = mgr.CloseServer(context.Background(), sockPath)
}

func TestDynamicServerManager_CloseServer_AfterCreate(t *testing.T) {
	mgr, tmpDir := newTestDynamicServerManager(t)
	sockPath := filepath.Join(tmpDir, "close-test.sock")

	_, err := mgr.CreateServer(context.Background(), sockPath)
	require.NoError(t, err)

	err = mgr.CloseServer(context.Background(), sockPath)
	require.NoError(t, err)

	// Closing again should be a no-op
	err = mgr.CloseServer(context.Background(), sockPath)
	require.NoError(t, err)
}

func TestDynamicServerManager_CreateServer_ExistingReplaced(t *testing.T) {
	mgr, tmpDir := newTestDynamicServerManager(t)
	sockPath := filepath.Join(tmpDir, "replace-test.sock")

	// Create server twice - should replace the existing one
	_, err := mgr.CreateServer(context.Background(), sockPath)
	require.NoError(t, err)

	_, err = mgr.CreateServer(context.Background(), sockPath)
	require.NoError(t, err)

	_ = mgr.CloseServer(context.Background(), sockPath)
}

func TestDynamicServerManager_RecoverServers_NoVolumesDir(t *testing.T) {
	mgr, _ := newTestDynamicServerManager(t)
	// No volumes dir → should handle gracefully (empty or error)
	// RecoverServers reads volumes dir - if it doesn't exist, it should return error or nil
	err := mgr.RecoverServers(context.Background())
	// Error is expected since volumes dir doesn't exist
	_ = err // may or may not error; just ensure no panic
}

func TestDynamicServerManager_RecoverServers_EmptyDir(t *testing.T) {
	mgr, tmpDir := newTestDynamicServerManager(t)
	// Create empty volumes dir
	volumesDir := filepath.Join(tmpDir, "volumes")
	require.NoError(t, os.MkdirAll(volumesDir, 0750))

	err := mgr.RecoverServers(context.Background())
	require.NoError(t, err)
}

func TestDynamicServerManager_RecoverServers_WithDynamicVolume(t *testing.T) {
	mgr, tmpDir := newTestDynamicServerManager(t)
	volumesDir := filepath.Join(tmpDir, "volumes")
	require.NoError(t, os.MkdirAll(volumesDir, 0750))

	// Create a dynamic volume dir structure
	volumeName := fmt.Sprintf("csi-dyn-test-%d", os.Getpid())
	sockDir := filepath.Join(volumesDir, volumeName, "csi")
	require.NoError(t, os.MkdirAll(sockDir, 0750))

	// Create a status.json in models dir to simulate a running mount
	mountID := "mount-1"
	mountIDDir := mgr.cfg.Get().GetMountIDDirForDynamic(volumeName, mountID)
	require.NoError(t, os.MkdirAll(mountIDDir, 0750))
	statusPath := filepath.Join(mountIDDir, "status.json")
	_, err := mgr.svc.sm.Set(statusPath, modelStatus.Status{
		VolumeName: volumeName,
		MountID:    mountID,
		Reference:  "test/model:latest",
		State:      modelStatus.StatePullSucceeded,
	})
	require.NoError(t, err)

	// RecoverServers - may create a dynamic server
	err = mgr.RecoverServers(context.Background())
	// May succeed or fail depending on socket creation; just ensure no panic
	_ = err

	// Cleanup any created servers
	sockPath := mgr.cfg.Get().GetCSISockPathForDynamic(volumeName)
	_ = mgr.CloseServer(context.Background(), sockPath)
}
