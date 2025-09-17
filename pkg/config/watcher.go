package config

import (
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
	"github.com/modelpack/model-csi-driver/pkg/logger"
)

var mutex = sync.Mutex{}

func (cfg *Config) watch(path string) {
	configDir := filepath.Dir(path)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		logger.Logger().WithError(err).Error("failed to create fsnotify watcher")
		return
	}
	defer func() { _ = watcher.Close() }()

	go func() {
		defer logger.Logger().Warn("fsnotify watcher goroutine exited")
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if (event.Op & (fsnotify.Write | fsnotify.Remove)) != 0 {
					logger.Logger().Infof("config file changed: %s, event: %s", event.Name, event.Op)
					cfg.reload(path)
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				logger.Logger().WithError(err).Error("watcher error")
			}
		}
	}()

	err = watcher.Add(configDir)
	if err != nil {
		logger.Logger().WithError(err).Error("failed to add config dir to watcher")
	}

	select {}
}

func (cfg *Config) reload(path string) {
	newCfg, err := parse(path)
	if err != nil {
		logger.Logger().WithError(err).Error("failed to parse config file")
		return
	}

	mutex.Lock()
	defer mutex.Unlock()

	*cfg = *newCfg

	logger.Logger().Infof("config reloaded: %s", path)
}
