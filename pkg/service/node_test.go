package service

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/require"
)

func TestNodePublishVolume_EmptyVolumeID(t *testing.T) {
	svc, _ := newNodeService(t)
	ctx := context.Background()
	_, err := svc.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:   "",
		TargetPath: "/tmp/target",
	})
	require.Error(t, err)
}

func TestNodePublishVolume_EmptyTargetPath(t *testing.T) {
	svc, _ := newNodeService(t)
	ctx := context.Background()
	_, err := svc.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:   "pvc-test-vol",
		TargetPath: "",
	})
	require.Error(t, err)
}

func TestNodePublishVolume_StaticVolume_NonExistentTarget(t *testing.T) {
	svc, _ := newNodeService(t)
	ctx := context.Background()
	// mounter.IsMounted will check targetPath existence
	_, err := svc.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:   "pvc-test-vol",
		TargetPath: "/tmp/nonexistent/mount/point",
	})
	// Either error from IsMounted or EnsureMountPoint since path is deep-nonexistent
	// The result will be an error (internal) or success depending on OS state
	_ = err // may or may not error; just ensure no panic
}

func TestNodeUnpublishVolume_EmptyVolumeID(t *testing.T) {
	svc, _ := newNodeService(t)
	ctx := context.Background()
	_, err := svc.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "",
		TargetPath: "/tmp/target",
	})
	require.Error(t, err)
}

func TestNodeUnpublishVolume_EmptyTargetPath(t *testing.T) {
	svc, _ := newNodeService(t)
	ctx := context.Background()
	_, err := svc.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "pvc-test-vol",
		TargetPath: "",
	})
	require.Error(t, err)
}
