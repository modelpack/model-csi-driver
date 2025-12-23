package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/metrics"
	"github.com/modelpack/model-csi-driver/pkg/status"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

func TestCacheManagerScanUpdatesMetrics(t *testing.T) {
	tempDir := t.TempDir()

	rawCfg := &config.RawConfig{ServiceName: "test", RootDir: tempDir}
	cfg := config.NewWithRaw(rawCfg)

	sm, err := status.NewStatusManager()
	require.NoError(t, err)

	// Create a pvc volume status
	pvcStatusPath := filepath.Join(tempDir, "volumes", "pvc-static", "status.json")
	_, err = sm.Set(pvcStatusPath, status.Status{Reference: "ref-pvc", MountID: ""})
	require.NoError(t, err)

	// Create a dynamic volume status under models/<mountID>/status.json
	dynamicStatusPath := filepath.Join(tempDir, "volumes", "csi-dyn", "models", "mount-1", "status.json")
	_, err = sm.Set(dynamicStatusPath, status.Status{Reference: "ref-dyn", MountID: "mount-1"})
	require.NoError(t, err)

	// An extra file to ensure cache size covers arbitrary files under RootDir.
	extraPath := filepath.Join(tempDir, "extra.bin")
	require.NoError(t, os.WriteFile(extraPath, []byte("abc"), 0o644))

	expectedSize, err := getUsedSize(rawCfg.RootDir)
	require.NoError(t, err)

	cm := &CacheManager{cfg: cfg, sm: sm}
	require.NoError(t, cm.Scan())

	require.Equal(t, float64(expectedSize), testutil.ToFloat64(metrics.NodeCacheSizeInBytes))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.NodeMountedPVCModels))
	require.Equal(t, float64(0), testutil.ToFloat64(metrics.NodeMountedInlineModels))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.NodeMountedDynamicModels))

	// Verify mount item metrics are exported as a snapshot without Reset/Delete races.
	reg := prometheus.NewRegistry()
	reg.MustRegister(metrics.MountItems)

	mfs, err := reg.Gather()
	require.NoError(t, err)

	mf := findMetricFamily(t, mfs, metrics.Prefix+"mount_item")
	require.Len(t, mf.Metric, 2)

	pvcLabels := map[string]string{
		"reference":   "ref-pvc",
		"type":        "pvc",
		"volume_name": "pvc-static",
		"mount_id":    "",
	}
	dynamicLabels := map[string]string{
		"reference":   "ref-dyn",
		"type":        "dynamic",
		"volume_name": "csi-dyn",
		"mount_id":    "mount-1",
	}

	var foundPVC, foundDynamic bool
	for _, m := range mf.Metric {
		if hasLabels(m, pvcLabels) {
			foundPVC = true
		}
		if hasLabels(m, dynamicLabels) {
			foundDynamic = true
		}
	}
	require.True(t, foundPVC, "pvc mount item metric not found")
	require.True(t, foundDynamic, "dynamic mount item metric not found")
}

func findMetricFamily(t *testing.T, mfs []*dto.MetricFamily, name string) *dto.MetricFamily {
	t.Helper()
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf
		}
	}
	require.FailNow(t, "metric family not found", name)
	return nil
}

func hasLabels(m *dto.Metric, want map[string]string) bool {
	labels := map[string]string{}
	for _, lp := range m.GetLabel() {
		labels[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if labels[k] != v {
			return false
		}
	}
	return true
}
