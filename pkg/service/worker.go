package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/containerd/pkg/kmutex"
	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/modelpack/model-csi-driver/pkg/metrics"
	"github.com/modelpack/model-csi-driver/pkg/status"
	"github.com/modelpack/model-csi-driver/pkg/utils"
	"github.com/pkg/errors"
	"golang.org/x/sync/singleflight"
)

var ErrConflict = errors.New("conflict")

type ContextMap struct {
	cancelFuncs map[string]*context.CancelFunc
	mutex       sync.Mutex
}

func NewContextMap() *ContextMap {
	return &ContextMap{
		cancelFuncs: make(map[string]*context.CancelFunc),
	}
}

func (cm *ContextMap) Set(key string, cancelFunc *context.CancelFunc) {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	if cancelFunc == nil {
		delete(cm.cancelFuncs, key)
		return
	}

	cm.cancelFuncs[key] = cancelFunc
}

func (cm *ContextMap) Get(key string) *context.CancelFunc {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	return cm.cancelFuncs[key]
}

type Worker struct {
	cfg        *config.Config
	newPuller  func(ctx context.Context, pullCfg *config.PullConfig, hook *Hook, diskQuotaChecker *DiskQuotaChecker) Puller
	sm         *status.StatusManager
	inflight   singleflight.Group
	contextMap *ContextMap
	kmutex     kmutex.KeyedLocker
}

func NewWorker(cfg *config.Config, sm *status.StatusManager) (*Worker, error) {
	return &Worker{
		cfg:        cfg,
		newPuller:  NewPuller,
		sm:         sm,
		inflight:   singleflight.Group{},
		contextMap: NewContextMap(),
		kmutex:     kmutex.New(),
	}, nil
}

func (worker *Worker) deleteModel(ctx context.Context, isStaticVolume bool, volumeName, mountID string) error {
	inflightKey := fmt.Sprintf("delete-%s/%s", volumeName, mountID)
	contextKey := fmt.Sprintf("%s/%s", volumeName, mountID)
	if cancelFunc := worker.contextMap.Get(contextKey); cancelFunc != nil {
		(*cancelFunc)()
		logger.WithContext(ctx).Infof("canceled pulling request: %s", contextKey)
	}
	_, err, _ := worker.inflight.Do(inflightKey, func() (interface{}, error) {
		if err := worker.kmutex.Lock(context.Background(), contextKey); err != nil {
			return nil, errors.Wrapf(err, "lock context key: %s", contextKey)
		}

		defer worker.kmutex.Unlock(contextKey)

		volumeDir := worker.cfg.Get().GetVolumeDir(volumeName)
		if !isStaticVolume {
			volumeDir = worker.cfg.Get().GetMountIDDirForDynamic(volumeName, mountID)
		}
		// Retry as much as possible to ensure that the "directory not empty"
		// error does not occur, such as when other processes are still writing
		// files to the directory.
		if err := utils.WithRetry(ctx, func() error {
			if err := os.RemoveAll(volumeDir); err != nil {
				return errors.Wrapf(err, "remove volume dir: %s", volumeDir)
			}
			return nil
		}, 60, 1*time.Second); err != nil {
			return nil, errors.Wrapf(err, "retry remove volume dir: %s", volumeDir)
		}
		logger.WithContext(ctx).Infof("removed volume dir: %s", volumeDir)
		return nil, nil
	})
	return err
}

func (worker *Worker) DeleteModel(ctx context.Context, isStaticVolume bool, volumeName, mountID string) error {
	start := time.Now()

	err := worker.deleteModel(ctx, isStaticVolume, volumeName, mountID)
	metrics.NodeOpObserve("delete_image", start, err)

	return err
}

func (worker *Worker) PullModel(ctx context.Context, isStaticVolume bool, volumeName, mountID, reference, modelDir string, checkDiskQuota bool) error {
	start := time.Now()

	statusPath := filepath.Join(filepath.Dir(modelDir), "status.json")
	err := worker.pullModel(ctx, statusPath, volumeName, mountID, reference, modelDir, checkDiskQuota)
	metrics.NodeOpObserve("pull_image", start, err)

	if err != nil && !errors.Is(err, ErrConflict) {
		if err2 := worker.DeleteModel(ctx, isStaticVolume, volumeName, mountID); err2 != nil {
			return errors.Wrapf(err, "delete model: %v", err2)
		}
	}

	return err
}

func (worker *Worker) pullModel(ctx context.Context, statusPath, volumeName, mountID, reference, modelDir string, checkDiskQuota bool) error {
	setStatus := func(state status.State, progress status.Progress) (*status.Status, error) {
		status, err := worker.sm.Set(statusPath, status.Status{
			VolumeName: volumeName,
			MountID:    mountID,
			Reference:  reference,
			State:      state,
			Progress:   progress,
		})
		if err != nil {
			return nil, errors.Wrapf(err, "set model status")
		}
		return status, nil
	}

	inflightKey := fmt.Sprintf("pull-%s/%s", volumeName, mountID)
	contextKey := fmt.Sprintf("%s/%s", volumeName, mountID)
	_, err, shared := worker.inflight.Do(inflightKey, func() (interface{}, error) {
		if err := worker.kmutex.Lock(context.Background(), contextKey); err != nil {
			return nil, errors.Wrapf(err, "lock context key: %s", contextKey)
		}
		defer worker.kmutex.Unlock(contextKey)

		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		worker.contextMap.Set(contextKey, &cancel)
		defer worker.contextMap.Set(contextKey, nil)

		// re-mount with different reference is not supported.
		if mountID != "" {
			if status, _ := worker.sm.Get(statusPath); status != nil {
				if status.Reference != "" && status.Reference != reference {
					return nil, errors.Wrapf(ErrConflict, "mount_id is re-used for different reference, origin: %s, want: %s", status.Reference, reference)
				}
			}
		}

		// For hardlinked model files, we need to ensure the model
		// directory is empty before pulling.
		if err := os.RemoveAll(modelDir); err != nil {
			return nil, errors.Wrapf(err, "cleanup model directory before pull: %s", modelDir)
		}

		hook := NewHook(ctx, func(progress status.Progress) {
			if _, err := setStatus(status.StatePullRunning, progress); err != nil {
				logger.WithContext(ctx).WithError(err).Errorf("set model status: %v", err)
			}
		})
		var diskQuotaChecker *DiskQuotaChecker
		checkDiskQuota := worker.cfg.Get().Features.CheckDiskQuota && checkDiskQuota && !worker.isModelExisted(ctx, reference)
		if checkDiskQuota {
			diskQuotaChecker = NewDiskQuotaChecker(worker.cfg)
		}
		puller := worker.newPuller(ctx, &worker.cfg.Get().PullConfig, hook, diskQuotaChecker)
		_, err := setStatus(status.StatePullRunning, hook.GetProgress())
		if err != nil {
			return nil, errors.Wrapf(err, "set status before pull model")
		}
		if err := puller.Pull(ctx, reference, modelDir); err != nil {
			if errors.Is(err, context.Canceled) {
				err = errors.Wrapf(err, "pull model canceled")
				if _, err2 := setStatus(status.StatePullCanceled, hook.GetProgress()); err2 != nil {
					return nil, errors.Wrapf(err, "set model status: %v", err2)
				}
			} else if errors.Is(err, context.DeadlineExceeded) {
				err = errors.Wrapf(err, "pull model timeout")
				if _, err2 := setStatus(status.StatePullTimeout, hook.GetProgress()); err2 != nil {
					return nil, errors.Wrapf(err, "set model status: %v", err2)
				}
			} else {
				err = errors.Wrapf(err, "pull model failed")
				if _, err2 := setStatus(status.StatePullFailed, hook.GetProgress()); err2 != nil {
					return nil, errors.Wrapf(err, "set model status: %v", err2)
				}
			}
			return nil, err
		}
		_, err = setStatus(status.StatePullSucceeded, hook.GetProgress())
		if err != nil {
			return nil, errors.Wrapf(err, "set status after pull model succeeded")
		}
		return nil, nil
	})
	if err != nil {
		logger.WithContext(ctx).WithError(err).Errorf("pull model failed (shared=%v)", shared)
		return errors.Wrapf(err, "pull model image: %s, shared: %v", reference, shared)
	}
	logger.WithContext(ctx).Infof("pull model succeeded (shared=%v)", shared)

	return nil
}

func (worker *Worker) isModelExisted(ctx context.Context, reference string) bool {
	volumesDir := worker.cfg.Get().GetVolumesDir()
	volumeDirs, err := os.ReadDir(volumesDir)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.WithContext(ctx).WithError(err).Errorf("read volume dirs from %s", volumesDir)
		}
		return false
	}

	isModelMountedHere := func(modelDir string) bool {
		status, err := worker.sm.Get(filepath.Join(modelDir, "status.json"))
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				logger.WithContext(ctx).WithError(err).Error("failed to get volume status")
			}
			return false
		}
		if status.Reference == reference {
			if _, err := os.Stat(filepath.Join(modelDir, "model")); err == nil {
				return true
			}
		}
		return false
	}
	for _, volumeDir := range volumeDirs {
		if !volumeDir.IsDir() {
			continue
		}
		if isStaticVolume(volumeDir.Name()) {
			if isModelMountedHere(worker.cfg.Get().GetVolumeDir(volumeDir.Name())) {
				return true
			}
		}
		if isDynamicVolume(volumeDir.Name()) {
			modelsDirForDynamic := worker.cfg.Get().GetModelsDirForDynamic(volumeDir.Name())
			modelDirs, err := os.ReadDir(modelsDirForDynamic)
			if err != nil {
				logger.WithContext(ctx).WithError(err).Errorf("failed to read model dirs from %s", modelsDirForDynamic)
				continue
			}
			for _, modelDir := range modelDirs {
				if !modelDir.IsDir() {
					continue
				}

				mountID := modelDir.Name()
				if isModelMountedHere(worker.cfg.Get().GetMountIDDirForDynamic(volumeDir.Name(), mountID)) {
					return true
				}
			}
		}
	}

	return false
}
