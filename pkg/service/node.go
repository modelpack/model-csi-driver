package service

import (
	"path/filepath"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otelCodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/pkg/errors"

	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/modelpack/model-csi-driver/pkg/metrics"
	"github.com/modelpack/model-csi-driver/pkg/mounter"
	"github.com/modelpack/model-csi-driver/pkg/tracing"
)

func (s *Service) NodeStageVolume(
	ctx context.Context,
	req *csi.NodeStageVolumeRequest) (
	*csi.NodeStageVolumeResponse, error) {

	return &csi.NodeStageVolumeResponse{}, nil
}

func (s *Service) NodeUnstageVolume(
	ctx context.Context,
	req *csi.NodeUnstageVolumeRequest) (
	*csi.NodeUnstageVolumeResponse, error) {

	return &csi.NodeUnstageVolumeResponse{}, nil
}

func isStaticVolume(volumeID string) bool {
	return strings.HasPrefix(volumeID, "pvc-")
}

func isDynamicVolume(volumeID string) bool {
	return strings.HasPrefix(volumeID, "csi-")
}

func (s *Service) nodePublishVolume(
	ctx context.Context,
	req *csi.NodePublishVolumeRequest) (
	*csi.NodePublishVolumeResponse, bool, error) {

	volumeID := req.GetVolumeId()
	targetPath := req.GetTargetPath()
	volumeAttributes := req.GetVolumeContext()
	if volumeAttributes == nil {
		volumeAttributes = map[string]string{}
	}

	if volumeID == "" {
		return nil, true, status.Error(codes.InvalidArgument, "missing required parameter: volumeId")
	}

	isStaticVolume := isStaticVolume(volumeID)

	if targetPath == "" {
		return nil, isStaticVolume, status.Error(codes.InvalidArgument, "missing required parameter: targetPath")
	}

	parentSpan := trace.SpanFromContext(ctx)
	parentSpan.SetAttributes(attribute.String("volume_name", volumeID))
	parentSpan.SetAttributes(attribute.String("target_path", targetPath))
	parentSpan.SetAttributes(attribute.Bool("static_volume", isStaticVolume))

	isMounted, err := mounter.IsMounted(ctx, targetPath)
	if err != nil {
		return nil, isStaticVolume, status.Error(codes.Internal, errors.Wrap(err, "check if target path is mounted").Error())
	}

	if isMounted {
		logger.WithContext(ctx).Info("target path is already mounted")
		return &csi.NodePublishVolumeResponse{}, isStaticVolume, nil
	}

	if err := mounter.EnsureMountPoint(ctx, targetPath); err != nil {
		return nil, isStaticVolume, status.Error(codes.Internal, errors.Wrap(err, "ensure mount point").Error())
	}

	if isStaticVolume {
		resp, err := s.nodePublishVolumeStatic(ctx, volumeID, targetPath)
		return resp, isStaticVolume, err
	}

	staticInlineModelReference := volumeAttributes[s.cfg.Get().ParameterKeyReference()]
	if staticInlineModelReference != "" {
		logger.WithContext(ctx).Infof("publishing static inline volume: %s", staticInlineModelReference)
		resp, err := s.nodePublishVolumeStaticInlineVolume(ctx, volumeID, targetPath, staticInlineModelReference)
		return resp, isStaticVolume, err
	}

	resp, err := s.nodePublishVolumeDynamicForRootMount(ctx, volumeID, targetPath)
	return resp, isStaticVolume, err
}

func (s *Service) NodePublishVolume(
	ctx context.Context,
	req *csi.NodePublishVolumeRequest) (
	*csi.NodePublishVolumeResponse, error) {
	ctx, span := tracing.Tracer.Start(ctx, "NodePublishVolume")
	defer span.End()

	volumeID := req.GetVolumeId()
	targetPath := req.GetTargetPath()

	ctx = logger.NewContext(ctx, "NodePublishVolume", volumeID, targetPath)

	logger.WithContext(ctx).Infof("publishing node volume")
	start := time.Now()
	resp, isStaticVolume, err := s.nodePublishVolume(ctx, req)
	if err != nil {
		span.SetStatus(otelCodes.Error, "failed to publish node volume")
		span.RecordError(err)
		logger.WithContext(ctx).Errorf("failed to publish node volume: %v", err)
		return nil, err
	}
	if isStaticVolume {
		metrics.NodeOpObserve("publish_volume", start, err)
	} else {
		metrics.NodeOpObserve("publish_dynamic_volume", start, err)
	}
	logger.WithContext(ctx).Infof("published node volume")

	return resp, nil
}

func (s *Service) nodeUnpublishVolume(
	ctx context.Context,
	req *csi.NodeUnpublishVolumeRequest) (
	*csi.NodeUnpublishVolumeResponse, bool, error) {
	volumeID := req.GetVolumeId()
	targetPath := req.GetTargetPath()

	if volumeID == "" {
		return nil, true, status.Error(codes.InvalidArgument, "missing required parameter: volumeId")
	}

	isStaticVolume := isStaticVolume(volumeID)

	if targetPath == "" {
		return nil, isStaticVolume, status.Error(codes.InvalidArgument, "missing required parameter: targetPath")
	}

	parentSpan := trace.SpanFromContext(ctx)
	parentSpan.SetAttributes(attribute.String("volume_name", volumeID))
	parentSpan.SetAttributes(attribute.String("target_path", targetPath))
	parentSpan.SetAttributes(attribute.Bool("static_volume", isStaticVolume))

	isMounted, err := mounter.IsMounted(ctx, targetPath)
	if err != nil {
		return nil, isStaticVolume, status.Error(codes.Internal, errors.Wrap(err, "check if target path is mounted").Error())
	}

	if !isMounted {
		logger.WithContext(ctx).Infof("target path is already umounted")
		return &csi.NodeUnpublishVolumeResponse{}, isStaticVolume, nil
	}

	if isStaticVolume {
		resp, err := s.nodeUnPublishVolumeStatic(ctx, volumeID, targetPath)
		return resp, isStaticVolume, err
	}

	statusPath := filepath.Join(s.cfg.Get().GetVolumeDir(volumeID), "status.json")
	volumeStatus, err := s.sm.Get(statusPath)
	if err == nil && volumeStatus != nil && volumeStatus.Inline {
		logger.WithContext(ctx).Infof("unpublishing static inline volume: %s", volumeStatus.Reference)
		resp, err := s.nodeUnPublishVolumeStaticInlineVolume(ctx, volumeID, targetPath)
		return resp, isStaticVolume, err
	}

	resp, err := s.nodeUnPublishVolumeDynamic(ctx, volumeID, targetPath)
	return resp, isStaticVolume, err
}

func (s *Service) NodeUnpublishVolume(
	ctx context.Context,
	req *csi.NodeUnpublishVolumeRequest) (
	*csi.NodeUnpublishVolumeResponse, error) {
	ctx, span := tracing.Tracer.Start(ctx, "NodeUnpublishVolume")
	defer span.End()

	volumeID := req.GetVolumeId()
	targetPath := req.GetTargetPath()

	ctx = logger.NewContext(ctx, "NodeUnpublishVolume", volumeID, targetPath)

	logger.WithContext(ctx).Infof("unpublishing node volume")
	start := time.Now()
	resp, isStaticVolume, err := s.nodeUnpublishVolume(ctx, req)
	if isStaticVolume {
		metrics.NodeOpObserve("unpublish_volume", start, err)
	} else {
		metrics.NodeOpObserve("unpublish_dynamic_volume", start, err)
	}
	if err != nil {
		span.SetStatus(otelCodes.Error, "failed to unpublish node volume")
		span.RecordError(err)
		logger.WithContext(ctx).Errorf("failed to unpublish node volume: %v", err)
		return nil, err
	}
	logger.WithContext(ctx).Infof("unpublished node volume")

	return resp, nil
}

func (s *Service) NodeGetVolumeStats(
	ctx context.Context,
	req *csi.NodeGetVolumeStatsRequest) (
	*csi.NodeGetVolumeStatsResponse, error) {

	return nil, status.Error(codes.Unimplemented, "")
}

func (s *Service) NodeExpandVolume(
	ctx context.Context,
	req *csi.NodeExpandVolumeRequest) (
	*csi.NodeExpandVolumeResponse, error) {

	return nil, status.Error(codes.Unimplemented, "")
}

func (s *Service) NodeGetCapabilities(
	ctx context.Context,
	req *csi.NodeGetCapabilitiesRequest) (
	*csi.NodeGetCapabilitiesResponse, error) {

	nscap := &csi.NodeServiceCapability{
		Type: &csi.NodeServiceCapability_Rpc{
			Rpc: &csi.NodeServiceCapability_RPC{
				Type: csi.NodeServiceCapability_RPC_UNKNOWN,
			},
		},
	}

	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			nscap,
		},
	}, nil
}

func (s *Service) NodeGetInfo(
	ctx context.Context,
	req *csi.NodeGetInfoRequest) (
	*csi.NodeGetInfoResponse, error) {

	return &csi.NodeGetInfoResponse{
		NodeId: s.cfg.Get().NodeID,
	}, nil
}
