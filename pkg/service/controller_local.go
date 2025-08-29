package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/modelpack/model-csi-driver/pkg/tracing"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel/attribute"
	otelCodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Service) localCreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, bool, error) {
	volumeName := req.GetName()
	parameters := req.GetParameters()

	if parameters == nil {
		parameters = map[string]string{}
	}

	modelType := strings.TrimSpace(parameters[s.cfg.ParameterKeyType()])
	modelReference := strings.TrimSpace(parameters[s.cfg.ParameterKeyReference()])
	mountID := strings.TrimSpace(parameters[s.cfg.ParameterKeyMountID()])
	checkDiskQuotaParam := strings.TrimSpace(parameters[s.cfg.ParameterKeyCheckDiskQuota()])
	isStaticVolume := mountID == ""

	if volumeName == "" {
		return nil, isStaticVolume, status.Error(codes.InvalidArgument, "missing required parameter: volumeName")
	}

	if modelType == "" {
		return nil, isStaticVolume, status.Errorf(codes.InvalidArgument, "missing required parameter: %s", s.cfg.ParameterKeyType())
	}

	if modelReference == "" {
		return nil, isStaticVolume, status.Errorf(codes.InvalidArgument, "missing required parameter: %s", s.cfg.ParameterKeyReference())
	}

	if modelType != "image" {
		return nil, isStaticVolume, status.Error(codes.InvalidArgument, fmt.Sprintf("unsupported model type: %s", modelType))
	}
	checkDiskQuota := false
	if checkDiskQuotaParam != "" {
		var err error
		checkDiskQuota, err = strconv.ParseBool(checkDiskQuotaParam)
		if err != nil {
			return nil, isStaticVolume, status.Errorf(codes.InvalidArgument, "invalid parameter:%s: %v", s.cfg.ParameterKeyCheckDiskQuota(), err)
		}
	}

	parentSpan := trace.SpanFromContext(ctx)
	parentSpan.SetAttributes(attribute.String("volume_name", volumeName))
	parentSpan.SetAttributes(attribute.String("reference", modelReference))
	parentSpan.SetAttributes(attribute.Bool("static_volume", isStaticVolume))

	if isStaticVolume {
		modelDir := s.cfg.GetModelDir(volumeName)
		startedAt := time.Now()
		ctx, span := tracing.Tracer.Start(ctx, "PullModel")
		span.SetAttributes(attribute.String("model_dir", modelDir))
		if err := s.worker.PullModel(ctx, isStaticVolume, volumeName, "", modelReference, modelDir, checkDiskQuota); err != nil {
			span.SetStatus(otelCodes.Error, "failed to pull model")
			span.RecordError(err)
			span.End()
			if errors.Is(err, syscall.ENOSPC) {
				return nil, isStaticVolume, status.Error(codes.ResourceExhausted, errors.Wrap(err, "pull model for static volume").Error())
			}
			return nil, isStaticVolume, status.Error(codes.Internal, errors.Wrap(err, "pull model").Error())
		}
		span.End()
		duration := time.Since(startedAt)
		logger.WithContext(ctx).Infof("pulled model: %s %s", modelReference, duration)

		return &csi.CreateVolumeResponse{
			Volume: &csi.Volume{
				VolumeId:      volumeName,
				VolumeContext: map[string]string{},
			},
		}, isStaticVolume, nil
	}

	volumeDir := s.cfg.GetVolumeDirForDynamic(volumeName)
	if _, err := os.Stat(volumeDir); err != nil {
		if os.IsNotExist(err) {
			return nil, isStaticVolume, status.Error(codes.Internal, fmt.Sprintf("volume directory does not exist: %s", volumeDir))
		}
		return nil, isStaticVolume, status.Error(codes.Internal, errors.Wrapf(err, "stat volume directory: %s", volumeDir).Error())
	}

	modelDir := s.cfg.GetModelDirForDynamic(volumeName, mountID)
	startedAt := time.Now()
	ctx, span := tracing.Tracer.Start(ctx, "PullModel")
	span.SetAttributes(attribute.String("model_dir", modelDir))
	if err := s.worker.PullModel(ctx, isStaticVolume, volumeName, mountID, modelReference, modelDir, checkDiskQuota); err != nil {
		span.SetStatus(otelCodes.Error, "failed to pull model")
		span.RecordError(err)
		span.End()
		if errors.Is(err, syscall.ENOSPC) {
			return nil, isStaticVolume, status.Error(codes.ResourceExhausted, errors.Wrap(err, "pull model for dynamic volume").Error())
		}
		return nil, isStaticVolume, status.Error(codes.Internal, errors.Wrap(err, "pull model for dynamic volume").Error())
	}
	span.End()
	duration := time.Since(startedAt)
	logger.WithContext(ctx).Infof("pulled model: %s, mount id: %s %s", modelReference, mountID, duration)
	volumeID := fmt.Sprintf("%s/%s", volumeName, mountID)

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeID,
			VolumeContext: map[string]string{},
		},
	}, isStaticVolume, nil
}

func (s *Service) localDeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, bool, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, true, status.Error(codes.InvalidArgument, "missing required parameter: volumeID")
	}

	volumeIDs := strings.Split(volumeID, "/")
	isStaticVolume := len(volumeIDs) == 1

	parentSpan := trace.SpanFromContext(ctx)
	parentSpan.SetAttributes(attribute.Bool("static_volume", isStaticVolume))

	ctx, span := tracing.Tracer.Start(ctx, "DeleteModel")
	defer span.End()
	if isStaticVolume {
		parentSpan.SetAttributes(attribute.String("volume_name", volumeID))
		err := s.worker.DeleteModel(ctx, isStaticVolume, volumeID, "")
		if err != nil {
			span.SetStatus(otelCodes.Error, "failed to delete model")
			span.RecordError(err)
			return nil, isStaticVolume, status.Error(codes.Internal, errors.Wrap(err, "delete model").Error())
		}
		return &csi.DeleteVolumeResponse{}, isStaticVolume, nil
	} else if len(volumeIDs) == 2 {
		volumeName := volumeIDs[0]
		mountID := volumeIDs[1]
		parentSpan.SetAttributes(attribute.String("volume_name", volumeName))
		parentSpan.SetAttributes(attribute.String("mount_id", mountID))
		err := s.worker.DeleteModel(ctx, isStaticVolume, volumeName, mountID)
		if err != nil {
			span.SetStatus(otelCodes.Error, "failed to delete model")
			span.RecordError(err)
			return nil, isStaticVolume, status.Error(codes.Internal, errors.Wrap(err, "delete model").Error())
		}
		return &csi.DeleteVolumeResponse{}, isStaticVolume, nil
	}

	return nil, isStaticVolume, status.Error(codes.InvalidArgument, "invalid volumeId format")
}

func (s *Service) localListVolumes(
	ctx context.Context,
	req *csi.ListVolumesRequest) (
	*csi.ListVolumesResponse, error) {
	volumesDir := s.cfg.GetVolumesDir()

	getEntryByVolumeName := func(volumeName string) (*csi.ListVolumesResponse_Entry, error) {
		statusPath := filepath.Join(volumesDir, volumeName, "status.json")
		modelStatus, err := s.sm.Get(statusPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			logger.WithContext(ctx).WithError(err).Errorf("failed to get volume status")
			return nil, status.Error(codes.Internal, err.Error())
		}
		progress, err := modelStatus.Progress.String()
		if err != nil {
			logger.WithContext(ctx).WithError(err).Errorf("failed to marshal progress")
			return nil, status.Error(codes.Internal, err.Error())
		}
		return &csi.ListVolumesResponse_Entry{
			Volume: &csi.Volume{
				VolumeId: modelStatus.VolumeName,
				VolumeContext: map[string]string{
					s.cfg.ParameterKeyReference():      modelStatus.Reference,
					s.cfg.ParameterKeyStatusState():    modelStatus.State,
					s.cfg.ParameterKeyStatusProgress(): progress,
				},
			},
		}, nil
	}

	volumeDirEntries, err := os.ReadDir(volumesDir)
	if err != nil {
		logger.WithContext(ctx).WithError(err).Errorf("failed to read volumes dir")
		return nil, status.Error(codes.Internal, err.Error())
	}

	entries := []*csi.ListVolumesResponse_Entry{}
	for _, entry := range volumeDirEntries {
		if !entry.IsDir() {
			continue
		}
		volumeName := entry.Name()
		entry, err := getEntryByVolumeName(volumeName)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			logger.WithContext(ctx).WithError(err).Errorf("failed to get entry for volume: %s", volumeName)
			return nil, err
		}
		entries = append(entries, entry)
	}

	return &csi.ListVolumesResponse{
		Entries: entries,
	}, nil
}
