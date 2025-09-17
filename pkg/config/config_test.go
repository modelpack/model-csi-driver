package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func copyFile(t *testing.T, src, dst string) {
	data, err := os.ReadFile(src)
	require.NoError(t, err)

	err = os.WriteFile(dst, data, 0644)
	require.NoError(t, err)
}

func TestConfig(t *testing.T) {
	// Create a temporary directory for testing
	tmpDir, err := os.MkdirTemp("", "config-test-")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tmpDir) }()

	require.NoError(t, os.Setenv("X_CSI_MODE", "node"))
	require.NoError(t, os.Setenv("CSI_NODE_ID", "test-node"))

	// Prepare the origin config file
	testConfigPath := "../../test/testdata/config.test.yaml"
	configPath := filepath.Join(tmpDir, "config.yaml")
	copyFile(t, testConfigPath, configPath)
	cfg, err := New(configPath)
	require.NoError(t, err)

	// Wait watcher to start
	time.Sleep(time.Second)

	// Update the origin config file (k8s configmap's atomic update is rename)
	tmpConfigPath := filepath.Join(tmpDir, "config.tmp.yaml")
	copyFile(t, testConfigPath, tmpConfigPath)
	data, err := os.ReadFile(tmpConfigPath)
	require.NoError(t, err)
	updatedData := strings.Replace(string(data), "disk_usage_limit: 10TiB", "disk_usage_limit: 5TiB", 1)
	require.NoError(t, os.WriteFile(tmpConfigPath, []byte(updatedData), 0644))
	require.NoError(t, os.Rename(tmpConfigPath, configPath))

	// Wait watcher to reload the config
	time.Sleep(time.Second * 1)

	// Verify the config is reloaded
	require.Equal(t, uint64(0x50000000000), uint64(cfg.Features.DiskUsageLimit))
}
