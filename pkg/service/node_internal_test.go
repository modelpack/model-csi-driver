package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/modelpack/model-csi-driver/pkg/mounter"
	modelStatus "github.com/modelpack/model-csi-driver/pkg/status"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// nodeUnPublishVolumeStatic with isMounted=false and non-existent status.json
func TestNodeUnPublishVolumeStatic_NotMounted_NoStatus(t *testing.T) {
	svc, _ := newNodeService(t)
	ctx := context.Background()
	resp, err := svc.nodeUnPublishVolumeStatic(ctx, "pvc-test-vol", "/tmp/target", false)
	require.NoError(t, err)
	require.NotNil(t, resp)
}

// nodeUnPublishVolumeStatic with isMounted=false and existing status.json
func TestNodeUnPublishVolumeStatic_NotMounted_WithStatus(t *testing.T) {
	svc, tmpDir := newNodeService(t)
	ctx := context.Background()
	volumeName := "pvc-unmount-test"
	volumeDir := filepath.Join(tmpDir, "volumes", volumeName)
	require.NoError(t, os.MkdirAll(volumeDir, 0755))
	statusPath := filepath.Join(volumeDir, "status.json")
	_, err := svc.sm.Set(statusPath, modelStatus.Status{
		VolumeName: volumeName,
		Reference:  "test/model:latest",
		State:      modelStatus.StateMounted,
	})
	require.NoError(t, err)

	resp, err := svc.nodeUnPublishVolumeStatic(ctx, volumeName, "/tmp/target", false)
	require.NoError(t, err)
	require.NotNil(t, resp)
}

// nodePublishVolumeStatic with gomonkey mocking mounter.Mount
func TestNodePublishVolumeStatic_MockMount(t *testing.T) {
	svc, tmpDir := newNodeService(t)
	ctx := context.Background()
	volumeName := "pvc-mount-test"
	volumeDir := filepath.Join(tmpDir, "volumes", volumeName)
	require.NoError(t, os.MkdirAll(volumeDir, 0755))
	statusPath := filepath.Join(volumeDir, "status.json")
	_, err := svc.sm.Set(statusPath, modelStatus.Status{
		VolumeName: volumeName,
		Reference:  "test/model:latest",
		State:      modelStatus.StatePullSucceeded,
	})
	require.NoError(t, err)

	// Mock mounter.Mount to return nil
	patch := gomonkey.ApplyFunc(mounter.Mount, func(ctx context.Context, builder mounter.Builder) error {
		return nil
	})
	defer patch.Reset()

	resp, err := svc.nodePublishVolumeStatic(ctx, volumeName, t.TempDir())
	require.NoError(t, err)
	require.NotNil(t, resp)
}

// Test NodePublishVolume via full path with mocked IsMounted
func TestNodePublishVolume_WithMockedMounter(t *testing.T) {
	svc, tmpDir := newNodeService(t)
	ctx := context.Background()
	volumeName := "pvc-publish-test"
	volumeDir := filepath.Join(tmpDir, "volumes", volumeName)
	require.NoError(t, os.MkdirAll(volumeDir, 0755))
	statusPath := filepath.Join(volumeDir, "status.json")
	_, err := svc.sm.Set(statusPath, modelStatus.Status{
		VolumeName: volumeName,
		Reference:  "test/model:latest",
		State:      modelStatus.StatePullSucceeded,
	})
	require.NoError(t, err)

	// Mock IsMounted to return false (no existing mount)
	patchIsMounted := gomonkey.ApplyFunc(mounter.IsMounted, func(ctx context.Context, mountPoint string) (bool, error) {
		return false, nil
	})
	defer patchIsMounted.Reset()

	// Mock EnsureMountPoint to succeed
	patchEnsure := gomonkey.ApplyFunc(mounter.EnsureMountPoint, func(ctx context.Context, mountPoint string) error {
		return nil
	})
	defer patchEnsure.Reset()

	// Mock mounter.Mount to return nil (bind mount)
	patchMount := gomonkey.ApplyFunc(mounter.Mount, func(ctx context.Context, builder mounter.Builder) error {
		return nil
	})
	defer patchMount.Reset()

	targetPath := t.TempDir()
	_, err = svc.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:   volumeName,
		TargetPath: targetPath,
	})
	require.NoError(t, err)
}

// NodeUnpublishVolume with mocked IsMounted
func TestNodeUnpublishVolume_WithMockedMounter(t *testing.T) {
	svc, _ := newNodeService(t)
	ctx := context.Background()

	patchIsMounted := gomonkey.ApplyFunc(mounter.IsMounted, func(ctx context.Context, mountPoint string) (bool, error) {
		return false, nil
	})
	defer patchIsMounted.Reset()

	targetPath := t.TempDir()
	_, err := svc.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "pvc-unmount-mock",
		TargetPath: targetPath,
	})
	require.NoError(t, err)
}
// tokenAuthInterceptor covers the grpc interceptor method
func TestTokenAuthInterceptor(t *testing.T) {
	svc, _ := newNodeService(t)
	ctx := context.Background()

	called := false
	fakeInvoker := grpc.UnaryInvoker(func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		called = true
		return nil
	})

	err := svc.tokenAuthInterceptor(ctx, "/test.Service/Method", nil, nil, nil, fakeInvoker)
	require.NoError(t, err)
	require.True(t, called)
}

// nodeUnPublishVolumeStaticInlineVolume with isMounted=false
func TestNodeUnPublishVolumeStaticInlineVolume_NotMounted(t *testing.T) {
	svc, _ := newNodeService(t)
	ctx := context.Background()
	volumeName := "inline-test-vol"
	targetPath := t.TempDir()

	// Create the volume dir so RemoveAll has something to remove
	volumeDir := svc.cfg.Get().GetVolumeDir(volumeName)
	require.NoError(t, os.MkdirAll(volumeDir, 0755))

	resp, err := svc.nodeUnPublishVolumeStaticInlineVolume(ctx, volumeName, targetPath, false)
	require.NoError(t, err)
	require.NotNil(t, resp)

	// Volume dir should be removed
	_, statErr := os.Stat(volumeDir)
	require.True(t, os.IsNotExist(statErr))
}

// nodeUnPublishVolumeDynamic with isMounted=false
func TestNodeUnPublishVolumeDynamic_NotMounted(t *testing.T) {
	svc, _ := newNodeService(t)
	ctx := context.Background()
	volumeName := "dynamic-test-vol"
	targetPath := t.TempDir()

	// Paths don't exist: IsInSameDevice will error (warning logged), sameDevice=false
	// UMount on sourceCSIDir will be called and its "not mounted"-style error swallowed
	// RemoveAll on non-existent sourceVolumeDir is a no-op
	resp, err := svc.nodeUnPublishVolumeDynamic(ctx, volumeName, targetPath, false)
	require.NoError(t, err)
	require.NotNil(t, resp)
}

// nodePublishVolumeDynamicForRootMount - covers the early mkdir path
// It will attempt to create dirs, create a CSI server, and then try to bind mount
func TestNodePublishVolumeDynamicForRootMount_ServerError(t *testing.T) {
	svc, _ := newNodeService(t)
	// Initialize DynamicServerManager so it's not nil
	svc.DynamicServerManager = NewDynamicServerManager(svc.cfg, svc)
	ctx := context.Background()
	volumeName := "dynamic-pub-vol"
	targetPath := t.TempDir()

	// Mock mounter.Mount to return nil so we can reach the status.json creation
	patchMount := gomonkey.ApplyFunc(mounter.Mount, func(ctx context.Context, builder mounter.Builder) error {
		return nil
	})
	defer patchMount.Reset()

	_, _ = svc.nodePublishVolumeDynamicForRootMount(ctx, volumeName, targetPath)
	// Just ensure no panic; the function will attempt dirs/server creation
}
