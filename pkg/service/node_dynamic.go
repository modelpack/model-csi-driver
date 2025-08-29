package service

import (
	"os"
	"path/filepath"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/modelpack/model-csi-driver/pkg/mounter"
	modelStatus "github.com/modelpack/model-csi-driver/pkg/status"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Service) nodePublishVolumeDynamicForRootMount(ctx context.Context, volumeName, targetPath string) (*csi.NodePublishVolumeResponse, error) {
	if s.dynamicCSISockPath == "" {
		return nil, status.Error(codes.FailedPrecondition, "dynamic csi endpoint is not configured")
	}

	sourceVolumeDir := s.cfg.GetVolumeDirForDynamic(volumeName)
	sourceCSIDir := s.cfg.GetCSISockDirForDynamic(volumeName)
	if err := os.MkdirAll(sourceCSIDir, 0755); err != nil {
		return nil, status.Error(codes.Internal, errors.Wrap(err, "create source csi dir").Error())
	}
	sourceModelsDir := s.cfg.GetModelsDirForDynamic(volumeName)
	if err := os.MkdirAll(sourceModelsDir, 0755); err != nil {
		return nil, status.Error(codes.Internal, errors.Wrap(err, "create source models dir").Error())
	}
	hostCSISockDir := filepath.Dir(s.dynamicCSISockPath)

	statusPath := filepath.Join(sourceVolumeDir, "status.json")
	_, err := s.sm.Set(statusPath, modelStatus.Status{
		VolumeName: volumeName,
	})
	if err != nil {
		return nil, status.Error(codes.Internal, errors.Wrap(err, "create volume status").Error())
	}

	if err = mounter.Mount(
		ctx,
		mounter.NewBuilder().
			Bind().
			From(hostCSISockDir).
			MountPoint(sourceCSIDir),
	); err != nil {
		return nil, status.Error(codes.Internal, errors.Wrapf(err, "bind mount %s to %s", hostCSISockDir, sourceCSIDir).Error())
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

func (s *Service) nodeUnPublishVolumeDynamic(ctx context.Context, volumeName, targetPath string) (*csi.NodeUnpublishVolumeResponse, error) {
	sourceCSIDir := s.cfg.GetCSISockDirForDynamic(volumeName)
	if err := mounter.UMount(ctx, sourceCSIDir, true); err != nil {
		logger.WithContext(ctx).WithError(err).Errorf("unmount csi directory path")
		// return nil, status.Error(codes.Internal, errors.Wrapf(err, "unmount csi directory path").Error())
	}

	if err := mounter.UMount(ctx, targetPath, true); err != nil {
		logger.WithContext(ctx).WithError(err).Errorf("unmount target path")
		// return nil, status.Error(codes.Internal, errors.Wrapf(err, "unmount target path").Error())
	}

	sourceVolumeDir := s.cfg.GetVolumeDirForDynamic(volumeName)
	if err := os.RemoveAll(sourceVolumeDir); err != nil {
		return nil, status.Error(codes.Internal, errors.Wrapf(err, "remove dynamic volume dir").Error())
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}
