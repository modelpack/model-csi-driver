package logger

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewContext(t *testing.T) {
	ctx := context.Background()
	ctx = NewContext(ctx, "NodePublishVolume", "pvc-volume-1", "/var/lib/kubelet/pods/target")

	require.NotNil(t, ctx.Value(RequestIDKey{}))
	require.Equal(t, "NodePublishVolume", ctx.Value(RequestOpKey{}))
	require.Equal(t, "pvc-volume-1", ctx.Value(RequestVolumeNameKey{}))
	require.Equal(t, "/var/lib/kubelet/pods/target", ctx.Value(RequestTargetPathKey{}))
}

func TestNewContext_EmptyTargetPath(t *testing.T) {
	ctx := context.Background()
	ctx = NewContext(ctx, "NodeUnpublishVolume", "pvc-volume-2", "")

	require.Nil(t, ctx.Value(RequestTargetPathKey{}))
	require.Equal(t, "NodeUnpublishVolume", ctx.Value(RequestOpKey{}))
}

func TestWithContext_Basic(t *testing.T) {
	ctx := NewContext(context.Background(), "op", "vol", "")
	entry := WithContext(ctx)
	require.NotNil(t, entry)
}

func TestWithContext_WithTargetPath(t *testing.T) {
	ctx := NewContext(context.Background(), "op", "vol", "/target")
	entry := WithContext(ctx)
	require.NotNil(t, entry)
}

func TestWithContext_NoRequestFields(t *testing.T) {
	// plain context without any request fields — should not panic.
	entry := WithContext(context.Background())
	require.NotNil(t, entry)
}

func TestLogger_NotNil(t *testing.T) {
	l := Logger()
	require.NotNil(t, l)
}
