package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

// linkFn is the function used to create hardlinks. It is a variable so
// that tests can inject failures (e.g. EXDEV) to exercise the fallback.
var linkFn = os.Link

// cloneByHardlink walks src and recreates its tree at dst, hardlinking
// regular files and copying symlinks. It returns an error as soon as any
// hardlink fails so that the caller can fall back to a real pull.
func cloneByHardlink(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, walkErr error) error {
		// Combined error handling for walk-callback errors and the
		// (effectively unreachable) filepath.Rel failure: any non-nil
		// error here is fatal for the clone operation. Folding both into
		// a single return keeps the body small and avoids dead branches.
		if walkErr != nil {
			return errors.Wrapf(walkErr, "walk %s", path)
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			// Readlink only fails if the entry vanished between Walk's
			// lstat and this call; treat it the same as a walk error.
			linkTarget, rlErr := os.Readlink(path)
			if rlErr != nil {
				return errors.Wrapf(rlErr, "read symlink: %s", path)
			}
			return os.Symlink(linkTarget, target)
		default:
			if err := linkFn(path, target); err != nil {
				return errors.Wrapf(err, "hardlink %s -> %s", path, target)
			}
			return nil
		}
	})
}

// parseDigestFromRef returns the digest portion of a reference like
// "registry/repo@sha256:abcdef..." or "" if the reference is tag-based.
func parseDigestFromRef(reference string) string {
	if i := strings.Index(reference, "@"); i >= 0 {
		return reference[i+1:]
	}
	return ""
}

// resolveDigest returns the manifest digest for a reference. For digest-form
// references it is parsed directly (no network call); for tag-form references
// (including mutable tags such as ":latest") it issues a remote Inspect to
// resolve the *current* digest. This is required for correct dedup: two pulls
// of the same mutable tag at different times may resolve to different digests,
// in which case the cached copy must NOT be reused.
//
// It is a package variable so tests can override it.
//
// The implementation is split into small injectable hooks so each error
// branch can be unit-tested in isolation without relying on global gomonkey
// patches (which are sensitive to test ordering).
var (
	getKeyChainByRefFn = auth.GetKeyChainByRef
	newBackendFn       = backend.New
	inspectArtifactFn  = func(b backend.Backend, ref string, plainHTTP bool, ctx context.Context) (*backend.InspectedModelArtifact, error) {
		return NewModelArtifact(b, ref, plainHTTP).Inspect(ctx, ref)
	}
)

var resolveDigest = func(ctx context.Context, reference string) (string, error) {
	if d := parseDigestFromRef(reference); d != "" {
		return d, nil
	}
	keyChain, err := getKeyChainByRefFn(reference)
	if err != nil {
		return "", errors.Wrapf(err, "get auth for digest resolution: %s", reference)
	}
	b, err := newBackendFn("")
	if err != nil {
		return "", errors.Wrap(err, "create modctl backend for digest resolution")
	}
	art, err := inspectArtifactFn(b, reference, keyChain.ServerScheme == "http", ctx)
	if err != nil {
		return "", errors.Wrapf(err, "inspect for digest resolution: %s", reference)
	}
	return art.Digest, nil
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
	refMutex   kmutex.KeyedLocker
}

func NewWorker(cfg *config.Config, sm *status.StatusManager) (*Worker, error) {
	return &Worker{
		cfg:        cfg,
		newPuller:  NewPuller,
		sm:         sm,
		inflight:   singleflight.Group{},
		contextMap: NewContextMap(),
		kmutex:     kmutex.New(),
		refMutex:   kmutex.New(),
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

		// If this volume is a live dedup source, lock its digest first so a
		// concurrent cloneByHardlink cannot walk a half-deleted tree.
		statusPath := filepath.Join(volumeDir, "status.json")
		if st, err := worker.sm.Get(statusPath); err == nil && st != nil && st.Digest != "" {
			refKey := "digest/" + st.Digest
			_ = worker.refMutex.Lock(context.Background(), refKey)
			defer worker.refMutex.Unlock(refKey)
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

// SafeRemoveAll removes root after locking refMutex for every dedup-source
// digest beneath it, so concurrent cloneByHardlink calls cannot observe a
// partially-deleted tree. Locks are taken in sorted order to avoid deadlock
// between concurrent SafeRemoveAll calls.
func (worker *Worker) SafeRemoveAll(ctx context.Context, root string) error {
	for _, d := range worker.collectDedupDigests(root) {
		refKey := "digest/" + d
		_ = worker.refMutex.Lock(context.Background(), refKey)
		defer worker.refMutex.Unlock(refKey)
	}

	return utils.WithRetry(ctx, func() error {
		if err := os.RemoveAll(root); err != nil {
			return errors.Wrapf(err, "remove dir: %s", root)
		}
		return nil
	}, 60, 1*time.Second)
}

// collectDedupDigests returns the sorted, unique digests recorded by
// fully-pulled status.json files under root. Walk errors are ignored:
// this is best-effort serialization, not an inventory.
func (worker *Worker) collectDedupDigests(root string) []string {
	seen := map[string]struct{}{}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() || info.Name() != "status.json" {
			return nil
		}
		st, err := worker.sm.Get(path)
		if err != nil || st == nil || st.Digest == "" || !st.IsFullPull() {
			return nil
		}
		seen[st.Digest] = struct{}{}
		return nil
	})
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
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
	// Resolved lazily inside the inflight callback. Used both for dedup
	// matching and persisted to status.json so the dedup index survives a
	// driver restart even when references use mutable tags like ":latest".
	var digest string

	setStatus := func(state status.State) (*status.Status, error) {
		status, err := worker.sm.Set(statusPath, status.Status{
			VolumeName:          volumeName,
			MountID:             mountID,
			Reference:           reference,
			Digest:              digest,
			State:               state,
			ExcludeModelWeights: excludeModelWeights,
			ExcludeFilePatterns: excludeFilePatterns,
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

		// Resolve the manifest digest of the reference. Dedup is keyed by
		// digest (not reference) so that when a mutable tag like ":latest"
		// points to new content remotely, the stale local copy is NOT
		// incorrectly reused. If resolution fails (e.g. registry unreachable),
		// dedup is silently disabled for this pull and the underlying puller
		// will surface the same error if it is truly fatal.
		if d, err := resolveDigest(ctx, reference); err != nil {
			logger.WithContext(ctx).WithError(err).Warnf("resolve digest for %s failed, dedup disabled for this pull", reference)
		} else {
			digest = d
		}

		// Cross-volume dedup: for full-pull requests (no exclude variants), try
		// to hardlink from an existing successfully-pulled model dir of the
		// same reference. This avoids both duplicate network I/O and duplicate
		// disk usage when many pods on the same node mount the same model.
		//
		// The refMutex is held across the *entire* pull (including the real
		// pull below) so that concurrent requests for the same reference are
		// serialized: the first request performs the real pull, and the rest
		// observe the now-populated source dir and clone via hardlink.
		fullPull := !excludeModelWeights && len(excludeFilePatterns) == 0
		if fullPull && digest != "" {
			refKey := "digest/" + digest
			// refMutex.Lock with a Background context only fails on context
			// cancellation, which cannot happen here; ignore the unreachable
			// error to keep the dedup hot path branch-free.
			_ = worker.refMutex.Lock(context.Background(), refKey)
			defer worker.refMutex.Unlock(refKey)

			if srcModelDir := worker.findExistingModelDir(ctx, digest); srcModelDir != "" && srcModelDir != modelDir {
				// setStatus failures (disk full, fs RO) are surfaced by the
				// real-pull path below; for the optimistic clone fast path
				// we record best-effort progress and proceed.
				_, _ = setStatus(status.StatePullRunning)
				if err := cloneByHardlink(srcModelDir, modelDir); err == nil {
					_, _ = setStatus(status.StatePullSucceeded)
					logger.WithContext(ctx).Infof("cloned model from existing dir by hardlink: %s", reference)
					return nil, nil
				} else {
					logger.WithContext(ctx).WithError(err).Warnf("hardlink clone failed, fallback to real pull: %s -> %s", srcModelDir, modelDir)
					_ = os.RemoveAll(modelDir)
				}
			}
		}

		hook := status.NewHook(ctx)
		worker.sm.HookManager.Set(statusPath, hook)

		var diskQuotaChecker *DiskQuotaChecker
		checkDiskQuota := worker.cfg.Get().Features.CheckDiskQuota && checkDiskQuota && !worker.isModelExisted(ctx, digest)
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
		// For tag references, re-resolve so status records the digest of the
		// bytes actually pulled, not the pre-pull one (which may be stale if
		// the tag moved). On failure we keep the pre-pull digest.
		if parseDigestFromRef(reference) == "" {
			if d, err := resolveDigest(ctx, reference); err != nil {
				logger.WithContext(ctx).WithError(err).Warnf("re-resolve digest after pull failed for %s, keeping pre-pull digest", reference)
			} else if d != digest {
				logger.WithContext(ctx).Infof("digest for %s changed during pull: %s -> %s", reference, digest, d)
				digest = d
			}
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

func (worker *Worker) isModelExisted(ctx context.Context, digest string) bool {
	if digest == "" {
		return false
	}
	return worker.findExistingModelDir(ctx, digest) != ""
}

// findExistingModelDir returns the absolute path of the first model dir that
// contains a successfully-pulled copy of the given manifest digest, or "" if
// none is found. Matching by digest (rather than by reference) ensures that
// when a mutable tag like ":latest" points to new content remotely, the
// stale local copy is NOT incorrectly reused. It is used both by the
// disk-quota fast path and by the cross-volume dedup logic in pullModel.
func (worker *Worker) findExistingModelDir(ctx context.Context, digest string) string {
	if digest == "" {
		return ""
	}
	volumesDir := worker.cfg.Get().GetVolumesDir()
	volumeDirs, err := os.ReadDir(volumesDir)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.WithContext(ctx).WithError(err).Errorf("read volume dirs from %s", volumesDir)
		}
		return ""
	}

	// Returns the model sub-directory path if the volume's status indicates a
	// successful pull of the requested reference and the on-disk model dir
	// exists.
	mountedModelDir := func(volumeStatusDir string) string {
		st, err := worker.sm.Get(filepath.Join(volumeStatusDir, "status.json"))
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				logger.WithContext(ctx).WithError(err).Error("failed to get volume status")
			}
			return ""
		}
		if st.Digest == "" || st.Digest != digest {
			return ""
		}
		if st.State != status.StatePullSucceeded && st.State != status.StateMounted {
			return ""
		}
		// Skip partial-pull sources: same digest, fewer files on disk.
		if !st.IsFullPull() {
			return ""
		}
		modelDir := filepath.Join(volumeStatusDir, "model")
		if _, err := os.Stat(modelDir); err != nil {
			return ""
		}
		return modelDir
	}
	for _, volumeDir := range volumeDirs {
		if !volumeDir.IsDir() {
			continue
		}
		if isStaticVolume(volumeDir.Name()) {
			if dir := mountedModelDir(worker.cfg.Get().GetVolumeDir(volumeDir.Name())); dir != "" {
				return dir
			}
		}
		if isDynamicVolume(volumeDir.Name()) {
			// Static inline volumes also use the "csi-" prefix but follow
			// the static layout (volumes/<name>/model + status.json directly
			// under the volume dir, no "models/" sub-tree). Try that layout
			// first so inline pulls participate in dedup.
			if dir := mountedModelDir(worker.cfg.Get().GetVolumeDir(volumeDir.Name())); dir != "" {
				return dir
			}

			modelsDirForDynamic := worker.cfg.Get().GetModelsDirForDynamic(volumeDir.Name())
			modelDirs, err := os.ReadDir(modelsDirForDynamic)
			if err != nil {
				if !os.IsNotExist(err) {
					logger.WithContext(ctx).WithError(err).Errorf("failed to read model dirs from %s", modelsDirForDynamic)
				}
				continue
			}
			for _, modelDir := range modelDirs {
				if !modelDir.IsDir() {
					continue
				}

				mountID := modelDir.Name()
				if dir := mountedModelDir(worker.cfg.Get().GetMountIDDirForDynamic(volumeDir.Name(), mountID)); dir != "" {
					return dir
				}
			}
		}
	}

	return ""
}
