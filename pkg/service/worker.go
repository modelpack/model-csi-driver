package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/containerd/pkg/kmutex"
	modctlBackend "github.com/modelpack/modctl/pkg/backend"
	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/config/auth"
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

// ResolveDigest returns the manifest digest for reference. Overridable
// for tests.
var ResolveDigest = defaultResolveDigest

func defaultResolveDigest(ctx context.Context, reference string) (string, error) {
	keyChain, err := auth.GetKeyChainByRef(reference)
	if err != nil {
		return "", errors.Wrapf(err, "get auth for model: %s", reference)
	}
	plainHTTP := keyChain.ServerScheme == "http"

	b, err := modctlBackend.New("")
	if err != nil {
		return "", errors.Wrap(err, "create modctl backend")
	}

	artifact, err := NewModelArtifact(b, reference, plainHTTP).Inspect(ctx, reference)
	if err != nil {
		return "", errors.Wrapf(err, "inspect model: %s", reference)
	}
	if artifact.Digest == "" {
		return "", errors.Errorf("empty digest from inspect: %s", reference)
	}
	return artifact.Digest, nil
}

type Worker struct {
	cfg       *config.Config
	newPuller func(ctx context.Context, pullCfg *config.PullConfig, hook *status.Hook, diskQuotaChecker *DiskQuotaChecker) Puller
	sm        *status.StatusManager
	store     *ModelStore
	// resolveDigest is overridable per-Worker for unit tests.
	resolveDigest func(ctx context.Context, reference string) (string, error)
	inflight      singleflight.Group
	contextMap    *ContextMap
	kmutex        kmutex.KeyedLocker
}

func NewWorker(cfg *config.Config, sm *status.StatusManager) (*Worker, error) {
	return &Worker{
		cfg:           cfg,
		newPuller:     NewPuller,
		sm:            sm,
		store:         NewModelStore(cfg.Get().RootDir),
		resolveDigest: ResolveDigest,
		inflight:      singleflight.Group{},
		contextMap:    NewContextMap(),
		kmutex:        kmutex.New(),
	}, nil
}

// ReleaseVolumeTree removes volumeDir and runs ModelStore GC for every
// cache key referenced by status.json files under it. Used by unpublish
// paths that bypass DeleteModel (static-inline / dynamic-root). MaybeGC
// errors are logged; only RemoveAll failures are returned.
func (worker *Worker) ReleaseVolumeTree(ctx context.Context, volumeDir string) error {
	cacheKeys := worker.collectCacheKeys(ctx, volumeDir)

	if err := fsRemoveAll(volumeDir); err != nil {
		return errors.Wrapf(err, "remove volume tree: %s", volumeDir)
	}

	for key := range cacheKeys {
		if err := worker.store.MaybeGC(ctx, key); err != nil {
			logger.WithContext(ctx).WithError(err).Warnf("maybe gc model store entry: %s", key)
		}
	}
	return nil
}

// collectCacheKeys walks volumeDir for status.json files and returns the
// distinct non-empty CacheKey values.
func (worker *Worker) collectCacheKeys(ctx context.Context, volumeDir string) map[string]struct{} {
	keys := map[string]struct{}{}
	err := filepath.WalkDir(volumeDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || d.Name() != "status.json" {
			return nil
		}
		st, err := worker.sm.Get(path)
		if err != nil || st == nil || st.CacheKey == "" {
			return nil
		}
		keys[st.CacheKey] = struct{}{}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		logger.WithContext(ctx).WithError(err).Warnf("walk volume dir for cache keys: %s", volumeDir)
	}
	return keys
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

		// Capture cache key before wiping so we can GC after.
		statusPath := filepath.Join(volumeDir, "status.json")
		var cacheKey string
		if st, err := worker.sm.Get(statusPath); err == nil && st != nil {
			cacheKey = st.CacheKey
		}

		// Retry to tolerate transient "directory not empty" while writers race.
		if err := utils.WithRetry(ctx, func() error {
			if err := os.RemoveAll(volumeDir); err != nil {
				return errors.Wrapf(err, "remove volume dir: %s", volumeDir)
			}
			return nil
		}, 60, 1*time.Second); err != nil {
			return nil, errors.Wrapf(err, "retry remove volume dir: %s", volumeDir)
		}
		logger.WithContext(ctx).Infof("removed volume dir: %s", volumeDir)

		worker.sm.HookManager.Delete(statusPath)

		// GC the store entry if this was the last reference. Best effort.
		if cacheKey != "" {
			if err := worker.store.MaybeGC(ctx, cacheKey); err != nil {
				logger.WithContext(ctx).WithError(err).Warnf("maybe gc model store entry: %s", cacheKey)
			}
		}

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

func (worker *Worker) PullModel(
	ctx context.Context,
	isStaticVolume bool,
	volumeName, mountID,
	reference,
	modelDir string,
	checkDiskQuota bool,
	excludeModelWeights bool,
	excludeFilePatterns []string,
) error {
	start := time.Now()

	statusPath := filepath.Join(filepath.Dir(modelDir), "status.json")
	err := worker.pullModel(ctx, statusPath, volumeName, mountID, reference, modelDir, checkDiskQuota, excludeModelWeights, excludeFilePatterns)
	metrics.NodeOpObserve("pull_image", start, err)

	if err != nil && !errors.Is(err, ErrConflict) {
		if err2 := worker.DeleteModel(ctx, isStaticVolume, volumeName, mountID); err2 != nil {
			return errors.Wrapf(err, "delete model: %v", err2)
		}
	}

	return err
}

func (worker *Worker) pullModel(ctx context.Context, statusPath, volumeName, mountID, reference, modelDir string, checkDiskQuota, excludeModelWeights bool, excludeFilePatterns []string) error {
	// Partial pulls (exclude*) bypass the store: their on-disk shape
	// depends on per-volume parameters, not the manifest digest.
	useStore := !excludeModelWeights && len(excludeFilePatterns) == 0

	setStatus := func(state status.State, cacheKey string) (*status.Status, error) {
		status, err := worker.sm.Set(statusPath, status.Status{
			VolumeName: volumeName,
			MountID:    mountID,
			Reference:  reference,
			CacheKey:   cacheKey,
			State:      state,
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

		hook := status.NewHook(ctx)
		worker.sm.HookManager.Set(statusPath, hook)

		var diskQuotaChecker *DiskQuotaChecker
		checkDiskQuota := worker.cfg.Get().Features.CheckDiskQuota && checkDiskQuota && !worker.isModelExisted(ctx, reference)
		if checkDiskQuota {
			diskQuotaChecker = NewDiskQuotaChecker(worker.cfg)
		}

		// Resolve the manifest digest so we can dedupe via the store.
		var cacheKey string
		if useStore {
			digest, err := worker.resolveDigest(ctx, reference)
			if err != nil {
				if _, err2 := setStatus(status.StatePullFailed, ""); err2 != nil {
					return nil, errors.Wrapf(err, "set model status: %v", err2)
				}
				return nil, errors.Wrap(err, "resolve manifest digest")
			}
			cacheKey = digest
		}

		puller := worker.newPuller(ctx, &worker.cfg.Get().PullConfig, hook, diskQuotaChecker)
		if _, err := setStatus(status.StatePullRunning, cacheKey); err != nil {
			return nil, errors.Wrapf(err, "set status before pull model")
		}

		var pullErr error
		if useStore {
			pullErr = worker.store.Materialize(ctx, cacheKey, modelDir, func(ctx context.Context, dst string) error {
				return puller.Pull(ctx, reference, dst, false, nil)
			})
		} else {
			pullErr = puller.Pull(ctx, reference, modelDir, excludeModelWeights, excludeFilePatterns)
		}

		if pullErr != nil {
			switch {
			case errors.Is(pullErr, context.Canceled):
				pullErr = errors.Wrapf(pullErr, "pull model canceled")
				if _, err2 := setStatus(status.StatePullCanceled, cacheKey); err2 != nil {
					return nil, errors.Wrapf(pullErr, "set model status: %v", err2)
				}
			case errors.Is(pullErr, context.DeadlineExceeded):
				pullErr = errors.Wrapf(pullErr, "pull model timeout")
				if _, err2 := setStatus(status.StatePullTimeout, cacheKey); err2 != nil {
					return nil, errors.Wrapf(pullErr, "set model status: %v", err2)
				}
			default:
				pullErr = errors.Wrapf(pullErr, "pull model failed")
				if _, err2 := setStatus(status.StatePullFailed, cacheKey); err2 != nil {
					return nil, errors.Wrapf(pullErr, "set model status: %v", err2)
				}
			}
			return nil, pullErr
		}

		if _, err := setStatus(status.StatePullSucceeded, cacheKey); err != nil {
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
