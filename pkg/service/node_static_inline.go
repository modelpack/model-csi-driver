package service

import (
	"os"
	"path/filepath"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/modelpack/model-csi-driver/pkg/mounter"
	modelStatus "github.com/modelpack/model-csi-driver/pkg/status"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Service) nodePublishVolumeStaticInlineVolume(ctx context.Context, volumeName, targetPath, reference string) (*csi.NodePublishVolumeResponse, error) {
	modelDir := s.cfg.Get().GetModelDir(volumeName)

	startedAt := time.Now()
	if err := s.worker.PullModel(ctx, true, volumeName, "", reference, modelDir, false); err != nil {
		return nil, status.Error(codes.Internal, errors.Wrap(err, "pull model").Error())
	}
	duration := time.Since(startedAt)
	logger.WithContext(ctx).Infof("pulled model: %s %s", reference, duration)

	if err := mounter.Mount(
		ctx,
		mounter.NewBuilder().
			Bind().
			From(modelDir).
			MountPoint(targetPath),
	); err != nil {
		return nil, status.Error(codes.Internal, errors.Wrapf(err, "bind mount %s to target %s", modelDir, targetPath).Error())
	}

	statusPath := filepath.Join(s.cfg.Get().GetVolumeDir(volumeName), "status.json")
	volumeStatus, err := s.sm.Get(statusPath)
	if err != nil {
		return nil, status.Error(codes.Internal, errors.Wrap(err, "get volume status").Error())
	}

	// The field distinguishes inline and PVC based volume.
	volumeStatus.Inline = true
	volumeStatus.State = modelStatus.StateMounted
	if _, err := s.sm.Set(statusPath, *volumeStatus); err != nil {
		return nil, status.Error(codes.Internal, errors.Wrap(err, "set volume status").Error())
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (s *Service) nodeUnPublishVolumeStaticInlineVolume(ctx context.Context, volumeName, targetPath string) (*csi.NodeUnpublishVolumeResponse, error) {
	if err := mounter.UMount(ctx, targetPath, true); err != nil {
		logger.WithContext(ctx).WithError(err).Errorf("unmount target path")
		// return nil, status.Error(codes.Internal, errors.Wrapf(err, "unmount target path").Error())
	}

	sourceVolumeDir := s.cfg.Get().GetVolumeDir(volumeName)
	if err := os.RemoveAll(sourceVolumeDir); err != nil {
		return nil, status.Error(codes.Internal, errors.Wrapf(err, "remove static inline volume dir").Error())
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}
