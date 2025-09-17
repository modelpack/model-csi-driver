package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/modelpack/modctl/pkg/backend"
	modctlConfig "github.com/modelpack/modctl/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestGetUsedSize(t *testing.T) {
	// Create a temporary directory for testing
	tmpDir, err := os.MkdirTemp("", "size-test-")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Test case 1: Empty directory
	size, err := getUsedSize(tmpDir)
	require.NoError(t, err)
	require.Equal(t, int64(4096), size)

	// Test case 2: Directory with a small file
	testFile := filepath.Join(tmpDir, "test.txt")
	content := []byte("test content")
	err = os.WriteFile(testFile, content, 0644)
	require.NoError(t, err)

	size, err = getUsedSize(tmpDir)
	require.NoError(t, err)
	require.Equal(t, int64(4096*2), size)

	// Test case 3: Directory with subdirectory and file
	subDir := filepath.Join(tmpDir, "subdir")
	err = os.Mkdir(subDir, 0755)
	require.NoError(t, err)

	subFile := filepath.Join(subDir, "subfile.txt")
	subContent := []byte("content in subdir")
	err = os.WriteFile(subFile, subContent, 0644)
	require.NoError(t, err)

	size, err = getUsedSize(tmpDir)
	require.NoError(t, err)
	require.Equal(t, int64(4096*4), size)

	// Test case 4: Directory with symlink
	symlinkPath := filepath.Join(tmpDir, "symlink")
	err = os.Symlink(testFile, symlinkPath)
	require.NoError(t, err)

	size, err = getUsedSize(tmpDir)
	require.NoError(t, err)
	require.Equal(t, int64(4096*4), size)

	// Test case 5: Non-existent path
	_, err = getUsedSize("/non/existent/path")
	require.Error(t, err)

	// Test case 6: Hard link (same inode)
	hardlinkPath := filepath.Join(tmpDir, "hardlink")
	err = os.Link(testFile, hardlinkPath)
	require.NoError(t, err)

	size, err = getUsedSize(tmpDir)
	require.NoError(t, err)
	require.Equal(t, int64(4096*4), size)

	// Test case 7: Directory with special files (should be ignored)
	specialFile := filepath.Join(tmpDir, "special")
	err = syscall.Mknod(specialFile, syscall.S_IFSOCK|0666, int(unix.Mkdev(255, 0)))
	require.NoError(t, err)

	size, err = getUsedSize(tmpDir)
	require.NoError(t, err)
	require.Equal(t, int64(4096*4), size)

	// Test case 8: Directory with a holed file
	holedFile := filepath.Join(tmpDir, "holedfile")
	f, err := os.Create(holedFile)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	err = f.Truncate(1024 * 1024) // 1MB sparse file
	require.NoError(t, err)
	_, err = f.WriteAt([]byte("data at offset"), 512*1024) // Write data at 512KB offset
	require.NoError(t, err)

	size, err = getUsedSize(tmpDir)
	require.NoError(t, err)
	require.Equal(t, int64(4096*5), size)

	// Test case 9: Directory with a file with 3MiB + 1 byte
	largeFile := filepath.Join(tmpDir, "largefile")
	largeContent := make([]byte, 3*1024*1024+1)
	err = os.WriteFile(largeFile, largeContent, 0644)
	require.NoError(t, err)

	size, err = getUsedSize(tmpDir)
	require.NoError(t, err)
	require.Equal(t, int64(4096*5+(1048576*3+4096)), size)
}

func TestDiskQuotaChecker(t *testing.T) {
	// Create a temporary directory for testing
	tmpDir, err := os.MkdirTemp("", "quota-test-")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Test case 1: Successful quota check with available space

	// The model size is 5MiB (3MiB + 2MiB - 3MiB deduplicated)
	ctx := context.Background()
	b, err := backend.New(filepath.Join(tmpDir, "modctl"))
	require.NoError(t, err)
	patch := gomonkey.ApplyMethod(b, "Inspect",
		func(backend.Backend, context.Context, string, *modctlConfig.Inspect) (interface{}, error) {
			return &backend.InspectedModelArtifact{
				Layers: []backend.InspectedModelArtifactLayer{
					{Digest: "sha256:layer1", Size: 3 * 1024 * 1024},
					{Digest: "sha256:layer2", Size: 2 * 1024 * 1024},
					{Digest: "sha256:layer1", Size: 3 * 1024 * 1024},
				},
			}, nil
		})
	defer patch.Reset()

	// The used size is 1MiB
	err = os.WriteFile(filepath.Join(tmpDir, "file-1"), make([]byte, 1*1024*1024), 0644)
	require.NoError(t, err)

	// The DiskUsageLimit is set to 7MiB
	cfg := &config.Config{
		RootDir: tmpDir,
		Features: config.Features{
			CheckDiskQuota: true,
			DiskUsageLimit: 7 * 1024 * 1024,
		},
	}

	checker := NewDiskQuotaChecker(cfg)
	err = checker.Check(ctx, b, "test/model:latest", false)
	require.NoError(t, err)

	// Test case 2: Failed quota check with insufficient space

	// The used size is 8MiB
	err = os.WriteFile(filepath.Join(tmpDir, "file-2"), make([]byte, 7*1024*1024), 0644)
	require.NoError(t, err)
	err = checker.Check(ctx, b, "test/model:latest", false)
	require.True(t, errors.Is(err, syscall.ENOSPC))

	// Update the DiskUsageLimit to 13MiB + 4096KiB
	cfg.Features.DiskUsageLimit = 13*1024*1024 + 4096
	err = checker.Check(ctx, b, "test/model:latest", false)
	require.NoError(t, err)

	// Test case 3: Check with DiskUsageLimit = 0 (use available disk space)

	// Update the DiskUsageLimit to 0MiB
	cfg.Features.DiskUsageLimit = 0

	// Mock syscall.Statfs to 5MiB available space
	patchStatfs := gomonkey.ApplyFunc(syscall.Statfs,
		func(path string, stat *syscall.Statfs_t) error {
			stat.Bavail = 5
			stat.Bsize = 1024 * 1024
			return nil
		})
	defer patchStatfs.Reset()

	err = checker.Check(ctx, b, "test/model:latest", false)
	require.True(t, errors.Is(err, syscall.ENOSPC))
}
