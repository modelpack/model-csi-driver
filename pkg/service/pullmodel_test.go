package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/status"
	pkgerrors "github.com/pkg/errors"
	"github.com/stretchr/testify/require"
)

type mockPuller struct {
	err error
}

func (m *mockPuller) Pull(ctx context.Context, reference, targetDir string, excludeModelWeights bool, excludeFilePatterns []string) error {
	return m.err
}

func newWorkerWithMockPuller(t *testing.T, pullErr error) *Worker {
	t.Helper()
	tmpDir := t.TempDir()
	rawCfg := &config.RawConfig{ServiceName: "test", RootDir: tmpDir}
	cfg := config.NewWithRaw(rawCfg)
	sm, err := status.NewStatusManager()
	require.NoError(t, err)

	worker, err := NewWorker(cfg, sm)
	require.NoError(t, err)

	worker.newPuller = func(ctx context.Context, pullCfg *config.PullConfig, hook *status.Hook, diskQuotaChecker *DiskQuotaChecker) Puller {
		return &mockPuller{err: pullErr}
	}
	return worker
}

func TestPullModel_Success(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	ctx := context.Background()
	volumeName := "pvc-pull-test"
	modelDir := filepath.Join(worker.cfg.Get().GetVolumeDir(volumeName), "model")

	err := worker.PullModel(ctx, true, volumeName, "", "test/model:latest", modelDir, false, false, nil)
	require.NoError(t, err)
}

func TestPullModel_Failure(t *testing.T) {
	worker := newWorkerWithMockPuller(t, pkgerrors.New("pull failed"))
	ctx := context.Background()
	volumeName := "pvc-pull-fail"
	modelDir := filepath.Join(worker.cfg.Get().GetVolumeDir(volumeName), "model")

	err := worker.PullModel(ctx, true, volumeName, "", "test/model:latest", modelDir, false, false, nil)
	require.Error(t, err)
}

func TestPullModel_DynamicVolume_Success(t *testing.T) {
	worker := newWorkerWithMockPuller(t, nil)
	ctx := context.Background()
	volumeName := "csi-dyn-pull"
	mountID := "mount-1"
	modelDir := worker.cfg.Get().GetModelDirForDynamic(volumeName, mountID)

	err := worker.PullModel(ctx, false, volumeName, mountID, "test/model:latest", modelDir, false, false, nil)
	require.NoError(t, err)
}

func TestPullModel_DynamicVolume_Failure(t *testing.T) {
	worker := newWorkerWithMockPuller(t, pkgerrors.New("network error"))
	ctx := context.Background()
	volumeName := "csi-dyn-fail"
	mountID := "mount-2"
	modelDir := worker.cfg.Get().GetModelDirForDynamic(volumeName, mountID)

	err := worker.PullModel(ctx, false, volumeName, mountID, "test/model:latest", modelDir, false, false, nil)
	require.Error(t, err)
}
