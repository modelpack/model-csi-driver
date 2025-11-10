package utils

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/pkg/errors"
)

var ErrBreakRetry = errors.New("break retry")

func WithRetry(ctx context.Context, handle func() error, total int, delay time.Duration) error {
	for {
		total--
		err := handle()
		if err == nil || errors.Is(err, ErrBreakRetry) {
			return err
		}

		if total > 0 {
			logger.WithContext(ctx).Warnf("retry (remain %d times) after %s", total, delay)
			time.Sleep(delay)
			continue
		}

		return err
	}
}

func EnsureSockNotExists(ctx context.Context, sockPath string) error {
	stat, err := os.Stat(sockPath)
	if err == nil {
		if stat.IsDir() {
			return errors.Errorf("sock path is a directory: %s", sockPath)
		}
		if err = os.Remove(sockPath); err != nil {
			return errors.Wrapf(err, "remove existed sock path: %s", sockPath)
		}
		logger.WithContext(ctx).Infof("removed existed sock path: %s", sockPath)
	} else if !os.IsNotExist(err) {
		return errors.Wrapf(err, "stat sock path: %s", sockPath)
	}

	if err = os.MkdirAll(filepath.Dir(sockPath), 0755); err != nil {
		return errors.Wrapf(err, "create sock path dir: %s", filepath.Dir(sockPath))
	}
	return nil
}

func IsInSameDevice(path1, path2 string) (bool, error) {
	info1, err := os.Stat(path1)
	if err != nil {
		return false, errors.Wrapf(err, "stat path: %s", path1)
	}
	info2, err := os.Stat(path2)
	if err != nil {
		return false, errors.Wrapf(err, "stat path: %s", path2)
	}
	stat1, ok1 := info1.Sys().(*syscall.Stat_t)
	stat2, ok2 := info2.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 {
		return false, errors.New("failed to get underlying file stat")
	}
	return stat1.Dev == stat2.Dev, nil
}
