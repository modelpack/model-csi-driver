package utils

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWithRetry_Success(t *testing.T) {
	ctx := context.Background()
	calls := 0
	err := WithRetry(ctx, func() error {
		calls++
		return nil
	}, 3, 0)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
}

func TestWithRetry_EventualSuccess(t *testing.T) {
	ctx := context.Background()
	calls := 0
	err := WithRetry(ctx, func() error {
		calls++
		if calls < 3 {
			return errors.New("transient error")
		}
		return nil
	}, 5, time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, 3, calls)
}

func TestWithRetry_ExhaustedRetries(t *testing.T) {
	ctx := context.Background()
	calls := 0
	err := WithRetry(ctx, func() error {
		calls++
		return errors.New("permanent error")
	}, 3, time.Millisecond)
	require.Error(t, err)
	require.Equal(t, 3, calls)
}

func TestWithRetry_BreakRetry(t *testing.T) {
	ctx := context.Background()
	calls := 0
	err := WithRetry(ctx, func() error {
		calls++
		return ErrBreakRetry
	}, 5, time.Millisecond)
	require.ErrorIs(t, err, ErrBreakRetry)
	require.Equal(t, 1, calls)
}

func TestEnsureSockNotExists_NonExistent(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "subdir", "csi.sock")
	ctx := context.Background()
	err := EnsureSockNotExists(ctx, sockPath)
	require.NoError(t, err)
	// The parent directory should have been created.
	_, statErr := os.Stat(filepath.Dir(sockPath))
	require.NoError(t, statErr)
}

func TestEnsureSockNotExists_ExistingFile(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "csi.sock")
	// Create a regular file at sockPath.
	require.NoError(t, os.WriteFile(sockPath, []byte("data"), 0644))
	ctx := context.Background()
	err := EnsureSockNotExists(ctx, sockPath)
	require.NoError(t, err)
	// The file should have been removed.
	_, statErr := os.Stat(sockPath)
	require.True(t, os.IsNotExist(statErr))
}

func TestEnsureSockNotExists_IsDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "dirissock")
	require.NoError(t, os.Mkdir(sockPath, 0755))
	ctx := context.Background()
	err := EnsureSockNotExists(ctx, sockPath)
	require.Error(t, err)
	require.Contains(t, err.Error(), "sock path is a directory")
}

func TestIsInSameDevice_SamePath(t *testing.T) {
	tmpDir := t.TempDir()
	same, err := IsInSameDevice(tmpDir, tmpDir)
	require.NoError(t, err)
	require.True(t, same)
}

func TestIsInSameDevice_ChildPath(t *testing.T) {
	tmpDir := t.TempDir()
	subDir := filepath.Join(tmpDir, "sub")
	require.NoError(t, os.Mkdir(subDir, 0755))
	same, err := IsInSameDevice(tmpDir, subDir)
	require.NoError(t, err)
	// tmpDir and subDir should be on same device.
	require.True(t, same)
}

func TestIsInSameDevice_NonExistent(t *testing.T) {
	_, err := IsInSameDevice("/non/existent/path1", "/non/existent/path2")
	require.Error(t, err)
}
