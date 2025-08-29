package service

import (
	"os"
	"path/filepath"
	"time"

	"github.com/CloudNativeAI/model-csi-driver/pkg/config"
	"github.com/CloudNativeAI/model-csi-driver/pkg/logger"
	"github.com/CloudNativeAI/model-csi-driver/pkg/metrics"
	"github.com/pkg/errors"
)

var CacheSacnInterval = 60 * time.Second

type CacheManager struct {
	cfg *config.Config
}

func (cm *CacheManager) getCacheSize() (int64, error) {
	var total int64
	if err := filepath.Walk(cm.cfg.RootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	}); err != nil {
		return 0, err
	}
	return total, nil
}

func (cm *CacheManager) scanModels() error {
	staticModels := 0
	dynamicModels := 0
	volumesDir := cm.cfg.GetVolumesDir()
	volumeDirs, err := os.ReadDir(volumesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.Wrapf(err, "read volume dirs from %s", volumesDir)
	}
	for _, volumeDir := range volumeDirs {
		if !volumeDir.IsDir() {
			continue
		}
		if isStaticVolume(volumeDir.Name()) {
			staticModels += 1
		}
		if isDynamicVolume(volumeDir.Name()) {
			modelsDir := cm.cfg.GetModelsDirForDynamic(volumeDir.Name())
			modelDirs, err := os.ReadDir(modelsDir)
			if err != nil {
				return errors.Wrapf(err, "read model dirs from %s", modelsDir)
			}
			for _, modelDir := range modelDirs {
				if !modelDir.IsDir() {
					continue
				}
				dynamicModels += 1
			}
		}
	}
	metrics.NodeMountedStaticImages.Set(float64(staticModels))
	metrics.NodeMountedDynamicImages.Set(float64(dynamicModels))
	return nil
}

func (cm *CacheManager) Scan() error {
	// Get the cache total size
	cacheSize, err := cm.getCacheSize()
	if err != nil {
		return errors.Wrapf(err, "scan cache from %s", cm.cfg.RootDir)
	}
	metrics.NodeCacheSizeInBytes.Set(float64(cacheSize))

	// Get the model mounted count
	if err := cm.scanModels(); err != nil {
		return errors.Wrapf(err, "scan models")
	}

	return nil
}

func NewCacheManager(cfg *config.Config) (*CacheManager, error) {
	cm := CacheManager{
		cfg: cfg,
	}

	go func() {
		for {
			if err := cm.Scan(); err != nil && !errors.Is(err, os.ErrNotExist) {
				logger.Logger().WithError(err).Warnf("scan cache failed")
			}
			time.Sleep(CacheSacnInterval)
		}
	}()

	return &cm, nil
}
