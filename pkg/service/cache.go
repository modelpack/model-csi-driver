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

var CacheScanInterval = 60 * time.Second

const (
	mountTypePVC = "pvc"
	mountTypeInline = "inline"
	mountTypeDynamic = "dynamic"
)

type CacheManager struct {
	cfg *config.Config
	sm *status.StatusManager
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
	volumesDir := cm.cfg.Get().GetVolumesDir()
	volumeDirs, err := os.ReadDir(volumesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.Wrapf(err, "read volume dirs from %s", volumesDir)
	}

	mountItems := []metrics.MountItem{}
	for _, volumeDir := range volumeDirs {
		if !volumeDir.IsDir() {
			continue
		}
		volumeName := volumeDir.Name()
		if isStaticVolume(volumeName) {
			statusPath := filepath.Join(volumesDir, volumeName, "status.json")
			modelStatus, err := cm.sm.Get(statusPath)
			if err == nil {
				mountItems = append(mountItems, metrics.MountItem{
					Reference:   modelStatus.Reference,
					Type:        mountTypePVC,
					VolumeName: volumeName,
					MountID:    modelStatus.MountID,
				})
				pvcModels += 1
			}
		}
		if isDynamicVolume(volumeName) {
			modelsDir := cm.cfg.Get().GetModelsDirForDynamic(volumeName)
			modelDirs, err := os.ReadDir(modelsDir)
      if err != nil {
				if os.IsNotExist(err) {
					// This is potentially an inline model, the status file is expected
					// to be directly under the volume directory.
					statusPath := filepath.Join(volumesDir, volumeName, "status.json")
					modelStatus, err := cm.sm.Get(statusPath)
					if err == nil {
						mountItems = append(mountItems, metrics.MountItem{
							Reference:   modelStatus.Reference,
							Type:        mountTypeInline,
							VolumeName: volumeName,
							MountID:    modelStatus.MountID,
						})
						inlineModels += 1
					}
					continue
				}
				logger.Logger().WithError(err).Warnf("read model dirs from %s", modelsDir)
				continue
			}
			for _, modelDir := range modelDirs {
				if !modelDir.IsDir() {
					continue
				}
				statusPath := filepath.Join(modelsDir, modelDir.Name(), "status.json")
				modelStatus, err := cm.sm.Get(statusPath)
				if err == nil {
					mountItems = append(mountItems, metrics.MountItem{
						Reference:   modelStatus.Reference,
						Type:        mountTypeDynamic,
						VolumeName: volumeName,
						MountID:    modelStatus.MountID,
					})
					dynamicModels += 1
				}
			}
		}
	}

	metrics.MountItems.Set(mountItems)
	metrics.NodeMountedPVCModels.Set(float64(pvcModels))
	metrics.NodeMountedInlineModels.Set(float64(inlineModels))
	metrics.NodeMountedDynamicModels.Set(float64(dynamicModels))

	return nil
}

func (cm *CacheManager) Scan() error {
	// Get the cache total size
	cacheSize, err := cm.getCacheSize()
	if err != nil {
		return errors.Wrapf(err, "scan cache from %s", cm.cfg.Get().RootDir)
	}
	metrics.NodeCacheSizeInBytes.Set(float64(cacheSize))

	// Get the model mounted count
	if err := cm.scanModels(); err != nil {
		return errors.Wrapf(err, "scan models")
	}

	return nil
}

func NewCacheManager(cfg *config.Config, sm *status.StatusManager) (*CacheManager, error) {
	cm := CacheManager{
		cfg: cfg,
		sm:  sm,
	}

	go func() {
		for {
			if err := cm.Scan(); err != nil && !errors.Is(err, os.ErrNotExist) {
				logger.Logger().WithError(err).Warnf("scan cache failed")
			}
			time.Sleep(CacheScanInterval)
		}
	}()

	return &cm, nil
}
