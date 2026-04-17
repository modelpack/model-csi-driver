package service

import (
	"os"
	"path/filepath"
	"time"

	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/modelpack/model-csi-driver/pkg/metrics"
	"github.com/modelpack/model-csi-driver/pkg/status"
	"github.com/pkg/errors"
)

var (
	CacheScanInterval = 60 * time.Second
	CacheGCInterval   = 10 * time.Minute
	CacheTTL          = 24 * time.Hour
)

const (
	mountTypePVC     = "pvc"
	mountTypeInline  = "inline"
	mountTypeDynamic = "dynamic"
)

type CacheManager struct {
	cfg *config.Config
	sm  *status.StatusManager
}

// volumeStatusInfo holds the context for each status file found during volume traversal.
type volumeStatusInfo struct {
	mountType  string
	volumeName string
	status     *status.Status
}

// forEachVolumeStatus walks all volume directories and invokes fn for every
// status file that can be read successfully.
func (cm *CacheManager) forEachVolumeStatus(fn func(info volumeStatusInfo)) error {
	volumesDir := cm.cfg.Get().GetVolumesDir()
	volumeDirs, err := os.ReadDir(volumesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.Wrapf(err, "read volume dirs from %s", volumesDir)
	}

	tryCall := func(statusPath, mountType, volumeName string) {
		st, err := cm.sm.Get(statusPath)
		if err != nil {
			return
		}
		fn(volumeStatusInfo{mountType: mountType, volumeName: volumeName, status: st})
	}

	for _, volumeDir := range volumeDirs {
		if !volumeDir.IsDir() {
			continue
		}
		volumeName := volumeDir.Name()

		if isStaticVolume(volumeName) {
			tryCall(filepath.Join(volumesDir, volumeName, "status.json"), mountTypePVC, volumeName)
			continue
		}
		if !isDynamicVolume(volumeName) {
			continue
		}

		modelsDir := cm.cfg.Get().GetModelsDirForDynamic(volumeName)
		modelDirs, err := os.ReadDir(modelsDir)
		if err != nil {
			if os.IsNotExist(err) {
				// This is potentially an inline model, the status file is expected
				// to be directly under the volume directory.
				tryCall(filepath.Join(volumesDir, volumeName, "status.json"), mountTypeInline, volumeName)
				continue
			}
			logger.Logger().WithError(err).Warnf("read model dirs from %s", modelsDir)
			continue
		}
		for _, modelDir := range modelDirs {
			if !modelDir.IsDir() {
				continue
			}
			tryCall(filepath.Join(modelsDir, modelDir.Name(), "status.json"), mountTypeDynamic, volumeName)
		}
	}

	return nil
}

func (cm *CacheManager) getCacheSize() (int64, error) {
	size, err := getUsedSize(cm.cfg.Get().RootDir)
	if err != nil {
		return 0, errors.Wrapf(err, "get used size: %s", cm.cfg.Get().RootDir)
	}
	return size, nil
}

func (cm *CacheManager) scanModels() error {
	pvcModels := 0
	inlineModels := 0
	dynamicModels := 0
	var mountItems []metrics.MountItem

	err := cm.forEachVolumeStatus(func(info volumeStatusInfo) {
		mountItems = append(mountItems, metrics.MountItem{
			Reference:  info.status.Reference,
			Type:       info.mountType,
			VolumeName: info.volumeName,
			MountID:    info.status.MountID,
		})
		switch info.mountType {
		case mountTypePVC:
			pvcModels++
		case mountTypeInline:
			inlineModels++
		case mountTypeDynamic:
			dynamicModels++
		}
	})
	if err != nil {
		return err
	}

	metrics.MountItems.Set(mountItems)
	metrics.NodeMountedPVCModels.Set(float64(pvcModels))
	metrics.NodeMountedInlineModels.Set(float64(inlineModels))
	metrics.NodeMountedDynamicModels.Set(float64(dynamicModels))

	return nil
}

func (cm *CacheManager) Scan() error {
	cacheSize, err := cm.getCacheSize()
	if err != nil {
		return errors.Wrapf(err, "scan cache from %s", cm.cfg.Get().RootDir)
	}
	metrics.NodeCacheSizeInBytes.Set(float64(cacheSize))

	if err := cm.scanModels(); err != nil {
		return errors.Wrapf(err, "scan models")
	}

	return nil
}

// activeCacheKeys returns the set of cache keys that are still referenced by
// at least one mounted volume.
func (cm *CacheManager) activeCacheKeys() (map[string]struct{}, error) {
	active := map[string]struct{}{}
	err := cm.forEachVolumeStatus(func(info volumeStatusInfo) {
		if info.status.ResolvedDigest == "" {
			return
		}
		active[cm.cfg.Get().GetCacheKey(info.status.ResolvedDigest)] = struct{}{}
	})
	return active, err
}

// gc removes cache directories that are not referenced by any mounted volume
// and whose modification time is older than CacheTTL.
func (cm *CacheManager) gc() error {
	active, err := cm.activeCacheKeys()
	if err != nil {
		return err
	}

	cacheRoot := cm.cfg.Get().GetCacheSHA256Dir()
	entries, err := os.ReadDir(cacheRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.Wrapf(err, "read cache root: %s", cacheRoot)
	}

	deadline := time.Now().Add(-CacheTTL)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if _, ok := active[name]; ok {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			logger.Logger().WithError(err).Warnf("stat cache dir: %s", name)
			continue
		}
		if info.ModTime().After(deadline) {
			continue
		}
		dirPath := filepath.Join(cacheRoot, name)
		if err := os.RemoveAll(dirPath); err != nil {
			logger.Logger().WithError(err).Warnf("remove cache dir: %s", dirPath)
			continue
		}
		logger.Logger().Infof("removed expired cache dir: %s", dirPath)
	}

	return nil
}

func NewCacheManager(cfg *config.Config, sm *status.StatusManager) (*CacheManager, error) {
	cm := CacheManager{
		cfg: cfg,
		sm:  sm,
	}

	// Scan loop: collect metrics at a frequent interval.
	go func() {
		for {
			if err := cm.Scan(); err != nil && !errors.Is(err, os.ErrNotExist) {
				logger.Logger().WithError(err).Warnf("scan cache failed")
			}
			time.Sleep(CacheScanInterval)
		}
	}()

	// GC loop: remove expired unused cache dirs at a less frequent interval.
	go func() {
		for {
			time.Sleep(CacheGCInterval)
			if err := cm.gc(); err != nil {
				logger.Logger().WithError(err).Warnf("gc cache failed")
			}
		}
	}()

	return &cm, nil
}
