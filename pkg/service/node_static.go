package service

import (
	"context"
	"os"
	"path/filepath"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/modelpack/model-csi-driver/pkg/mounter"
	modelStatus "github.com/modelpack/model-csi-driver/pkg/status"
	"github.com/pkg/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Service) nodePublishVolumeStatic(ctx context.Context, volumeName, targetPath string) (*csi.NodePublishVolumeResponse, error) {
	statusPath := filepath.Join(s.cfg.Get().GetVolumeDir(volumeName), "status.json")
	volumeStatus, err := s.sm.Get(statusPath)
	if err != nil {
		return nil, status.Error(codes.Internal, errors.Wrap(err, "get volume status").Error())
	}
	sourcePath := s.cfg.Get().GetModelDir(volumeStatus.VolumeName)

	if err = mounter.Mount(
		ctx,
		mounter.NewBuilder().
			Bind().
			From(sourcePath).
			MountPoint(targetPath),
	); err != nil {
		return nil, status.Error(codes.Internal, errors.Wrapf(err, "bind mount %s to target", sourcePath).Error())
	}

	volumeStatus.State = modelStatus.StateMounted
	if _, err := s.sm.Set(statusPath, *volumeStatus); err != nil {
		return nil, status.Error(codes.Internal, errors.Wrap(err, "set volume status").Error())
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (s *Service) nodeUnPublishVolumeStatic(ctx context.Context, volumeName, targetPath string, isMounted bool) (*csi.NodeUnpublishVolumeResponse, error) {
	if isMounted {
		if err := mounter.UMount(ctx, targetPath, true); err != nil {
			return nil, status.Error(codes.Internal, errors.Wrapf(err, "unmount target path").Error())
		}
	}

	statusPath := filepath.Join(s.cfg.Get().GetVolumeDir(volumeName), "status.json")
	volumeStatus, err := s.sm.Get(statusPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &csi.NodeUnpublishVolumeResponse{}, nil
		}
		return nil, status.Error(codes.Internal, errors.Wrap(err, "get volume status").Error())
	}

	volumeStatus.State = modelStatus.StateUmounted
	if _, err := s.sm.Set(statusPath, *volumeStatus); err != nil {
		return nil, status.Error(codes.Internal, errors.Wrap(err, "set volume status").Error())
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}
