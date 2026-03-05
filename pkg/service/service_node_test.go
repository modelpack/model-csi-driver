package service

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	grpcStatus "google.golang.org/grpc/status"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	cfg := config.NewWithRaw(&config.RawConfig{
		ServiceName: "test.csi.example.com",
		NodeID:      "test-node-1",
	})
	return &Service{cfg: cfg}
}

// Identity

func TestGetPluginInfo(t *testing.T) {
	svc := newTestService(t)
	resp, err := svc.GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{})
	require.NoError(t, err)
	require.Equal(t, "test.csi.example.com", resp.Name)
	require.Equal(t, VendorVersion, resp.VendorVersion)
}

func TestGetPluginCapabilities(t *testing.T) {
	svc := newTestService(t)
	resp, err := svc.GetPluginCapabilities(context.Background(), &csi.GetPluginCapabilitiesRequest{})
	require.NoError(t, err)
	require.Len(t, resp.Capabilities, 1)
	require.NotNil(t, resp.Capabilities[0].GetService())
	require.Equal(t,
		csi.PluginCapability_Service_CONTROLLER_SERVICE,
		resp.Capabilities[0].GetService().Type,
	)
}

func TestProbe(t *testing.T) {
	svc := newTestService(t)
	resp, err := svc.Probe(context.Background(), &csi.ProbeRequest{})
	require.NoError(t, err)
	require.NotNil(t, resp)
}

// Node stubs

func TestNodeStageVolume(t *testing.T) {
	svc := newTestService(t)
	resp, err := svc.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{})
	require.NoError(t, err)
	require.NotNil(t, resp)
}

func TestNodeUnstageVolume(t *testing.T) {
	svc := newTestService(t)
	resp, err := svc.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{})
	require.NoError(t, err)
	require.NotNil(t, resp)
}

func TestNodeGetCapabilities(t *testing.T) {
	svc := newTestService(t)
	resp, err := svc.NodeGetCapabilities(context.Background(), &csi.NodeGetCapabilitiesRequest{})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Len(t, resp.Capabilities, 1)
}

func TestNodeGetInfo(t *testing.T) {
	svc := newTestService(t)
	resp, err := svc.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	require.NoError(t, err)
	require.Equal(t, "test-node-1", resp.NodeId)
}

func TestNodeGetVolumeStats_Unimplemented(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{})
	require.Error(t, err)
	st, ok := grpcStatus.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.Unimplemented, st.Code())
}

func TestNodeExpandVolume_Unimplemented(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{})
	require.Error(t, err)
	st, ok := grpcStatus.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.Unimplemented, st.Code())
}

// isStaticVolume / isDynamicVolume

func TestIsStaticVolume(t *testing.T) {
	require.True(t, isStaticVolume("pvc-12345"))
	require.True(t, isStaticVolume("pvc-"))
	require.False(t, isStaticVolume("csi-12345"))
	require.False(t, isStaticVolume("vol-12345"))
	require.False(t, isStaticVolume(""))
}

func TestIsDynamicVolume(t *testing.T) {
	require.True(t, isDynamicVolume("csi-abcdef"))
	require.True(t, isDynamicVolume("csi-"))
	require.False(t, isDynamicVolume("pvc-abcdef"))
	require.False(t, isDynamicVolume("vol-abcdef"))
	require.False(t, isDynamicVolume(""))
}
