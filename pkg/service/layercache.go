package service

import (
	"context"
	"encoding/json"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"golang.org/x/sync/semaphore"
)

// Action describes the result of LayerCache.Acquire.
type Action int

const (
	// ActionPull means the caller should perform the actual pull and then
	// notify the cache via Publish/Fail.
	ActionPull Action = iota
	// ActionHit means the cache hardlinked an existing copy into the target
	// path; the caller can skip the download.
	ActionHit
)

// LayersFileName is the on-disk file used to persist the digest -> path
// mapping per volume / per dynamic mount, so the cache can be rebuilt after
// the daemonset restarts.
const LayersFileName = "layers.json"

type layerState int

const (
	stateIdle layerState = iota
	statePulling
	stateDone
)

type layerEntry struct {
	mu      sync.Mutex
	state   layerState
	paths   []string
	waiters []chan struct{} // per-waiter notification channels
}

func newLayerEntry() *layerEntry {
	return &layerEntry{state: stateIdle}
}

// notifyWaiters wakes all waiters and clears the slice. Caller must hold e.mu.
func (e *layerEntry) notifyWaiters() {
	for _, ch := range e.waiters {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	e.waiters = nil
}

// LayerCache is a process-wide registry of OCI layer digests that have been
// materialized somewhere under the volumes root. It serves two purposes:
//
//  1. Layer-level singleflight: when multiple pullers race on the same
//     digest, only the first one downloads while the others wait and then
//     hardlink the result.
//  2. Cross-mount disk dedup: when a previously pulled layer is requested
//     again from a different mount, the cache hardlinks the existing file
//     into the new target instead of re-downloading.
type LayerCache struct {
	cfg *config.Config
	sem *semaphore.Weighted

	mu    sync.Mutex
	items map[digest.Digest]*layerEntry

	// volume dir -> digest -> target path. Used to remove paths from the
	// in-memory index when a volume is deleted, and to drive the persisted
	// layers.json.
	perVolume map[string]map[digest.Digest]string

	dirtyMu    sync.Mutex
	dirtyVols  map[string]struct{}
	flushTimer *time.Timer
}

// NewLayerCache constructs an empty LayerCache.
func NewLayerCache(cfg *config.Config) *LayerCache {
	concurrency := cfg.Get().PullConfig.NodeLayerConcurrency
	if concurrency == 0 {
		concurrency = math.MaxInt64
	}

	return &LayerCache{
		cfg:       cfg,
		sem:       semaphore.NewWeighted(int64(concurrency)),
		items:     make(map[digest.Digest]*layerEntry),
		perVolume: make(map[string]map[digest.Digest]string),
	}
}

// getOrCreateEntry returns the entry for digest, creating it under the cache
// lock to avoid races between concurrent Acquire calls.
func (c *LayerCache) getOrCreateEntry(d digest.Digest) *layerEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[d]
	if !ok {
		e = newLayerEntry()
		c.items[d] = e
	}
	return e
}

// Acquire is the entry point used by the layerCacheHook in BeforePullLayer.
//
// It returns ActionHit when the target was successfully hardlinked from an
// existing copy, in which case the caller must NOT pull. It returns
// ActionPull to instruct the caller to perform the pull and then notify the
// cache via Publish/Fail.
func (c *LayerCache) Acquire(ctx context.Context, d digest.Digest, target string) (Action, error) {
	if d == "" || target == "" {
		return ActionPull, nil
	}
	e := c.getOrCreateEntry(d)

	e.mu.Lock()
	for {
		// Try to hardlink from any known path first.
		if action := c.tryHardlinkLocked(e, target); action == ActionHit {
			e.mu.Unlock()
			return ActionHit, nil
		}

		switch e.state {
		case stateIdle, stateDone:
			// Nothing usable on disk and no in-flight pull: become the
			// owner. Acquire the global concurrency semaphore first.
			e.mu.Unlock()
			if err := c.sem.Acquire(ctx, 1); err != nil {
				return ActionPull, err
			}
			e.mu.Lock()
			// Re-check state after re-acquiring lock; another goroutine
			// may have completed the pull while we waited on the semaphore.
			if action := c.tryHardlinkLocked(e, target); action == ActionHit {
				e.mu.Unlock()
				c.sem.Release(1)
				return ActionHit, nil
			}
			if e.state == statePulling {
				// Someone else took ownership while we waited on the semaphore.
				c.sem.Release(1)
				// Fall through to the waiting path below.
				goto wait
			}
			e.state = statePulling
			e.mu.Unlock()
			return ActionPull, nil
		case statePulling:
			goto wait
		}
	wait:
		// Another puller is downloading this digest; wait for it.
		ch := make(chan struct{}, 1)
		e.waiters = append(e.waiters, ch)
		e.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			// Remove our channel from waiters.
			e.mu.Lock()
			for i, w := range e.waiters {
				if w == ch {
					e.waiters = append(e.waiters[:i], e.waiters[i+1:]...)
					break
				}
			}
			e.mu.Unlock()
			return ActionPull, ctx.Err()
		}
		e.mu.Lock()
		// Loop and re-evaluate.
	}
}

// tryHardlinkLocked attempts to hardlink one of the known paths for the
// entry into target. Stale paths are pruned from the entry. Caller must hold
// e.mu.
func (c *LayerCache) tryHardlinkLocked(e *layerEntry, target string) Action {
	srcPaths := append([]string(nil), e.paths...)
	kept := make([]string, 0, len(srcPaths)+1)
	hit := false
	for _, p := range srcPaths {
		if p == target {
			if _, err := os.Stat(p); err == nil {
				kept = append(kept, p)
				hit = true
			}
			continue
		}
		if hit {
			if _, err := os.Stat(p); err == nil {
				kept = append(kept, p)
			}
			continue
		}
		if _, err := os.Stat(p); err != nil {
			// Stale; drop it.
			continue
		}
		if err := hardlink(p, target); err != nil {
			logger.Logger().WithError(err).Debugf("hardlink fallback for digest path: %s -> %s", p, target)
			// Keep the source; just couldn't link to this target.
			kept = append(kept, p)
			continue
		}
		kept = append(kept, p, target)
		hit = true
	}
	e.paths = uniqueStrings(kept)
	if hit {
		return ActionHit
	}
	return ActionPull
}

// Publish records that target now contains the blob for digest d, persists
// the mapping for the owning volume, and wakes any waiters.
func (c *LayerCache) Publish(d digest.Digest, target string) {
	if d == "" || target == "" {
		return
	}
	e := c.getOrCreateEntry(d)

	e.mu.Lock()
	if !slices.Contains(e.paths, target) {
		e.paths = append(e.paths, target)
	}
	wasPulling := e.state == statePulling
	e.state = stateDone
	e.notifyWaiters()
	e.mu.Unlock()

	// Release the semaphore only if this Publish corresponds to an actual
	// pull (not a cache-hit re-registration).
	if wasPulling {
		c.sem.Release(1)
	}

	c.recordVolumeMapping(d, target)
}

// Fail is called by the owner when the underlying pull failed; waiters are
// woken so they can either retry hardlinking from another path or take over
// as the new owner.
func (c *LayerCache) Fail(d digest.Digest) {
	if d == "" {
		return
	}
	c.mu.Lock()
	e, ok := c.items[d]
	c.mu.Unlock()
	if !ok {
		return
	}

	e.mu.Lock()
	wasPulling := e.state == statePulling
	if len(e.paths) == 0 {
		e.state = stateIdle
	} else {
		e.state = stateDone
	}
	e.notifyWaiters()
	e.mu.Unlock()

	if wasPulling {
		c.sem.Release(1)
	}
}

// OnVolumeRemoved is called by the worker after it removed the volume
// directory from disk. All in-memory paths under that directory are
// dropped, and the persisted layers.json is removed.
func (c *LayerCache) OnVolumeRemoved(volumeDir string) {
	if volumeDir == "" {
		return
	}
	c.mu.Lock()
	owned := c.perVolume[volumeDir]
	delete(c.perVolume, volumeDir)
	// Snapshot entries to mutate outside the cache lock.
	items := make(map[digest.Digest]*layerEntry, len(owned))
	for d := range owned {
		if e, ok := c.items[d]; ok {
			items[d] = e
		}
	}
	c.mu.Unlock()

	for d, p := range owned {
		e := items[d]
		if e == nil {
			continue
		}
		e.mu.Lock()
		out := e.paths[:0]
		for _, existing := range e.paths {
			if existing == p {
				continue
			}
			out = append(out, existing)
		}
		e.paths = out
		if len(e.paths) == 0 && e.state == stateDone {
			e.state = stateIdle
		}
		e.mu.Unlock()
	}
}

// Rebuild scans the configured volumes directory for previously persisted
// layers.json files and reconstructs the in-memory index. Stale entries
// (file no longer exists on disk) are dropped. It is safe to call once at
// process start before serving any pulls.
func (c *LayerCache) Rebuild(ctx context.Context) error {
	root := c.cfg.Get().GetVolumesDir()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.Wrapf(err, "read volumes dir: %s", root)
	}
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		volRoot := filepath.Join(root, ent.Name())
		c.loadLayersFile(ctx, volRoot)
		// Also visit dynamic per-mount dirs, where each mountID has its own
		// layers.json next to status.json.
		modelsDir := filepath.Join(volRoot, "models")
		if subEntries, err := os.ReadDir(modelsDir); err == nil {
			for _, sub := range subEntries {
				if !sub.IsDir() {
					continue
				}
				c.loadLayersFile(ctx, filepath.Join(modelsDir, sub.Name()))
			}
		}
	}
	return nil
}

func (c *LayerCache) loadLayersFile(ctx context.Context, dir string) {
	path := filepath.Join(dir, LayersFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	var f layersFile
	if err := json.Unmarshal(data, &f); err != nil {
		logger.WithContext(ctx).WithError(err).Warnf("invalid layers.json: %s", path)
		return
	}

	for _, item := range f.Items {
		if item.Digest == "" || item.Path == "" {
			continue
		}
		if _, err := os.Stat(item.Path); err != nil {
			continue
		}
		e := c.getOrCreateEntry(item.Digest)
		e.mu.Lock()
		if !slices.Contains(e.paths, item.Path) {
			e.paths = append(e.paths, item.Path)
		}
		e.state = stateDone
		e.mu.Unlock()
		c.indexVolumeMapping(dir, item.Digest, item.Path)
	}
}

// recordVolumeMapping derives the owning volume directory for a path and
// schedules persistence of the mapping to disk.
func (c *LayerCache) recordVolumeMapping(d digest.Digest, target string) {
	volDir := c.volumeDirFor(target)
	if volDir == "" {
		return
	}
	c.indexVolumeMapping(volDir, d, target)
	c.schedulePersist(volDir)
}

func (c *LayerCache) schedulePersist(volDir string) {
	c.dirtyMu.Lock()
	defer c.dirtyMu.Unlock()
	if c.dirtyVols == nil {
		c.dirtyVols = make(map[string]struct{})
	}
	c.dirtyVols[volDir] = struct{}{}
	if c.flushTimer == nil {
		c.flushTimer = time.AfterFunc(500*time.Millisecond, c.flushDirtyVolumes)
	} else {
		c.flushTimer.Reset(500 * time.Millisecond)
	}
}

func (c *LayerCache) flushDirtyVolumes() {
	c.dirtyMu.Lock()
	dirty := c.dirtyVols
	c.dirtyVols = nil
	c.flushTimer = nil
	c.dirtyMu.Unlock()

	for volDir := range dirty {
		c.mu.Lock()
		owned := c.perVolume[volDir]
		snapshot := make(map[digest.Digest]string, len(owned))
		maps.Copy(snapshot, owned)
		c.mu.Unlock()

		if err := writeLayersFile(volDir, snapshot); err != nil {
			logger.Logger().WithError(err).Warnf("persist layers.json for %s", volDir)
		}
	}
}

// FlushPersist forces an immediate write of all pending layers.json files.
// Useful for tests and graceful shutdown.
func (c *LayerCache) FlushPersist() {
	c.dirtyMu.Lock()
	if c.flushTimer != nil {
		c.flushTimer.Stop()
	}
	c.flushTimer = nil
	dirty := c.dirtyVols
	c.dirtyVols = nil
	c.dirtyMu.Unlock()

	for volDir := range dirty {
		c.mu.Lock()
		owned := c.perVolume[volDir]
		snapshot := make(map[digest.Digest]string, len(owned))
		maps.Copy(snapshot, owned)
		c.mu.Unlock()

		if err := writeLayersFile(volDir, snapshot); err != nil {
			logger.Logger().WithError(err).Warnf("persist layers.json for %s", volDir)
		}
	}
}

func (c *LayerCache) indexVolumeMapping(volDir string, d digest.Digest, target string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.perVolume[volDir]
	if !ok {
		m = make(map[digest.Digest]string)
		c.perVolume[volDir] = m
	}
	m[d] = target
}

// volumeDirFor returns the directory whose layers.json owns target. For a
// static volume that is `<volumes>/<volumeName>`; for a dynamic volume that
// is `<volumes>/<volumeName>/models/<mountID>`. An empty string is returned
// when target is not under the configured volumes root.
func (c *LayerCache) volumeDirFor(target string) string {
	root := c.cfg.Get().GetVolumesDir()
	rel, err := filepath.Rel(root, target)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) == 0 {
		return ""
	}
	volume := parts[0]
	if len(parts) >= 3 && parts[1] == "models" {
		return filepath.Join(root, volume, "models", parts[2])
	}
	return filepath.Join(root, volume)
}

// ---- helpers ---------------------------------------------------------------

type layersFileItem struct {
	Digest digest.Digest `json:"digest"`
	Path   string        `json:"path"`
}

type layersFile struct {
	Schema int              `json:"schema"`
	Items  []layersFileItem `json:"items"`
}

func writeLayersFile(dir string, items map[digest.Digest]string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return errors.Wrap(err, "mkdir layers dir")
	}
	out := layersFile{Schema: 1}
	for d, p := range items {
		out.Items = append(out.Items, layersFileItem{Digest: d, Path: p})
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].Digest < out.Items[j].Digest })
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return errors.Wrap(err, "marshal layers.json")
	}
	tmp := filepath.Join(dir, LayersFileName+".tmp")
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return errors.Wrap(err, "write tmp layers.json")
	}
	if err := os.Rename(tmp, filepath.Join(dir, LayersFileName)); err != nil {
		return errors.Wrap(err, "rename layers.json")
	}
	return nil
}

// hardlink links src to dst, ensuring the target directory exists and that
// no stale file occupies the destination.
func hardlink(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return errors.Wrap(err, "mkdir hardlink target")
	}
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return errors.Wrap(err, "remove stale hardlink target")
	}
	if err := os.Link(src, dst); err != nil {
		return errors.Wrap(err, "create hardlink")
	}
	return nil
}

func uniqueStrings(in []string) []string {
	if len(in) <= 1 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := in[:0]
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
