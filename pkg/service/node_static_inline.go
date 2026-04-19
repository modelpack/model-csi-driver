package service

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/modelpack/model-csi-driver/pkg/mounter"
	modelStatus "github.com/modelpack/model-csi-driver/pkg/status"
	"github.com/pkg/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Service) nodePublishVolumeStaticInlineVolume(ctx context.Context, volumeName, targetPath, reference string, excludeModelWeights bool, excludeFilePatterns []string) (*csi.NodePublishVolumeResponse, error) {
	// Partial-file variants (exclude_model_weights / exclude_file_patterns) are
	// intentionally not shared through the node-level cache in this path to
	// keep the sharing semantics simple: only a full pull becomes a shared
	// cache entry. Fall back to the legacy per-volume pull for those cases.
	if excludeModelWeights || len(excludeFilePatterns) > 0 {
		return s.nodePublishVolumeStaticInlineVolumeLegacy(ctx, volumeName, targetPath, reference, excludeModelWeights, excludeFilePatterns)
	}

	statusPath := filepath.Join(s.cfg.Get().GetVolumeDir(volumeName), "status.json")

	// Persist an initial status so that progress / reference is observable
	// even while the shared pull is still in flight.
	if _, err := s.sm.Set(statusPath, modelStatus.Status{
		VolumeName: volumeName,
		Reference:  reference,
		Inline:     true,
		State:      modelStatus.StatePullRunning,
	}); err != nil {
		return nil, status.Error(codes.Internal, errors.Wrap(err, "set initial volume status").Error())
	}

	startedAt := time.Now()
	digest, err := s.worker.EnsureCachedModel(ctx, reference, volumeName)
	if err != nil {
		// Best-effort: record failure state; the volume directory will be
		// cleaned up by the unpublish path.
		if _, setErr := s.sm.Set(statusPath, modelStatus.Status{
			VolumeName: volumeName,
			Reference:  reference,
			Inline:     true,
			State:      modelStatus.StatePullFailed,
		}); setErr != nil {
			logger.WithContext(ctx).WithError(setErr).Warn("failed to persist pull-failed status")
		}
		return nil, status.Error(codes.Internal, errors.Wrap(err, "ensure cached model").Error())
	}
	duration := time.Since(startedAt)
	logger.WithContext(ctx).Infof("ensured cached model: %s digest=%s duration=%s", reference, digest, duration)

	sourceDir := s.cfg.Get().GetCacheContentDir(digest)
	if err := mounter.Mount(
		ctx,
		mounter.NewBuilder().
			Bind().
			From(sourceDir).
			MountPoint(targetPath),
	); err != nil {
		// Roll back the ref we just registered so we don't leak an entry that
		// would keep the cache alive forever.
		if relErr := s.worker.ReleaseCachedModel(ctx, digest, volumeName); relErr != nil {
			logger.WithContext(ctx).WithError(relErr).Warnf("release cached model after mount failure: %s", digest)
		}
		return nil, status.Error(codes.Internal, errors.Wrapf(err, "bind mount %s to target %s", sourceDir, targetPath).Error())
	}

	if _, err := s.sm.Set(statusPath, modelStatus.Status{
		VolumeName:  volumeName,
		Reference:   reference,
		Inline:      true,
		CacheDigest: digest,
		State:       modelStatus.StateMounted,
	}); err != nil {
		return nil, status.Error(codes.Internal, errors.Wrap(err, "set volume status").Error())
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

// nodePublishVolumeStaticInlineVolumeLegacy is the per-volume pull path used
// only for partial-file inline volumes (exclude_model_weights /
// exclude_file_patterns). It preserves the original behavior to avoid
// accidentally sharing a partial pull with other pods that may expect a full
// model tree.
func (s *Service) nodePublishVolumeStaticInlineVolumeLegacy(ctx context.Context, volumeName, targetPath, reference string, excludeModelWeights bool, excludeFilePatterns []string) (*csi.NodePublishVolumeResponse, error) {
	modelDir := s.cfg.Get().GetModelDir(volumeName)

	startedAt := time.Now()
	if err := s.worker.PullModel(ctx, true, volumeName, "", reference, modelDir, false, excludeModelWeights, excludeFilePatterns); err != nil {
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

func (s *Service) nodeUnPublishVolumeStaticInlineVolume(ctx context.Context, volumeName, targetPath string, isMounted bool) (*csi.NodeUnpublishVolumeResponse, error) {
	if isMounted {
		if err := mounter.UMount(ctx, targetPath, true); err != nil {
			return nil, status.Error(codes.Internal, errors.Wrapf(err, "unmount target path").Error())
		}
	}

	// Release the shared-cache reference, if any. We first try the digest
	// recorded in status.json; if that is missing (e.g. older volumes or a
	// crash before status was persisted), fall back to scanning the refs tree.
	sourceVolumeDir := s.cfg.Get().GetVolumeDir(volumeName)
	statusPath := filepath.Join(sourceVolumeDir, "status.json")
	digest := ""
	if volumeStatus, err := s.sm.Get(statusPath); err == nil && volumeStatus != nil {
		digest = volumeStatus.CacheDigest
	}
	if digest == "" {
		if found, err := s.worker.FindCacheDigestByRef(volumeName); err != nil {
			logger.WithContext(ctx).WithError(err).Warnf("scan cache refs for volume: %s", volumeName)
		} else {
			digest = found
		}
	}
	if digest != "" {
		if err := s.worker.ReleaseCachedModel(ctx, digest, volumeName); err != nil {
			logger.WithContext(ctx).WithError(err).Warnf("release cached model: %s", digest)
		}
	}

	if err := os.RemoveAll(sourceVolumeDir); err != nil {
		return nil, status.Error(codes.Internal, errors.Wrapf(err, "remove static inline volume dir").Error())
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}
