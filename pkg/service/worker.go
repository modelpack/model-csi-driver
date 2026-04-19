package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/containerd/pkg/kmutex"
	"github.com/modelpack/modctl/pkg/backend"
	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/config/auth"
	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/modelpack/model-csi-driver/pkg/metrics"
	"github.com/modelpack/model-csi-driver/pkg/status"
	"github.com/modelpack/model-csi-driver/pkg/utils"
	"github.com/pkg/errors"
	"golang.org/x/sync/singleflight"
)

// cacheReadyMarker is the sentinel filename placed under the refs directory
// to indicate that the corresponding content directory is fully materialized.
// It is kept under refs/ (not content/) so it doesn't pollute the bind-mounted
// model content seen by pods.
const cacheReadyMarker = ".ready"

// ResolveCacheDigest resolves a model reference to its manifest digest, used
// as the cache key for the node-level shared cache. It is exposed as a
// package-level variable so integration tests that inject a fake Puller can
// also inject a fake digest resolver and avoid contacting a real registry.
var ResolveCacheDigest = func(ctx context.Context, reference string) (string, error) {
	keyChain, err := auth.GetKeyChainByRef(reference)
	if err != nil {
		return "", errors.Wrapf(err, "get auth for model: %s", reference)
	}
	plainHTTP := keyChain.ServerScheme == "http"

	b, err := backend.New("")
	if err != nil {
		return "", errors.Wrap(err, "create modctl backend")
	}

	modelArtifact := NewModelArtifact(b, reference, plainHTTP)
	artifact, err := modelArtifact.Inspect(ctx, reference)
	if err != nil {
		return "", errors.Wrapf(err, "inspect model: %s", reference)
	}
	if artifact.Digest == "" {
		return "", errors.Errorf("empty manifest digest for model: %s", reference)
	}
	return artifact.Digest, nil
}

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
	newPuller  func(ctx context.Context, pullCfg *config.PullConfig, hook *status.Hook, diskQuotaChecker *DiskQuotaChecker) Puller
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
		// Intentionally use context.Background() here: deleteModel is
		// frequently invoked as the cleanup tail of a failed/canceled
		// PullModel, whose ctx is already past its deadline. Using that
		// ctx would make the lock acquisition fail immediately with
		// "context deadline exceeded" and leave the volume dir on disk.
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

		statusPath := filepath.Join(volumeDir, "status.json")
		worker.sm.HookManager.Delete(statusPath)

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
	setStatus := func(state status.State) (*status.Status, error) {
		status, err := worker.sm.Set(statusPath, status.Status{
			VolumeName: volumeName,
			MountID:    mountID,
			Reference:  reference,
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
		// Intentionally use context.Background() here: the inflight
		// caller's ctx may already be past its deadline (e.g. CSI client
		// timeout) by the time another shared waiter actually gets to
		// acquire this lock. Use a fresh ctx so the lock can always be
		// obtained; cancellation is propagated separately via the
		// per-key contextMap below.
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
		puller := worker.newPuller(ctx, &worker.cfg.Get().PullConfig, hook, diskQuotaChecker)
		_, err := setStatus(status.StatePullRunning)
		if err != nil {
			return nil, errors.Wrapf(err, "set status before pull model")
		}
		if err := puller.Pull(ctx, reference, modelDir, excludeModelWeights, excludeFilePatterns); err != nil {
			if errors.Is(err, context.Canceled) {
				err = errors.Wrapf(err, "pull model canceled")
				if _, err2 := setStatus(status.StatePullCanceled); err2 != nil {
					return nil, errors.Wrapf(err, "set model status: %v", err2)
				}
			} else if errors.Is(err, context.DeadlineExceeded) {
				err = errors.Wrapf(err, "pull model timeout")
				if _, err2 := setStatus(status.StatePullTimeout); err2 != nil {
					return nil, errors.Wrapf(err, "set model status: %v", err2)
				}
			} else {
				err = errors.Wrapf(err, "pull model failed")
				if _, err2 := setStatus(status.StatePullFailed); err2 != nil {
					return nil, errors.Wrapf(err, "set model status: %v", err2)
				}
			}
			return nil, err
		}
		_, err = setStatus(status.StatePullSucceeded)
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

// EnsureCachedModel makes sure the model for the given reference is present in
// the shared node-level cache, then registers a reference under
// cache/refs/<algo>/<hex>/<refName>. The returned string is the cache digest
// (e.g. "sha256:abc..."), which callers must remember (typically via
// status.json) so that they can call ReleaseCachedModel later.
//
// Concurrent callers for the same reference dedupe on the manifest digest:
// only one pull actually runs, the others wait and then just register their
// own ref file. Each caller holds an exclusive kmutex on the digest while
// checking readiness / pulling / writing its ref file, so ref registration is
// serialized against garbage collection in ReleaseCachedModel.
func (worker *Worker) EnsureCachedModel(ctx context.Context, reference, refName string) (string, error) {
	start := time.Now()

	digest, err := ResolveCacheDigest(ctx, reference)
	if err != nil {
		return "", err
	}

	contentDir := worker.cfg.Get().GetCacheContentDir(digest)
	refsDir := worker.cfg.Get().GetCacheRefsDir(digest)

	// Deduplicate concurrent pulls for the same digest. The inner function
	// only performs the pull when not yet ready; ref registration is handled
	// outside (and under its own lock) so each caller writes its own ref file
	// even when sharing the pull result.
	_, pullErr, shared := worker.inflight.Do("cache-pull-"+digest, func() (interface{}, error) {
		// Intentionally use context.Background(): the caller's ctx may
		// already be past its deadline (e.g. CSI client timeout) by the
		// time a shared waiter actually gets to acquire this lock.
		if err := worker.kmutex.Lock(context.Background(), digest); err != nil {
			return nil, errors.Wrapf(err, "lock cache digest: %s", digest)
		}
		defer worker.kmutex.Unlock(digest)

		if _, err := os.Stat(filepath.Join(refsDir, cacheReadyMarker)); err == nil {
			return nil, nil
		}

		// Clean up any half-finished cache directory from previous failures
		// before starting a fresh pull.
		if err := os.RemoveAll(contentDir); err != nil {
			return nil, errors.Wrapf(err, "cleanup cache content dir: %s", contentDir)
		}
		if err := os.MkdirAll(refsDir, 0755); err != nil {
			return nil, errors.Wrapf(err, "create cache refs dir: %s", refsDir)
		}

		hook := status.NewHook(ctx)
		p := worker.newPuller(ctx, &worker.cfg.Get().PullConfig, hook, nil)
		if err := p.Pull(ctx, reference, contentDir, false, nil); err != nil {
			_ = os.RemoveAll(contentDir)
			return nil, errors.Wrapf(err, "pull model %s into cache", reference)
		}

		readyPath := filepath.Join(refsDir, cacheReadyMarker)
		if err := os.WriteFile(readyPath, []byte{}, 0644); err != nil {
			_ = os.RemoveAll(contentDir)
			return nil, errors.Wrapf(err, "write cache ready marker: %s", readyPath)
		}
		return nil, nil
	})
	metrics.NodeOpObserve("cache_ensure_pull", start, pullErr)
	if pullErr != nil {
		return "", errors.Wrapf(pullErr, "ensure cached model: %s (shared=%v)", reference, shared)
	}
	logger.WithContext(ctx).Infof("ensured cached model: %s digest=%s shared=%v", reference, digest, shared)

	// Register this caller's reference. Held under the same per-digest lock
	// that guards release/GC, guaranteeing that the content directory cannot
	// disappear between readiness check and bind mount by the caller.
	// Use context.Background() for the same reason as above.
	if err := worker.kmutex.Lock(context.Background(), digest); err != nil {
		return "", errors.Wrapf(err, "lock cache digest for ref: %s", digest)
	}
	defer worker.kmutex.Unlock(digest)

	if _, err := os.Stat(filepath.Join(refsDir, cacheReadyMarker)); err != nil {
		return "", errors.Wrapf(err, "cache content vanished before ref registration: %s", digest)
	}
	refFile := filepath.Join(refsDir, refName)
	if err := os.WriteFile(refFile, []byte{}, 0644); err != nil {
		return "", errors.Wrapf(err, "write cache ref file: %s", refFile)
	}

	return digest, nil
}

// ReleaseCachedModel removes a caller's ref file under cache/refs/<digest>/.
// If no other callers hold a ref, the entire cache content + refs entry is
// garbage collected. Unknown / already-gone digests are treated as success.
func (worker *Worker) ReleaseCachedModel(ctx context.Context, digest, refName string) error {
	start := time.Now()
	if digest == "" {
		return nil
	}

	contentDir := worker.cfg.Get().GetCacheContentDir(digest)
	refsDir := worker.cfg.Get().GetCacheRefsDir(digest)

	// Use context.Background(): release is typically called from an
	// unpublish path whose ctx may have already been canceled by kubelet,
	// but we still need to drop the ref and GC the cache reliably.
	if err := worker.kmutex.Lock(context.Background(), digest); err != nil {
		return errors.Wrapf(err, "lock cache digest for release: %s", digest)
	}
	defer worker.kmutex.Unlock(digest)

	refFile := filepath.Join(refsDir, refName)
	if err := os.Remove(refFile); err != nil && !os.IsNotExist(err) {
		return errors.Wrapf(err, "remove cache ref file: %s", refFile)
	}

	entries, err := os.ReadDir(refsDir)
	if err != nil {
		if os.IsNotExist(err) {
			metrics.NodeOpObserve("cache_release", start, nil)
			return nil
		}
		return errors.Wrapf(err, "read cache refs dir: %s", refsDir)
	}
	hasUser := false
	for _, entry := range entries {
		if entry.Name() == cacheReadyMarker {
			continue
		}
		hasUser = true
		break
	}
	if hasUser {
		metrics.NodeOpObserve("cache_release", start, nil)
		return nil
	}

	// No more users: GC the cache content and the refs entry.
	if err := os.RemoveAll(contentDir); err != nil {
		return errors.Wrapf(err, "remove cache content dir: %s", contentDir)
	}
	if err := os.RemoveAll(refsDir); err != nil {
		return errors.Wrapf(err, "remove cache refs dir: %s", refsDir)
	}
	logger.WithContext(ctx).Infof("garbage collected cache entry: digest=%s", digest)
	metrics.NodeOpObserve("cache_release", start, nil)
	return nil
}

// FindCacheDigestByRef scans cache/refs/<algo>/<hex>/ for a ref file named
// refName and returns the corresponding digest string ("<algo>:<hex>"). Used
// by unpublish paths that don't have the digest at hand and want to avoid
// re-contacting the registry just to release a reference.
func (worker *Worker) FindCacheDigestByRef(refName string) (string, error) {
	refsRoot := worker.cfg.Get().GetCacheRefsRootDir()
	algoEntries, err := os.ReadDir(refsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", errors.Wrapf(err, "read cache refs root: %s", refsRoot)
	}
	for _, algoEntry := range algoEntries {
		if !algoEntry.IsDir() {
			continue
		}
		algoDir := filepath.Join(refsRoot, algoEntry.Name())
		hexEntries, err := os.ReadDir(algoDir)
		if err != nil {
			continue
		}
		for _, hexEntry := range hexEntries {
			if !hexEntry.IsDir() {
				continue
			}
			candidate := filepath.Join(algoDir, hexEntry.Name(), refName)
			if _, err := os.Stat(candidate); err == nil {
				return algoEntry.Name() + ":" + hexEntry.Name(), nil
			}
		}
	}
	return "", nil
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
