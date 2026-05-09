package service

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/modelpack/model-csi-driver/pkg/status"
	"github.com/opencontainers/go-digest"
	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"
)

// defaultMaxConcurrentLayers is the default maximum number of layers that
// can be pulled concurrently across all volumes on a node.
const defaultMaxConcurrentLayers int64 = 8

// LayerCache maintains an in-memory mapping of layer digest → file paths
// on disk, enabling layer-level deduplication via hardlinks.
//
// Thread-safety: all methods are safe for concurrent use.
type LayerCache struct {
	mu sync.RWMutex
	// layers maps a layer digest to all known on-disk file paths containing
	// that layer's content. Multiple volumes can reference the same layer.
	layers map[digest.Digest][]string

	// sfGroup deduplicates concurrent pull requests for the same layer digest.
	// The key is the digest string.
	sfGroup singleflight.Group

	// sem controls the maximum number of concurrently in-flight layer pulls
	// at the node level, preventing uncontrolled network and disk IO fan-out.
	sem *semaphore.Weighted
}

// NewLayerCache creates a new empty LayerCache with the given concurrency limit.
func NewLayerCache(maxConcurrentLayers int64) *LayerCache {
	if maxConcurrentLayers <= 0 {
		maxConcurrentLayers = defaultMaxConcurrentLayers
	}
	return &LayerCache{
		layers: make(map[digest.Digest][]string),
		sem:    semaphore.NewWeighted(maxConcurrentLayers),
	}
}

// Semaphore returns the weighted semaphore for node-level flow control.
func (lc *LayerCache) Semaphore() *semaphore.Weighted {
	return lc.sem
}

// SflightGroup returns the singleflight group for layer-level dedup.
func (lc *LayerCache) SflightGroup() *singleflight.Group {
	return &lc.sfGroup
}

// Register adds a file path for a given layer digest.
// If the path is already registered for the digest, this is a no-op.
func (lc *LayerCache) Register(d digest.Digest, path string) {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	paths := lc.layers[d]
	for _, p := range paths {
		if p == path {
			return // already registered
		}
	}
	lc.layers[d] = append(paths, path)
}

// Lookup returns an existing, valid file path for the given layer digest.
// It verifies the file still exists on disk before returning it.
// Returns ("", false) if no valid path is found.
func (lc *LayerCache) Lookup(d digest.Digest) (string, bool) {
	lc.mu.RLock()
	paths := lc.layers[d]
	lc.mu.RUnlock()

	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// Remove removes a specific path from all digest entries.
func (lc *LayerCache) Remove(path string) {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	for d, paths := range lc.layers {
		filtered := paths[:0]
		for _, p := range paths {
			if p != path {
				filtered = append(filtered, p)
			}
		}
		if len(filtered) == 0 {
			delete(lc.layers, d)
		} else {
			lc.layers[d] = filtered
		}
	}
}

// RemoveByPrefix removes all paths that have the given prefix from all digest
// entries. This is used during volume cleanup to evict all layer references
// under a volume directory.
func (lc *LayerCache) RemoveByPrefix(prefix string) {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	for d, paths := range lc.layers {
		filtered := paths[:0]
		for _, p := range paths {
			if !strings.HasPrefix(p, prefix) {
				filtered = append(filtered, p)
			}
		}
		if len(filtered) == 0 {
			delete(lc.layers, d)
		} else {
			lc.layers[d] = filtered
		}
	}
}

// Len returns the number of unique digests tracked.
func (lc *LayerCache) Len() int {
	lc.mu.RLock()
	defer lc.mu.RUnlock()
	return len(lc.layers)
}

// PathCount returns the total number of paths tracked across all digests.
func (lc *LayerCache) PathCount() int {
	lc.mu.RLock()
	defer lc.mu.RUnlock()
	count := 0
	for _, paths := range lc.layers {
		count += len(paths)
	}
	return count
}

// Snapshot returns a copy of the current cache state for testing/debugging.
func (lc *LayerCache) Snapshot() map[digest.Digest][]string {
	lc.mu.RLock()
	defer lc.mu.RUnlock()
	snapshot := make(map[digest.Digest][]string, len(lc.layers))
	for d, paths := range lc.layers {
		cp := make([]string, len(paths))
		copy(cp, paths)
		snapshot[d] = cp
	}
	return snapshot
}

// Rebuild scans existing volume directories and re-populates the cache
// from disk. This is called on startup to restore layer cache state after
// a daemon restart or node reboot.
//
// For each volume with a successful pull status, we scan the model directory
// and register all files. The digest mapping is restored from the status
// metadata stored during previous pulls.
func (lc *LayerCache) Rebuild(cfg *config.RawConfig, sm *status.StatusManager) {
	volumesDir := cfg.GetVolumesDir()
	volumeDirs, err := os.ReadDir(volumesDir)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Logger().WithError(err).Errorf("layer cache rebuild: read volumes dir %s", volumesDir)
		}
		return
	}

	registered := 0
	for _, volumeDir := range volumeDirs {
		if !volumeDir.IsDir() {
			continue
		}
		volumeName := volumeDir.Name()

		if isStaticVolume(volumeName) {
			n := lc.rebuildVolume(cfg.GetVolumeDir(volumeName), sm)
			registered += n
		}

		if isDynamicVolume(volumeName) {
			modelsDir := cfg.GetModelsDirForDynamic(volumeName)
			modelDirs, err := os.ReadDir(modelsDir)
			if err != nil {
				continue
			}
			for _, modelDir := range modelDirs {
				if !modelDir.IsDir() {
					continue
				}
				mountID := modelDir.Name()
				n := lc.rebuildVolume(cfg.GetMountIDDirForDynamic(volumeName, mountID), sm)
				registered += n
			}
		}
	}

	logger.Logger().Infof("layer cache rebuild complete: %d layer-path entries registered, %d unique digests",
		registered, lc.Len())
}

// rebuildVolume scans a single volume's model directory and registers
// any files found. It reads the layer digest metadata stored alongside
// the volume status to map files back to their digests.
func (lc *LayerCache) rebuildVolume(volumeDir string, sm *status.StatusManager) int {
	statusPath := filepath.Join(volumeDir, "status.json")
	s, err := sm.Get(statusPath)
	if err != nil {
		return 0
	}

	// Only rebuild from successfully pulled volumes.
	if s.State != status.StatePullSucceeded && s.State != status.StateMounted {
		return 0
	}

	// Read the layer digest metadata file.
	metadataPath := filepath.Join(volumeDir, "layer_digests.json")
	metadata, err := loadLayerMetadata(metadataPath)
	if err != nil {
		return 0
	}

	registered := 0
	modelDir := filepath.Join(volumeDir, "model")
	for _, entry := range metadata {
		filePath := filepath.Join(modelDir, entry.FilePath)
		if _, err := os.Stat(filePath); err != nil {
			continue // file no longer exists, skip
		}
		d, err := digest.Parse(entry.Digest)
		if err != nil {
			continue
		}
		lc.Register(d, filePath)
		registered++
	}

	return registered
}
