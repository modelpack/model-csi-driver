package service

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/dustin/go-humanize"
	"github.com/modelpack/modctl/pkg/backend"
	modctlConfig "github.com/modelpack/modctl/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
)

type DiskQuotaChecker struct {
	cfg *config.Config
}

func getUsedSize(path string) (int64, error) {
	var total int64 = 0
	inodes := make(map[uint64]bool)

	err := filepath.Walk(path, func(fname string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil
		}
		inode := stat.Ino
		if info.Mode().IsRegular() || info.IsDir() {
			if exist := inodes[inode]; !exist {
				inodes[inode] = true
				total += int64(stat.Blocks) * 512
			}
		} else if info.Mode()&os.ModeSymlink != 0 {
			total += int64(stat.Blocks) * 512
		}

		return nil
	})

	return total, err
}

func NewDiskQuotaChecker(cfg *config.Config) *DiskQuotaChecker {
	return &DiskQuotaChecker{
		cfg: cfg,
	}
}

func (d *DiskQuotaChecker) getModelSize(ctx context.Context, b backend.Backend, reference string, plainHTTP bool) (int64, error) {
	result, err := b.Inspect(ctx, reference, &modctlConfig.Inspect{
		Remote:    true,
		Insecure:  true,
		PlainHTTP: plainHTTP,
	})
	if err != nil {
		return 0, errors.Wrap(err, "inspect model")
	}

	modelArtifact, ok := result.(*backend.InspectedModelArtifact)
	if !ok {
		return 0, fmt.Errorf("invalid inspected result")
	}

	totalSize := int64(0)
	digestMap := make(map[string]bool)
	for idx := range modelArtifact.Layers {
		layer := modelArtifact.Layers[idx]
		if _, exists := digestMap[layer.Digest]; exists {
			continue
		}
		totalSize += layer.Size
		digestMap[layer.Digest] = true
	}

	return totalSize, nil
}

func humanizeBytes(size int64) string {
	if size >= 0 {
		return humanize.IBytes(uint64(size))
	}
	return fmt.Sprintf("-%s", humanize.IBytes(uint64(-size)))
}

// Check checks if there is enough disk quota to mount the model.
//
// If cfg.Features.CheckDiskQuota is enabled and the Mount request specifies checkDiskQuota = true:
// - When cfg.Features.DiskUsageLimit == 0: reject if available disk space < model size;
// - When cfg.Features.DiskUsageLimit > 0: reject if (cfg.Features.DiskUsageLimit - used space) < model size;
func (d *DiskQuotaChecker) Check(ctx context.Context, b backend.Backend, reference string, plainHTTP bool) error {
	availSize := int64(0)

	if d.cfg.Get().Features.DiskUsageLimit > 0 {
		usedSize, err := getUsedSize(d.cfg.Get().RootDir)
		if err != nil {
			return errors.Wrap(err, "get root dir used size")
		}
		availSize = int64(d.cfg.Get().Features.DiskUsageLimit) - usedSize
	} else {
		var st syscall.Statfs_t
		if err := syscall.Statfs(d.cfg.Get().RootDir, &st); err != nil {
			return errors.Wrap(err, "stat root dir")
		}
		availSize = int64(st.Bavail) * int64(st.Bsize)
	}

	modelSize, err := d.getModelSize(ctx, b, reference, plainHTTP)
	if err != nil {
		return errors.Wrap(err, "get model size")
	}

	logger.WithContext(ctx).Infof(
		"root dir maximum limit size: %s, available: %s, model: %s",
		humanizeBytes(int64(d.cfg.Get().Features.DiskUsageLimit)), humanizeBytes(availSize), humanizeBytes(modelSize),
	)

	if modelSize > availSize {
		return errors.Wrapf(
			syscall.ENOSPC, "model image %s is %s, but only %s of disk quota is available",
			reference, humanizeBytes(modelSize), humanizeBytes(availSize),
		)
	}

	return nil
}
