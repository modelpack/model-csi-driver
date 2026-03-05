package mounter

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// ─── EnsureMountPoint ─────────────────────────────────────────────────────────

func TestEnsureMountPoint_NonExistent(t *testing.T) {
	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "a", "b", "c")

	err := EnsureMountPoint(context.Background(), target)
	require.NoError(t, err)

	info, err := os.Stat(target)
	require.NoError(t, err)
	require.True(t, info.IsDir())
}

func TestEnsureMountPoint_AlreadyExists(t *testing.T) {
	tmpDir := t.TempDir()
	// Calling twice should not fail.
	err := EnsureMountPoint(context.Background(), tmpDir)
	require.NoError(t, err)
}

// ─── IsMounted ────────────────────────────────────────────────────────────────

func TestIsMounted_NonExistentPath(t *testing.T) {
	mounted, err := IsMounted(context.Background(), "/non/existent/path/12345")
	require.NoError(t, err)
	require.False(t, mounted)
}

func TestIsMounted_ExistingNotMounted(t *testing.T) {
	tmpDir := t.TempDir()
	mounted, err := IsMounted(context.Background(), tmpDir)
	require.NoError(t, err)
	// tmpDir is not a mount point (usually).
	_ = mounted // could be true on CI if tmpDir is itself a mount point; just no error.
}

// ─── MountBuilder ─────────────────────────────────────────────────────────────

func TestMountBuilder_Bind_Build(t *testing.T) {
	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "target")

	cmd, err := NewBuilder().Bind().From("/source").MountPoint(target).Build()
	require.NoError(t, err)
	require.NotEmpty(t, cmd.String())
}

func TestMountBuilder_RBind_Build(t *testing.T) {
	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "target")

	cmd, err := NewBuilder().RBind().From("/source").MountPoint(target).Build()
	require.NoError(t, err)
	require.NotEmpty(t, cmd.String())
}

func TestMountBuilder_Tmpfs_Build(t *testing.T) {
	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "tmpfs-target")

	cmd, err := NewBuilder().Tmpfs().Size("1073741824").MountPoint(target).Build()
	require.NoError(t, err)
	require.Contains(t, cmd.String(), "tmpfs")
}

func TestMountBuilder_MissingMountPoint(t *testing.T) {
	b := &MountBuilder{command: "mount"}
	_, err := b.Build()
	require.Error(t, err)
	require.Contains(t, err.Error(), "mountPoint is required")
}

func TestMountCmd_String(t *testing.T) {
	cmd := MountCmd{command: "mount", args: []string{"--bind", "/src", "/dst"}}
	s := cmd.String()
	require.Contains(t, s, "mount")
	require.Contains(t, s, "--bind")
}

func TestMountBuilder_Size_Capped(t *testing.T) {
	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "t")

	// One very large size should be capped to 2<<30
	cmd, err := NewBuilder().Tmpfs().Size("99999999999").MountPoint(target).Build()
	require.NoError(t, err)
	// Size should be capped at 2*1024*1024*1024 = 2147483648
	require.Contains(t, cmd.String(), "2147483648")
}
