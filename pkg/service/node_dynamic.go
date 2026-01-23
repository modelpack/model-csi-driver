package service

import (
	"context"
	"os"
	"path/filepath"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/modelpack/model-csi-driver/pkg/mounter"
	modelStatus "github.com/modelpack/model-csi-driver/pkg/status"
	"github.com/modelpack/model-csi-driver/pkg/utils"
	"github.com/pkg/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Service) nodePublishVolumeDynamicForRootMount(ctx context.Context, volumeName, targetPath string) (*csi.NodePublishVolumeResponse, error) {
	sourceModelsDir := s.cfg.Get().GetModelsDirForDynamic(volumeName)
	if err := os.MkdirAll(sourceModelsDir, 0755); err != nil {
		return nil, status.Error(codes.Internal, errors.Wrap(err, "create source models dir").Error())
	}

	sourceCSISockPath := s.cfg.Get().GetCSISockPathForDynamic(volumeName)
	_, err := s.DynamicServerManager.CreateServer(ctx, sourceCSISockPath)
	if err != nil {
		return nil, status.Error(codes.Internal, errors.Wrap(err, "create dynamic csi server").Error())
	}

	sourceVolumeDir := s.cfg.Get().GetVolumeDirForDynamic(volumeName)
	statusPath := filepath.Join(sourceVolumeDir, "status.json")
	_, err = s.sm.Set(statusPath, modelStatus.Status{
		VolumeName: volumeName,
	})
	if err != nil {
		return nil, status.Error(codes.Internal, errors.Wrap(err, "create volume status").Error())
	}

	if err = mounter.Mount(
		ctx,
		mounter.NewBuilder().
			RBind().
			From(sourceVolumeDir).
			MountPoint(targetPath),
	); err != nil {
		return nil, status.Error(codes.Internal, errors.Wrapf(err, "bind mount %s to target", sourceVolumeDir).Error())
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (s *Service) nodeUnPublishVolumeDynamic(ctx context.Context, volumeName, targetPath string, isMounted bool) (*csi.NodeUnpublishVolumeResponse, error) {
	sourceCSIDir := s.cfg.Get().GetCSISockDirForDynamic(volumeName)
	volumeDir := s.cfg.Get().GetVolumeDirForDynamic(volumeName)

	sameDevice, err := utils.IsInSameDevice(sourceCSIDir, volumeDir)
	if err != nil {
		logger.WithContext(ctx).WithError(err).Warnf("check same device for csi dir and volume dir")
	}
	logger.WithContext(ctx).Infof("check csi dir and volume dir in same device: %v", sameDevice)
	if sameDevice {
		sourceCSISockPath := s.cfg.Get().GetCSISockPathForDynamic(volumeName)
		if err := s.DynamicServerManager.CloseServer(ctx, sourceCSISockPath); err != nil {
			logger.WithContext(ctx).WithError(err).Errorf("close dynamic csi server")
		}
	} else {
		// Deprecated: use DynamicServerManager to manage dynamic csi.sock servers,
		// keep this for backward compatibility.
		if err := mounter.UMount(ctx, sourceCSIDir, true); err != nil {
			logger.WithContext(ctx).WithError(err).Warnf("unmount csi directory path")
		}
	}

	if isMounted {
		if err := mounter.UMount(ctx, targetPath, true); err != nil {
			return nil, status.Error(codes.Internal, errors.Wrapf(err, "unmount target path").Error())
		}
	}

	sourceVolumeDir := s.cfg.Get().GetVolumeDirForDynamic(volumeName)
	if err := os.RemoveAll(sourceVolumeDir); err != nil {
		return nil, status.Error(codes.Internal, errors.Wrapf(err, "remove dynamic volume dir").Error())
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}
