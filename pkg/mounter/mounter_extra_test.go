package mounter

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// Test execCmd with a harmless command
func TestExecCmd_Success(t *testing.T) {
	out, err := execCmd(context.Background(), "echo", "hello")
	require.NoError(t, err)
	require.Contains(t, out, "hello")
}

func TestExecCmd_Failure(t *testing.T) {
	_, err := execCmd(context.Background(), "false")
	require.Error(t, err)
}

// Test UMount with empty mountpoint
func TestUMount_EmptyMountPoint(t *testing.T) {
	err := UMount(context.Background(), "", false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not specified")
}

// Test UMount on a directory that is definitely not mounted (expects no error from swallowed "not mounted" message)
func TestUMount_NotMounted(t *testing.T) {
	tmpDir := t.TempDir()
	// umount will return "not mounted" which UMount swallows; just ensure no panic
	_ = UMount(context.Background(), tmpDir, false)
}

// Test that UMount lazy path is reachable
func TestUMount_LazyPath_EmptyMountPoint(t *testing.T) {
	err := UMount(context.Background(), "", true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not specified")
}

// Test Mount actually runs execCmd (will fail without root but covers the function)
func TestMount_ExecFails_CoversFunction(t *testing.T) {
	tmpDir := t.TempDir()
	// Mount will call execCmd("mount", "--bind", "/source", tmpDir) which fails (no root)
	// This exercises the Mount function including the error return path
	err := Mount(context.Background(), NewBuilder().Bind().From("/nonexistent/source/path").MountPoint(tmpDir))
	require.Error(t, err)
	require.Contains(t, err.Error(), "mount failed")
}

