package service

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/status"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

// ─── LayerCache basic ops ─────────────────────────────────────────────────────

func TestLayerCache_RegisterAndLookup(t *testing.T) {
	lc := NewLayerCache(8)

	d := digest.FromString("layer-content-1")
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "layer1.bin")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0644))

	lc.Register(d, path)

	found, ok := lc.Lookup(d)
	require.True(t, ok)
	require.Equal(t, path, found)
}

func TestLayerCache_LookupMissing(t *testing.T) {
	lc := NewLayerCache(8)
	d := digest.FromString("nonexistent")
	_, ok := lc.Lookup(d)
	require.False(t, ok)
}

func TestLayerCache_LookupStaleFile(t *testing.T) {
	lc := NewLayerCache(8)
	d := digest.FromString("stale-content")
	path := filepath.Join(t.TempDir(), "deleted.bin")

	// Register a path that doesn't exist on disk.
	lc.Register(d, path)

	_, ok := lc.Lookup(d)
	require.False(t, ok, "should not find stale path")
}

func TestLayerCache_RegisterIdempotent(t *testing.T) {
	lc := NewLayerCache(8)
	d := digest.FromString("dup-content")
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "layer.bin")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0644))

	lc.Register(d, path)
	lc.Register(d, path)
	lc.Register(d, path)

	require.Equal(t, 1, lc.PathCount())
}

func TestLayerCache_MultiplePaths(t *testing.T) {
	lc := NewLayerCache(8)
	d := digest.FromString("shared-content")
	tmpDir := t.TempDir()

	path1 := filepath.Join(tmpDir, "vol1", "layer.bin")
	path2 := filepath.Join(tmpDir, "vol2", "layer.bin")
	require.NoError(t, os.MkdirAll(filepath.Dir(path1), 0755))
	require.NoError(t, os.MkdirAll(filepath.Dir(path2), 0755))
	require.NoError(t, os.WriteFile(path1, []byte("data"), 0644))
	require.NoError(t, os.WriteFile(path2, []byte("data"), 0644))

	lc.Register(d, path1)
	lc.Register(d, path2)

	require.Equal(t, 2, lc.PathCount())
	require.Equal(t, 1, lc.Len())

	found, ok := lc.Lookup(d)
	require.True(t, ok)
	require.Contains(t, []string{path1, path2}, found)
}

// ─── LayerCache Remove ────────────────────────────────────────────────────────

func TestLayerCache_Remove(t *testing.T) {
	lc := NewLayerCache(8)
	d := digest.FromString("remove-test")
	tmpDir := t.TempDir()

	path1 := filepath.Join(tmpDir, "layer1.bin")
	path2 := filepath.Join(tmpDir, "layer2.bin")
	require.NoError(t, os.WriteFile(path1, []byte("data"), 0644))
	require.NoError(t, os.WriteFile(path2, []byte("data"), 0644))

	lc.Register(d, path1)
	lc.Register(d, path2)

	lc.Remove(path1)
	require.Equal(t, 1, lc.PathCount())

	found, ok := lc.Lookup(d)
	require.True(t, ok)
	require.Equal(t, path2, found)
}

func TestLayerCache_RemoveLastPath(t *testing.T) {
	lc := NewLayerCache(8)
	d := digest.FromString("remove-last")
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "only.bin")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0644))

	lc.Register(d, path)
	lc.Remove(path)

	require.Equal(t, 0, lc.Len())
	require.Equal(t, 0, lc.PathCount())
}

// ─── LayerCache RemoveByPrefix ────────────────────────────────────────────────

func TestLayerCache_RemoveByPrefix(t *testing.T) {
	lc := NewLayerCache(8)
	tmpDir := t.TempDir()

	// Create two volumes with layers.
	d1 := digest.FromString("layer-a")
	d2 := digest.FromString("layer-b")
	d3 := digest.FromString("layer-c")

	vol1Path := filepath.Join(tmpDir, "vol1", "model", "a.bin")
	vol1PathB := filepath.Join(tmpDir, "vol1", "model", "b.bin")
	vol2Path := filepath.Join(tmpDir, "vol2", "model", "c.bin")

	for _, p := range []string{vol1Path, vol1PathB, vol2Path} {
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0755))
		require.NoError(t, os.WriteFile(p, []byte("data"), 0644))
	}

	lc.Register(d1, vol1Path)
	lc.Register(d2, vol1PathB)
	lc.Register(d3, vol2Path)

	require.Equal(t, 3, lc.Len())

	// Remove all entries under vol1.
	lc.RemoveByPrefix(filepath.Join(tmpDir, "vol1"))

	require.Equal(t, 1, lc.Len())
	require.Equal(t, 1, lc.PathCount())

	// vol2 entry should still be there.
	found, ok := lc.Lookup(d3)
	require.True(t, ok)
	require.Equal(t, vol2Path, found)
}

// ─── LayerCache Snapshot ──────────────────────────────────────────────────────

func TestLayerCache_Snapshot(t *testing.T) {
	lc := NewLayerCache(8)
	d := digest.FromString("snap-content")
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "snap.bin")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0644))

	lc.Register(d, path)

	snap := lc.Snapshot()
	require.Len(t, snap, 1)
	require.Equal(t, []string{path}, snap[d])

	// Modifying snapshot should not affect the cache.
	snap[d] = append(snap[d], "extra")
	require.Equal(t, 1, lc.PathCount())
}

// ─── LayerCache Concurrency ───────────────────────────────────────────────────

func TestLayerCache_ConcurrentRegisterAndLookup(t *testing.T) {
	lc := NewLayerCache(8)
	tmpDir := t.TempDir()

	const n = 50
	digests := make([]digest.Digest, n)
	paths := make([]string, n)
	for i := 0; i < n; i++ {
		digests[i] = digest.FromString(string(rune(i)))
		paths[i] = filepath.Join(tmpDir, string(rune('a'+i)))
		require.NoError(t, os.WriteFile(paths[i], []byte("data"), 0644))
	}

	var wg sync.WaitGroup
	// Concurrently register.
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lc.Register(digests[i], paths[i])
		}(i)
	}
	wg.Wait()

	// Concurrently lookup.
	var found atomic.Int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, ok := lc.Lookup(digests[i]); ok {
				found.Add(1)
			}
		}(i)
	}
	wg.Wait()

	require.Equal(t, int32(n), found.Load())
}

func TestLayerCache_ConcurrentSameLayerRegister(t *testing.T) {
	lc := NewLayerCache(8)
	d := digest.FromString("same-layer")
	tmpDir := t.TempDir()

	const n = 20
	paths := make([]string, n)
	for i := 0; i < n; i++ {
		paths[i] = filepath.Join(tmpDir, string(rune('a'+i)))
		require.NoError(t, os.WriteFile(paths[i], []byte("data"), 0644))
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lc.Register(d, paths[i])
		}(i)
	}
	wg.Wait()

	// All n paths should be registered under one digest.
	require.Equal(t, 1, lc.Len())
	require.Equal(t, n, lc.PathCount())
}

// ─── LayerCache Rebuild ───────────────────────────────────────────────────────

func TestLayerCache_Rebuild(t *testing.T) {
	tmpDir := t.TempDir()
	rawCfg := &config.RawConfig{ServiceName: "test", RootDir: tmpDir}
	cfg := config.NewWithRaw(rawCfg)
	sm, err := status.NewStatusManager()
	require.NoError(t, err)

	// Set up a static volume with a successful pull.
	volumeName := "pvc-rebuild-test"
	volumeDir := cfg.Get().GetVolumeDir(volumeName)
	modelDir := cfg.Get().GetModelDir(volumeName)
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	// Write status.
	statusPath := filepath.Join(volumeDir, "status.json")
	_, err = sm.Set(statusPath, status.Status{
		VolumeName: volumeName,
		Reference:  "registry/model:v1",
		State:      status.StatePullSucceeded,
	})
	require.NoError(t, err)

	// Write a model file.
	layerFile := filepath.Join(modelDir, "weights.bin")
	require.NoError(t, os.WriteFile(layerFile, []byte("layer-data"), 0644))

	// Write layer metadata.
	d := digest.FromString("weights-content")
	metadataPath := filepath.Join(volumeDir, "layer_digests.json")
	require.NoError(t, saveLayerMetadata(metadataPath, []LayerMetadataEntry{
		{Digest: d.String(), FilePath: "weights.bin", Size: 10},
	}))

	// Rebuild.
	lc := NewLayerCache(8)
	lc.Rebuild(cfg.Get(), sm)

	require.Equal(t, 1, lc.Len())

	found, ok := lc.Lookup(d)
	require.True(t, ok)
	require.Equal(t, layerFile, found)
}

func TestLayerCache_RebuildSkipsFailedPull(t *testing.T) {
	tmpDir := t.TempDir()
	rawCfg := &config.RawConfig{ServiceName: "test", RootDir: tmpDir}
	cfg := config.NewWithRaw(rawCfg)
	sm, err := status.NewStatusManager()
	require.NoError(t, err)

	volumeName := "pvc-failed-test"
	volumeDir := cfg.Get().GetVolumeDir(volumeName)
	modelDir := cfg.Get().GetModelDir(volumeName)
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	statusPath := filepath.Join(volumeDir, "status.json")
	_, err = sm.Set(statusPath, status.Status{
		VolumeName: volumeName,
		Reference:  "registry/model:v1",
		State:      status.StatePullFailed,
	})
	require.NoError(t, err)

	lc := NewLayerCache(8)
	lc.Rebuild(cfg.Get(), sm)

	require.Equal(t, 0, lc.Len(), "should not rebuild from failed pull")
}

func TestLayerCache_RebuildDynamicVolume(t *testing.T) {
	tmpDir := t.TempDir()
	rawCfg := &config.RawConfig{ServiceName: "test", RootDir: tmpDir}
	cfg := config.NewWithRaw(rawCfg)
	sm, err := status.NewStatusManager()
	require.NoError(t, err)

	volumeName := "csi-dyn-rebuild"
	mountID := "mount-1"
	mountDir := cfg.Get().GetMountIDDirForDynamic(volumeName, mountID)
	modelDir := cfg.Get().GetModelDirForDynamic(volumeName, mountID)
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	statusPath := filepath.Join(mountDir, "status.json")
	_, err = sm.Set(statusPath, status.Status{
		Reference: "registry/model:dyn",
		State:     status.StateMounted,
	})
	require.NoError(t, err)

	layerFile := filepath.Join(modelDir, "model.bin")
	require.NoError(t, os.WriteFile(layerFile, []byte("dyn-data"), 0644))

	d := digest.FromString("dyn-content")
	metadataPath := filepath.Join(mountDir, "layer_digests.json")
	require.NoError(t, saveLayerMetadata(metadataPath, []LayerMetadataEntry{
		{Digest: d.String(), FilePath: "model.bin", Size: 8},
	}))

	lc := NewLayerCache(8)
	lc.Rebuild(cfg.Get(), sm)

	require.Equal(t, 1, lc.Len())
	found, ok := lc.Lookup(d)
	require.True(t, ok)
	require.Equal(t, layerFile, found)
}

func TestLayerCache_RebuildSkipsMissingFile(t *testing.T) {
	tmpDir := t.TempDir()
	rawCfg := &config.RawConfig{ServiceName: "test", RootDir: tmpDir}
	cfg := config.NewWithRaw(rawCfg)
	sm, err := status.NewStatusManager()
	require.NoError(t, err)

	volumeName := "pvc-missing-file"
	volumeDir := cfg.Get().GetVolumeDir(volumeName)
	modelDir := cfg.Get().GetModelDir(volumeName)
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	statusPath := filepath.Join(volumeDir, "status.json")
	_, err = sm.Set(statusPath, status.Status{
		VolumeName: volumeName,
		Reference:  "registry/model:v1",
		State:      status.StatePullSucceeded,
	})
	require.NoError(t, err)

	d := digest.FromString("missing-file-content")
	metadataPath := filepath.Join(volumeDir, "layer_digests.json")
	require.NoError(t, saveLayerMetadata(metadataPath, []LayerMetadataEntry{
		{Digest: d.String(), FilePath: "does_not_exist.bin", Size: 10},
	}))

	lc := NewLayerCache(8)
	lc.Rebuild(cfg.Get(), sm)

	require.Equal(t, 0, lc.Len(), "should skip missing files during rebuild")
}

// ─── LayerCache Cleanup Integration ───────────────────────────────────────────

func TestLayerCache_CleanupOnDeleteModel(t *testing.T) {
	tmpDir := t.TempDir()
	rawCfg := &config.RawConfig{ServiceName: "test", RootDir: tmpDir}
	cfg := config.NewWithRaw(rawCfg)
	sm, err := status.NewStatusManager()
	require.NoError(t, err)

	lc := NewLayerCache(8)

	worker, err := NewWorkerWithLayerCache(cfg, sm, lc)
	require.NoError(t, err)

	// Simulate a pulled volume with cached layers.
	volumeName := "pvc-cleanup-test"
	modelDir := cfg.Get().GetModelDir(volumeName)
	require.NoError(t, os.MkdirAll(modelDir, 0755))

	d := digest.FromString("cleanup-content")
	layerFile := filepath.Join(modelDir, "weights.bin")
	require.NoError(t, os.WriteFile(layerFile, []byte("data"), 0644))
	lc.Register(d, layerFile)

	require.Equal(t, 1, lc.Len())

	// Delete the volume.
	err = worker.DeleteModel(context.Background(), true, volumeName, "")
	require.NoError(t, err)

	// Layer cache should be cleaned.
	require.Equal(t, 0, lc.Len(), "layer cache should be empty after delete")
}

// ─── Hardlink fallback tests ──────────────────────────────────────────────────

func TestLayerCache_HardlinkFallbackOnMissingSource(t *testing.T) {
	lc := NewLayerCache(8)
	d := digest.FromString("hardlink-source-missing")

	// Register a path that won't exist on disk.
	lc.Register(d, "/nonexistent/path/layer.bin")

	// Lookup should fail (source doesn't exist).
	_, ok := lc.Lookup(d)
	require.False(t, ok, "should not find source that doesn't exist")
}

func TestLayerCache_HardlinkFallbackUsesSecondPath(t *testing.T) {
	lc := NewLayerCache(8)
	d := digest.FromString("multi-path-fallback")
	tmpDir := t.TempDir()

	// First path doesn't exist, second does.
	path1 := filepath.Join(tmpDir, "deleted.bin")
	path2 := filepath.Join(tmpDir, "exists.bin")
	require.NoError(t, os.WriteFile(path2, []byte("data"), 0644))

	lc.Register(d, path1)
	lc.Register(d, path2)

	found, ok := lc.Lookup(d)
	require.True(t, ok)
	require.Equal(t, path2, found)
}

// ─── Layer metadata persistence ───────────────────────────────────────────────

func TestLayerMetadata_SaveAndLoad(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "layer_digests.json")

	entries := []LayerMetadataEntry{
		{Digest: "sha256:abc", FilePath: "weights.bin", Size: 100},
		{Digest: "sha256:def", FilePath: "config.json", Size: 50},
	}

	require.NoError(t, saveLayerMetadata(path, entries))

	loaded, err := loadLayerMetadata(path)
	require.NoError(t, err)
	require.Equal(t, entries, loaded)
}

func TestLayerMetadata_LoadMissing(t *testing.T) {
	_, err := loadLayerMetadata("/nonexistent/path")
	require.Error(t, err)
}

// ─── Semaphore / flow control ─────────────────────────────────────────────────

func TestLayerCache_SemaphoreBasic(t *testing.T) {
	lc := NewLayerCache(2)
	sem := lc.Semaphore()
	require.NotNil(t, sem)

	// Should be able to acquire 2 slots.
	require.True(t, sem.TryAcquire(1))
	require.True(t, sem.TryAcquire(1))
	// Third should fail (limit is 2).
	require.False(t, sem.TryAcquire(1))

	sem.Release(1)
	require.True(t, sem.TryAcquire(1))
}

func TestLayerCache_DefaultConcurrency(t *testing.T) {
	lc := NewLayerCache(0) // Should use default.
	sem := lc.Semaphore()
	require.NotNil(t, sem)

	// Default is 8, should be able to acquire 8.
	for i := 0; i < 8; i++ {
		require.True(t, sem.TryAcquire(1))
	}
	require.False(t, sem.TryAcquire(1))
}

// ─── Worker with LayerCache integration ───────────────────────────────────────

func TestNewWorkerWithLayerCache(t *testing.T) {
	tmpDir := t.TempDir()
	rawCfg := &config.RawConfig{ServiceName: "test", RootDir: tmpDir}
	cfg := config.NewWithRaw(rawCfg)
	sm, err := status.NewStatusManager()
	require.NoError(t, err)

	lc := NewLayerCache(8)

	worker, err := NewWorkerWithLayerCache(cfg, sm, lc)
	require.NoError(t, err)
	require.NotNil(t, worker)
	require.NotNil(t, worker.layerCache)
}

func TestNewWorkerWithNilLayerCache(t *testing.T) {
	tmpDir := t.TempDir()
	rawCfg := &config.RawConfig{ServiceName: "test", RootDir: tmpDir}
	cfg := config.NewWithRaw(rawCfg)
	sm, err := status.NewStatusManager()
	require.NoError(t, err)

	// Should use the standard puller when lc is nil.
	worker, err := NewWorkerWithLayerCache(cfg, sm, nil)
	require.NoError(t, err)
	require.NotNil(t, worker)
	require.Nil(t, worker.layerCache)
}

// ─── Concurrent dedup via singleflight ────────────────────────────────────────

func TestLayerCache_SingleflightDedup(t *testing.T) {
	lc := NewLayerCache(8)
	sfg := lc.SflightGroup()

	var callCount atomic.Int32

	d := digest.FromString("singleflight-test")
	key := d.String()

	const n = 10
	var started atomic.Int32

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started.Add(1)
			// Wait until all goroutines have started.
			for started.Load() < n {
				runtime.Gosched()
			}
			_, _, _ = sfg.Do(key, func() (interface{}, error) {
				callCount.Add(1)
				// Sleep to keep the function in-flight while other goroutines arrive.
				time.Sleep(50 * time.Millisecond)
				return nil, nil
			})
		}()
	}

	wg.Wait()

	// Due to singleflight, the function should be called exactly once
	// because all goroutines overlap on the same key while blocked.
	require.Equal(t, int32(1), callCount.Load(),
		"singleflight should deduplicate concurrent calls")
}

// ─── getLayerFilePath ─────────────────────────────────────────────────────────

func TestGetLayerFilePath_NoAnnotations(t *testing.T) {
	desc := createTestDescriptor("sha256:abc123", nil)
	require.Equal(t, "", getLayerFilePath(desc))
}

func TestGetLayerFilePath_CurrentSpec(t *testing.T) {
	desc := createTestDescriptor("sha256:abc123", map[string]string{
		"org.cncf.model.filepath": "weights/model.safetensors",
	})
	require.Equal(t, "weights/model.safetensors", getLayerFilePath(desc))
}

func TestGetLayerFilePath_LegacySpec(t *testing.T) {
	desc := createTestDescriptor("sha256:abc123", map[string]string{
		"org.cnai.model.filepath": "config.json",
	})
	require.Equal(t, "config.json", getLayerFilePath(desc))
}

func TestGetLayerFilePath_PrefersCurrentOverLegacy(t *testing.T) {
	desc := createTestDescriptor("sha256:abc123", map[string]string{
		"org.cncf.model.filepath": "current.bin",
		"org.cnai.model.filepath": "legacy.bin",
	})
	require.Equal(t, "current.bin", getLayerFilePath(desc))
}

// ─── Test helpers ─────────────────────────────────────────────────────────────

func createTestDescriptor(digestStr string, annotations map[string]string) ocispec.Descriptor {
	d, _ := digest.Parse(digestStr)
	return ocispec.Descriptor{
		Digest:      d,
		Size:        100,
		Annotations: annotations,
	}
}
