package service

import (
	"os"
	"path/filepath"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otelCodes "go.opentelemetry.io/otel/codes"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/modelpack/model-csi-driver/pkg/metrics"
	modelStatus "github.com/modelpack/model-csi-driver/pkg/status"
	"github.com/modelpack/model-csi-driver/pkg/tracing"
	"github.com/pkg/errors"
)

func (s *Service) CreateVolume(
	ctx context.Context,
	req *csi.CreateVolumeRequest) (
	*csi.CreateVolumeResponse, error) {
	spanName := "NodeCreateVolume"
	if s.cfg.IsControllerMode() {
		spanName = "ControllerCreateVolume"
	}
	ctx, span := tracing.Tracer.Start(ctx, spanName)
	defer span.End()
	span.SetAttributes(attribute.String("mode", s.cfg.Mode))

	ctx = logger.NewContext(ctx, "CreateVolume", req.GetName(), "")

	logger.WithContext(ctx).Infof("creating volume with parameters: %v", req.GetParameters())
	var resp *csi.CreateVolumeResponse
	var isStaticVolume bool
	var err error
	start := time.Now()
	if s.cfg.IsControllerMode() {
		resp, err = s.remoteCreateVolume(ctx, req)
		metrics.ControllerOpObserve("create_volume", start, err)
	} else {
		resp, isStaticVolume, err = s.localCreateVolume(ctx, req)
		if isStaticVolume {
			metrics.NodeOpObserve("create_volume", start, err)
		} else {
			metrics.NodeOpObserve("create_dynamic_volume", start, err)
		}
	}
	if err != nil {
		span.SetStatus(otelCodes.Error, "failed to create volume")
		span.RecordError(err)
		logger.WithContext(ctx).WithError(err).Errorf("failed to create volume")
	} else {
		logger.WithContext(ctx).Infof("created volume")
	}
	return resp, err
}

func (s *Service) DeleteVolume(
	ctx context.Context,
	req *csi.DeleteVolumeRequest) (
	*csi.DeleteVolumeResponse, error) {
	ctx, span := tracing.Tracer.Start(ctx, "DeleteVolume")
	defer span.End()
	span.SetAttributes(attribute.String("mode", s.cfg.Mode))

	ctx = logger.NewContext(ctx, "DeleteVolume", req.GetVolumeId(), "")

	logger.WithContext(ctx).Infof("deleting volume")
	var resp *csi.DeleteVolumeResponse
	var isStaticVolume bool
	var err error
	start := time.Now()
	if s.cfg.IsControllerMode() {
		resp, err = s.remoteDeleteVolume(ctx, req)
		metrics.ControllerOpObserve("delete_volume", start, err)
	} else {
		resp, isStaticVolume, err = s.localDeleteVolume(ctx, req)
		if isStaticVolume {
			metrics.NodeOpObserve("delete_volume", start, err)
		} else {
			metrics.NodeOpObserve("delete_dynamic_volume", start, err)
		}
	}

	if err != nil {
		span.SetStatus(otelCodes.Error, "failed to delete volume")
		span.RecordError(err)
		logger.WithContext(ctx).WithError(err).Errorf("failed to delete volume")
		return nil, status.Error(codes.Internal, err.Error())
	} else {
		logger.WithContext(ctx).Infof("deleted volume")
	}

	return resp, nil
}

func (s *Service) getDynamicVolume(ctx context.Context, volumeName, mountID string) (*modelStatus.Status, error) {
	ctx = logger.NewContext(ctx, "GetVolume", volumeName, "")

	modelDir := s.cfg.GetMountIDDirForDynamic(volumeName, mountID)
	statusPath := filepath.Join(modelDir, "status.json")
	status, err := s.sm.Get(statusPath)
	if err != nil {
		logger.WithContext(ctx).WithError(err).Errorf("failed to get volume status")
		return nil, err
	}

	return status, err
}

func (s *Service) GetDynamicVolume(ctx context.Context, volumeName, mountID string) (*modelStatus.Status, error) {
	start := time.Now()
	status, err := s.getDynamicVolume(ctx, volumeName, mountID)
	metrics.NodeOpObserve("get_dynamic_volume", start, err)
	return status, err
}

func (s *Service) listDynamicVolumes(ctx context.Context, volumeName string) ([]modelStatus.Status, error) {
	ctx = logger.NewContext(ctx, "ListVolumes", volumeName, "")

	modelsDir := s.cfg.GetModelsDirForDynamic(volumeName)

	entries, err := os.ReadDir(modelsDir)
	if err != nil {
		logger.WithContext(ctx).WithError(err).Errorf("failed to read models dir")
		return nil, err
	}

	statuses := []modelStatus.Status{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		mountID := entry.Name()
		modelDir := s.cfg.GetMountIDDirForDynamic(volumeName, mountID)
		statusPath := filepath.Join(modelDir, "status.json")
		status, err := s.sm.Get(statusPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			logger.WithContext(ctx).WithError(err).Errorf("failed to get volume status")
			return nil, err
		}

		statuses = append(statuses, *status)
	}

	return statuses, err
}

func (s *Service) ListDynamicVolumes(ctx context.Context, volumeName string) ([]modelStatus.Status, error) {
	start := time.Now()
	statuses, err := s.listDynamicVolumes(ctx, volumeName)
	metrics.NodeOpObserve("list_dynamic_volumes", start, err)
	return statuses, err
}

func (s *Service) ListVolumes(
	ctx context.Context,
	req *csi.ListVolumesRequest) (
	*csi.ListVolumesResponse, error) {
	ctx = logger.NewContext(ctx, "ListVolumes", "", "")

	logger.WithContext(ctx).Infof("listing volumes")
	var resp *csi.ListVolumesResponse
	var err error
	if s.cfg.IsControllerMode() {
		resp, err = s.remoteListVolumes(ctx, req)
	} else {
		return nil, status.Error(codes.Unimplemented, "local list volumes not implemented")
	}

	if err != nil {
		logger.WithContext(ctx).WithError(err).Errorf("failed to list volumes")
		return nil, status.Error(codes.Internal, err.Error())
	} else {
		logger.WithContext(ctx).Infof("listed volumes")
	}

	return resp, nil
}

func (s *Service) ControllerPublishVolume(
	ctx context.Context,
	req *csi.ControllerPublishVolumeRequest) (
	*csi.ControllerPublishVolumeResponse, error) {
	return &csi.ControllerPublishVolumeResponse{}, nil
}

func (s *Service) ControllerUnpublishVolume(
	ctx context.Context,
	req *csi.ControllerUnpublishVolumeRequest) (
	*csi.ControllerUnpublishVolumeResponse, error) {
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (s *Service) ValidateVolumeCapabilities(
	ctx context.Context,
	req *csi.ValidateVolumeCapabilitiesRequest) (
	*csi.ValidateVolumeCapabilitiesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (s *Service) GetCapacity(
	ctx context.Context,
	req *csi.GetCapacityRequest) (
	*csi.GetCapacityResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (s *Service) ControllerGetCapabilities(
	ctx context.Context,
	req *csi.ControllerGetCapabilitiesRequest) (
	*csi.ControllerGetCapabilitiesResponse, error) {
	newCap := func(cap csi.ControllerServiceCapability_RPC_Type) *csi.ControllerServiceCapability {
		return &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: cap,
				},
			},
		}
	}

	var caps []*csi.ControllerServiceCapability
	for _, capability := range []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		// csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
		// csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
		// csi.ControllerServiceCapability_RPC_GET_CAPACITY,
		// csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
	} {
		caps = append(caps, newCap(capability))
	}

	resp := &csi.ControllerGetCapabilitiesResponse{
		Capabilities: caps,
	}

	return resp, nil
}

func (s *Service) CreateSnapshot(
	ctx context.Context,
	req *csi.CreateSnapshotRequest) (
	*csi.CreateSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (s *Service) DeleteSnapshot(
	ctx context.Context,
	req *csi.DeleteSnapshotRequest) (
	*csi.DeleteSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (s *Service) ListSnapshots(
	ctx context.Context,
	req *csi.ListSnapshotsRequest) (
	*csi.ListSnapshotsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (s *Service) ControllerExpandVolume(
	ctx context.Context,
	req *csi.ControllerExpandVolumeRequest) (
	*csi.ControllerExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}
