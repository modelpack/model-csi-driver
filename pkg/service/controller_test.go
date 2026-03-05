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
	"google.golang.org/grpc/codes"
	grpcStatus "google.golang.org/grpc/status"
)

// newNodeService creates a Service wired up for node-mode testing.
func newNodeService(t *testing.T) (*Service, string) {
	t.Helper()
	tmpDir := t.TempDir()
	rawCfg := &config.RawConfig{
		ServiceName: "test.csi.example.com",
		NodeID:      "test-node-1",
		RootDir:     tmpDir,
	}
	cfg := config.NewWithRaw(rawCfg)
	sm, err := status.NewStatusManager()
	require.NoError(t, err)
	worker, err := NewWorker(cfg, sm)
	require.NoError(t, err)
	return &Service{cfg: cfg, sm: sm, worker: worker}, tmpDir
}

// ─── Simple stub controller methods ────────────────────────────────────────────

func TestControllerPublishVolume(t *testing.T) {
	svc := newTestService(t)
	resp, err := svc.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{})
	require.NoError(t, err)
	require.NotNil(t, resp)
}

func TestControllerUnpublishVolume(t *testing.T) {
	svc := newTestService(t)
	resp, err := svc.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{})
	require.NoError(t, err)
	require.NotNil(t, resp)
}

func TestValidateVolumeCapabilities_Unimplemented(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{})
	require.Error(t, err)
	st, _ := grpcStatus.FromError(err)
	require.Equal(t, codes.Unimplemented, st.Code())
}

func TestGetCapacity_Unimplemented(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.GetCapacity(context.Background(), &csi.GetCapacityRequest{})
	require.Error(t, err)
	st, _ := grpcStatus.FromError(err)
	require.Equal(t, codes.Unimplemented, st.Code())
}

func TestControllerGetCapabilities(t *testing.T) {
	svc := newTestService(t)
	resp, err := svc.ControllerGetCapabilities(context.Background(), &csi.ControllerGetCapabilitiesRequest{})
	require.NoError(t, err)
	require.NotEmpty(t, resp.Capabilities)
}

func TestCreateSnapshot_Unimplemented(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{})
	require.Error(t, err)
	st, _ := grpcStatus.FromError(err)
	require.Equal(t, codes.Unimplemented, st.Code())
}

func TestDeleteSnapshot_Unimplemented(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{})
	require.Error(t, err)
	st, _ := grpcStatus.FromError(err)
	require.Equal(t, codes.Unimplemented, st.Code())
}

func TestListSnapshots_Unimplemented(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{})
	require.Error(t, err)
	st, _ := grpcStatus.FromError(err)
	require.Equal(t, codes.Unimplemented, st.Code())
}

func TestControllerExpandVolume_Unimplemented(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{})
	require.Error(t, err)
	st, _ := grpcStatus.FromError(err)
	require.Equal(t, codes.Unimplemented, st.Code())
}

// ─── getDynamicVolume / ListDynamicVolumes ─────────────────────────────────────

func TestGetDynamicVolume_NotFound(t *testing.T) {
	svc, _ := newNodeService(t)
	_, err := svc.GetDynamicVolume(context.Background(), "csi-vol", "mount-1")
	require.Error(t, err)
}

func TestGetDynamicVolume_Found(t *testing.T) {
	svc, tmpDir := newNodeService(t)

	volumeName := "csi-vol-1"
	mountID := "mount-abc"
	_ = tmpDir

	mountIDDir := svc.cfg.Get().GetMountIDDirForDynamic(volumeName, mountID)
	require.NoError(t, os.MkdirAll(mountIDDir, 0755))
	statusPath := filepath.Join(mountIDDir, "status.json")
	_, err := svc.sm.Set(statusPath, status.Status{
		Reference: "registry/model:v1",
		State:     status.StatePullSucceeded,
	})
	require.NoError(t, err)

	st, err := svc.GetDynamicVolume(context.Background(), volumeName, mountID)
	require.NoError(t, err)
	require.Equal(t, "registry/model:v1", st.Reference)
}

func TestListDynamicVolumes_EmptyDir(t *testing.T) {
	svc, _ := newNodeService(t)

	// Non-existent directory should return error.
	_, err := svc.ListDynamicVolumes(context.Background(), "csi-vol-nonexistent")
	require.Error(t, err)
}

func TestListDynamicVolumes_WithItems(t *testing.T) {
	svc, _ := newNodeService(t)

	volumeName := "csi-list-vol"

	// Create two mounts.
	for _, mountID := range []string{"mount-1", "mount-2"} {
		mountIDDir := svc.cfg.Get().GetMountIDDirForDynamic(volumeName, mountID)
		require.NoError(t, os.MkdirAll(mountIDDir, 0755))
		statusPath := filepath.Join(mountIDDir, "status.json")
		_, err := svc.sm.Set(statusPath, status.Status{
			MountID:   mountID,
			Reference: "reg/model:v1",
			State:     status.StatePullSucceeded,
		})
		require.NoError(t, err)
	}

	statuses, err := svc.ListDynamicVolumes(context.Background(), volumeName)
	require.NoError(t, err)
	require.Len(t, statuses, 2)
}

// ─── localCreateVolume validation ──────────────────────────────────────────────

func TestLocalCreateVolume_MissingVolumeName(t *testing.T) {
	svc, _ := newNodeService(t)
	_, _, err := svc.localCreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "",
	})
	require.Error(t, err)
	st, _ := grpcStatus.FromError(err)
	require.Equal(t, codes.InvalidArgument, st.Code())
}

func TestLocalCreateVolume_MissingModelType(t *testing.T) {
	svc, _ := newNodeService(t)
	_, _, err := svc.localCreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "pvc-test",
		Parameters: map[string]string{},
	})
	require.Error(t, err)
	st, _ := grpcStatus.FromError(err)
	require.Equal(t, codes.InvalidArgument, st.Code())
}

func TestLocalCreateVolume_MissingReference(t *testing.T) {
	svc, _ := newNodeService(t)
	_, _, err := svc.localCreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-test",
		Parameters: map[string]string{
			svc.cfg.Get().ParameterKeyType(): "image",
		},
	})
	require.Error(t, err)
	st, _ := grpcStatus.FromError(err)
	require.Equal(t, codes.InvalidArgument, st.Code())
}

func TestLocalCreateVolume_UnsupportedModelType(t *testing.T) {
	svc, _ := newNodeService(t)
	_, _, err := svc.localCreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-test",
		Parameters: map[string]string{
			svc.cfg.Get().ParameterKeyType():      "oci",
			svc.cfg.Get().ParameterKeyReference(): "registry/model:v1",
		},
	})
	require.Error(t, err)
	st, _ := grpcStatus.FromError(err)
	require.Equal(t, codes.InvalidArgument, st.Code())
}

func TestLocalCreateVolume_InvalidCheckDiskQuota(t *testing.T) {
	svc, _ := newNodeService(t)
	_, _, err := svc.localCreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-test",
		Parameters: map[string]string{
			svc.cfg.Get().ParameterKeyType():           "image",
			svc.cfg.Get().ParameterKeyReference():      "registry/model:v1",
			svc.cfg.Get().ParameterKeyCheckDiskQuota(): "invalid",
		},
	})
	require.Error(t, err)
	st, _ := grpcStatus.FromError(err)
	require.Equal(t, codes.InvalidArgument, st.Code())
}

func TestLocalCreateVolume_InvalidExcludeModelWeights(t *testing.T) {
	svc, _ := newNodeService(t)
	_, _, err := svc.localCreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-test",
		Parameters: map[string]string{
			svc.cfg.Get().ParameterKeyType():                 "image",
			svc.cfg.Get().ParameterKeyReference():            "registry/model:v1",
			svc.cfg.Get().ParameterKeyExcludeModelWeights():  "notabool",
		},
	})
	require.Error(t, err)
	st, _ := grpcStatus.FromError(err)
	require.Equal(t, codes.InvalidArgument, st.Code())
}

func TestLocalCreateVolume_InvalidExcludeFilePatterns(t *testing.T) {
	svc, _ := newNodeService(t)
	_, _, err := svc.localCreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-test",
		Parameters: map[string]string{
			svc.cfg.Get().ParameterKeyType():                "image",
			svc.cfg.Get().ParameterKeyReference():           "registry/model:v1",
			svc.cfg.Get().ParameterKeyExcludeFilePatterns(): `not-valid-json`,
		},
	})
	require.Error(t, err)
	st, _ := grpcStatus.FromError(err)
	require.Equal(t, codes.InvalidArgument, st.Code())
}

// ─── localDeleteVolume validation ──────────────────────────────────────────────

func TestLocalDeleteVolume_EmptyVolumeID(t *testing.T) {
	svc, _ := newNodeService(t)
	_, _, err := svc.localDeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: ""})
	require.Error(t, err)
	st, _ := grpcStatus.FromError(err)
	require.Equal(t, codes.InvalidArgument, st.Code())
}

func TestLocalDeleteVolume_StaticVolume_NonExistent(t *testing.T) {
	svc, _ := newNodeService(t)
	resp, _, err := svc.localDeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "pvc-nonexistent"})
	require.NoError(t, err)
	require.NotNil(t, resp)
}

func TestLocalDeleteVolume_DynamicVolume_NonExistent(t *testing.T) {
	svc, _ := newNodeService(t)
	resp, _, err := svc.localDeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: "csi-vol/mount-abc",
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
}

func TestLocalDeleteVolume_InvalidFormat(t *testing.T) {
	svc, _ := newNodeService(t)
	_, _, err := svc.localDeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: "a/b/c",
	})
	require.Error(t, err)
	st, _ := grpcStatus.FromError(err)
	require.Equal(t, codes.InvalidArgument, st.Code())
}

// ─── StatusManager helper ────────────────────────────────────────────────────

func TestService_StatusManager(t *testing.T) {
	svc, _ := newNodeService(t)
	require.NotNil(t, svc.StatusManager())
}

// --- CreateVolume / DeleteVolume / ListVolumes (controller.go) ---

func TestCreateVolume_NodeMode_MissingParams(t *testing.T) {
	svc, _ := newNodeService(t)
	ctx := context.Background()
	// Missing type parameter – localCreateVolume should return error
	_, err := svc.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:       "pvc-test",
		Parameters: map[string]string{},
	})
	require.Error(t, err)
}

func TestCreateVolume_NodeMode_InvalidType(t *testing.T) {
	svc, _ := newNodeService(t)
	ctx := context.Background()
	_, err := svc.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name: "pvc-test",
		Parameters: map[string]string{
			svc.cfg.Get().ParameterKeyType():      "unsupported",
			svc.cfg.Get().ParameterKeyReference(): "test/model:latest",
		},
	})
	require.Error(t, err)
}

func TestDeleteVolume_NodeMode_EmptyID(t *testing.T) {
	svc, _ := newNodeService(t)
	ctx := context.Background()
	_, err := svc.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: ""})
	require.Error(t, err)
}

func TestDeleteVolume_NodeMode_StaticVolume(t *testing.T) {
	svc, tmpDir := newNodeService(t)
	ctx := context.Background()
	volDir := filepath.Join(tmpDir, "volumes", "pvc-del-static")
	require.NoError(t, os.MkdirAll(volDir, 0750))
	_, err := svc.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: "pvc-del-static"})
	require.NoError(t, err)
}

func TestListVolumes_NodeMode(t *testing.T) {
	svc, _ := newNodeService(t)
	ctx := context.Background()
	// Node mode always returns Unimplemented for ListVolumes
	_, err := svc.ListVolumes(ctx, &csi.ListVolumesRequest{})
	require.Error(t, err)
}

// --- NewDynamicServerManager ---

func TestNewDynamicServerManager(t *testing.T) {
	svc, _ := newNodeService(t)
	mgr := NewDynamicServerManager(svc.cfg, svc)
	require.NotNil(t, mgr)
}

