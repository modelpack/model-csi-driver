package status

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/modelpack/model-csi-driver/pkg/tracing"
)

func TestMain(m *testing.M) {
	// Initialize a noop tracer so hook_test doesn't panic.
	tracing.Tracer = noop.NewTracerProvider().Tracer("test")
	os.Exit(m.Run())
}

// ─── StatusManager ────────────────────────────────────────────────────────────

func TestNewStatusManager(t *testing.T) {
	sm, err := NewStatusManager()
	require.NoError(t, err)
	require.NotNil(t, sm)
	require.NotNil(t, sm.HookManager)
}

func TestStatusManager_SetAndGet(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "status.json")

	sm, err := NewStatusManager()
	require.NoError(t, err)

	s := Status{
		VolumeName: "pvc-vol-1",
		MountID:    "mount-1",
		Reference:  "registry/model:v1",
		State:      StatePullRunning,
	}

	written, err := sm.Set(statusPath, s)
	require.NoError(t, err)
	require.Equal(t, StatePullRunning, written.State)

	got, err := sm.Get(statusPath)
	require.NoError(t, err)
	require.Equal(t, "pvc-vol-1", got.VolumeName)
	require.Equal(t, StatePullRunning, got.State)
}

func TestStatusManager_GetNotExists(t *testing.T) {
	sm, err := NewStatusManager()
	require.NoError(t, err)

	_, err = sm.Get("/non/existent/path/status.json")
	require.Error(t, err)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestStatusManager_GetEmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "status.json")
	require.NoError(t, os.WriteFile(statusPath, []byte("   "), 0644))

	sm, err := NewStatusManager()
	require.NoError(t, err)

	_, err = sm.Get(statusPath)
	require.Error(t, err)
}

func TestStatusManager_GetInvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "status.json")
	require.NoError(t, os.WriteFile(statusPath, []byte("not-json"), 0644))

	sm, err := NewStatusManager()
	require.NoError(t, err)

	_, err = sm.Get(statusPath)
	require.Error(t, err)
}

func TestStatusManager_OverwriteStatus(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "status.json")

	sm, err := NewStatusManager()
	require.NoError(t, err)

	_, err = sm.Set(statusPath, Status{State: StatePullRunning})
	require.NoError(t, err)

	_, err = sm.Set(statusPath, Status{State: StatePullSucceeded})
	require.NoError(t, err)

	got, err := sm.Get(statusPath)
	require.NoError(t, err)
	require.Equal(t, StatePullSucceeded, got.State)
}

func TestStatusManager_GetWithHookProgress(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "status.json")

	sm, err := NewStatusManager()
	require.NoError(t, err)

	_, err = sm.Set(statusPath, Status{State: StatePullRunning, VolumeName: "vol"})
	require.NoError(t, err)

	// Register a hook and add some progress.
	hook := NewHook(context.Background())
	hook.SetTotal(2)
	sm.HookManager.Set(statusPath, hook)

	got, err := sm.Get(statusPath)
	require.NoError(t, err)
	require.Equal(t, 2, got.Progress.Total)
}

// ─── Progress ─────────────────────────────────────────────────────────────────

func TestProgress_String(t *testing.T) {
	p := Progress{Total: 3, Items: []ProgressItem{
		{Digest: "sha256:abc", Path: "/model.safetensors", Size: 1024},
	}}
	s, err := p.String()
	require.NoError(t, err)
	require.Contains(t, s, "sha256:abc")
}

// ─── HookManager ──────────────────────────────────────────────────────────────

func TestHookManager_SetGetDelete(t *testing.T) {
	hm := NewHookManager()

	// GetProgress on missing key returns empty.
	p := hm.GetProgress("k1")
	require.Equal(t, 0, p.Total)

	hook := NewHook(context.Background())
	hook.SetTotal(5)
	hm.Set("k1", hook)

	p = hm.GetProgress("k1")
	require.Equal(t, 5, p.Total)

	hm.Delete("k1")
	p = hm.GetProgress("k1")
	require.Equal(t, 0, p.Total)
}

// ─── Hook ─────────────────────────────────────────────────────────────────────

func TestHook_SetTotal(t *testing.T) {
	h := NewHook(context.Background())
	h.SetTotal(10)
	p := h.GetProgress()
	require.Equal(t, 10, p.Total)
}

func TestHook_BeforeAndAfterPullLayer_Success(t *testing.T) {
	h := NewHook(context.Background())
	h.SetTotal(1)

	desc := ocispec.Descriptor{
		Digest:    digest.Digest("sha256:aabbcc"),
		MediaType: "application/octet-stream",
		Size:      2048,
		Annotations: map[string]string{
			"org.modelpack.model.filepath": "weights.safetensors",
		},
	}
	manifest := ocispec.Manifest{}

	h.BeforePullLayer(desc, manifest)

	p := h.GetProgress()
	require.Len(t, p.Items, 1)
	require.Nil(t, p.Items[0].FinishedAt)

	h.AfterPullLayer(desc, nil)

	p = h.GetProgress()
	require.Len(t, p.Items, 1)
	require.NotNil(t, p.Items[0].FinishedAt)
}

func TestHook_AfterPullLayer_WithError(t *testing.T) {
	h := NewHook(context.Background())

	desc := ocispec.Descriptor{
		Digest:    digest.Digest("sha256:fail"),
		MediaType: "application/octet-stream",
		Size:      512,
	}
	manifest := ocispec.Manifest{}
	h.BeforePullLayer(desc, manifest)
	h.AfterPullLayer(desc, os.ErrInvalid)

	p := h.GetProgress()
	require.Len(t, p.Items, 1)
	require.Equal(t, os.ErrInvalid, p.Items[0].Error)
}

func TestHook_AfterPullLayer_UnknownDigest(t *testing.T) {
	// AfterPullLayer on an unregistered digest should not panic.
	h := NewHook(context.Background())
	desc := ocispec.Descriptor{Digest: digest.Digest("sha256:unknown")}
	// Should not panic.
	h.AfterPullLayer(desc, nil)
}

func TestHook_GetProgress_Sorted(t *testing.T) {
	h := NewHook(context.Background())

	now := time.Now()
	manifest := ocispec.Manifest{}

	for _, d := range []struct {
		dgst  digest.Digest
		delay time.Duration
	}{
		{"sha256:cc", 10 * time.Millisecond},
		{"sha256:aa", 0},
		{"sha256:bb", 5 * time.Millisecond},
	} {
		d := d
		_ = ocispec.Descriptor{Digest: d.dgst, Size: 100}
		// Manually insert to control StartedAt.
		h.mutex.Lock()
		at := now.Add(d.delay)
		h.progress[d.dgst] = &ProgressItem{
			Digest:    d.dgst,
			StartedAt: at,
		}
		h.manifest = &manifest
		h.mutex.Unlock()
	}

	p := h.GetProgress()
	require.Len(t, p.Items, 3)
	// Items should be sorted by StartedAt ascending.
	require.True(t, p.Items[0].StartedAt.Before(p.Items[1].StartedAt) || p.Items[0].StartedAt.Equal(p.Items[1].StartedAt))
	require.True(t, p.Items[1].StartedAt.Before(p.Items[2].StartedAt) || p.Items[1].StartedAt.Equal(p.Items[2].StartedAt))
}

func TestHook_GetTotal_FromManifestLayers(t *testing.T) {
	h := NewHook(context.Background())

	manifest := ocispec.Manifest{
		Layers: []ocispec.Descriptor{
			{Digest: "sha256:l1"},
			{Digest: "sha256:l2"},
		},
	}
	desc := ocispec.Descriptor{Digest: "sha256:l1", Size: 100}
	h.BeforePullLayer(desc, manifest)

	p := h.GetProgress()
	// total comes from manifest.Layers
	require.Equal(t, 2, p.Total)
}
