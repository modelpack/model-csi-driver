package service

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	modctlBackend "github.com/modelpack/modctl/pkg/backend"
	modctlConfig "github.com/modelpack/modctl/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/config/auth"
	"github.com/modelpack/model-csi-driver/pkg/status"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestPullerPull_NoPatternsReturnsEarly(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(auth.GetKeyChainByRef, func(string) (*auth.PassKeyChain, error) {
		return &auth.PassKeyChain{ServerScheme: "https"}, nil
	})
	patches.ApplyFunc(modctlBackend.New, func(string) (modctlBackend.Backend, error) {
		return nil, nil
	})
	patches.ApplyMethod(reflect.TypeOf(&ModelArtifact{}), "GetPatterns", func(*ModelArtifact, context.Context, bool, []string) ([]string, int, error) {
		return nil, 3, nil
	})

	targetDir := filepath.Join(t.TempDir(), "model")
	p := &puller{pullCfg: &config.PullConfig{Concurrency: 1}}

	err := p.Pull(context.Background(), "example.com/ns/model:latest", targetDir, true, nil)
	require.NoError(t, err)

	stat, statErr := os.Stat(targetDir)
	require.NoError(t, statErr)
	require.True(t, stat.IsDir())
}

func TestPullerCombinedHook_NoLayerCache(t *testing.T) {
	p := &puller{}
	h := p.combinedHook(context.Background(), "/tmp")
	require.NotNil(t, h)
	ch, ok := h.(*combinedHook)
	require.True(t, ok)
	require.Nil(t, ch.lc)
}

func TestPullerCombinedHook_WithLayerCache(t *testing.T) {
	lc, _ := newTestCache(t)
	p := &puller{layerCache: lc}
	h := p.combinedHook(context.Background(), t.TempDir())
	require.NotNil(t, h)
	ch, ok := h.(*combinedHook)
	require.True(t, ok)
	require.NotNil(t, ch.lc)
}

func TestPullerPull_FullPull_InvokesBackendPull(t *testing.T) {
	tmpDir := t.TempDir()
	b, err := modctlBackend.New(filepath.Join(tmpDir, "modctl"))
	require.NoError(t, err)

	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(auth.GetKeyChainByRef, func(string) (*auth.PassKeyChain, error) {
		return &auth.PassKeyChain{ServerScheme: "https"}, nil
	})
	patches.ApplyFunc(modctlBackend.New, func(string) (modctlBackend.Backend, error) {
		return b, nil
	})
	called := false
	patches.ApplyMethod(b, "Pull", func(modctlBackend.Backend, context.Context, string, *modctlConfig.Pull) error {
		called = true
		return nil
	})

	targetDir := filepath.Join(tmpDir, "model")
	p := &puller{pullCfg: &config.PullConfig{Concurrency: 1}}

	err = p.Pull(context.Background(), "example.com/ns/model:latest", targetDir, false, nil)
	require.NoError(t, err)
	require.True(t, called)
}

func TestPullerPull_FullPull_BackendPullError(t *testing.T) {
	tmpDir := t.TempDir()
	b, err := modctlBackend.New(filepath.Join(tmpDir, "modctl"))
	require.NoError(t, err)

	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(auth.GetKeyChainByRef, func(string) (*auth.PassKeyChain, error) {
		return &auth.PassKeyChain{ServerScheme: "https"}, nil
	})
	patches.ApplyFunc(modctlBackend.New, func(string) (modctlBackend.Backend, error) {
		return b, nil
	})
	patches.ApplyMethod(b, "Pull", func(modctlBackend.Backend, context.Context, string, *modctlConfig.Pull) error {
		return errors.New("boom")
	})

	targetDir := filepath.Join(tmpDir, "model")
	p := &puller{pullCfg: &config.PullConfig{Concurrency: 1}}

	err = p.Pull(context.Background(), "example.com/ns/model:latest", targetDir, false, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "pull model image")
}

func TestPullerPull_Fetch_Success(t *testing.T) {
	tmpDir := t.TempDir()
	b, err := modctlBackend.New(filepath.Join(tmpDir, "modctl"))
	require.NoError(t, err)

	ctx := context.Background()
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(auth.GetKeyChainByRef, func(string) (*auth.PassKeyChain, error) {
		return &auth.PassKeyChain{ServerScheme: "https"}, nil
	})
	patches.ApplyFunc(modctlBackend.New, func(string) (modctlBackend.Backend, error) {
		return b, nil
	})
	patches.ApplyMethod(reflect.TypeOf(&ModelArtifact{}), "GetPatterns", func(*ModelArtifact, context.Context, bool, []string) ([]string, int, error) {
		return []string{"README.md"}, 2, nil
	})
	called := false
	patches.ApplyMethod(b, "Fetch", func(modctlBackend.Backend, context.Context, string, *modctlConfig.Fetch) error {
		called = true
		return nil
	})

	targetDir := filepath.Join(tmpDir, "model")
	p := &puller{pullCfg: &config.PullConfig{Concurrency: 1}, hook: status.NewHook(ctx)}

	err = p.Pull(ctx, "example.com/ns/model:latest", targetDir, true, nil)
	require.NoError(t, err)
	require.True(t, called)
}

func TestPullerPull_Fetch_Error(t *testing.T) {
	tmpDir := t.TempDir()
	b, err := modctlBackend.New(filepath.Join(tmpDir, "modctl"))
	require.NoError(t, err)

	ctx := context.Background()
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(auth.GetKeyChainByRef, func(string) (*auth.PassKeyChain, error) {
		return &auth.PassKeyChain{ServerScheme: "https"}, nil
	})
	patches.ApplyFunc(modctlBackend.New, func(string) (modctlBackend.Backend, error) {
		return b, nil
	})
	patches.ApplyMethod(reflect.TypeOf(&ModelArtifact{}), "GetPatterns", func(*ModelArtifact, context.Context, bool, []string) ([]string, int, error) {
		return []string{"README.md"}, 2, nil
	})
	patches.ApplyMethod(b, "Fetch", func(modctlBackend.Backend, context.Context, string, *modctlConfig.Fetch) error {
		return errors.New("boom")
	})

	targetDir := filepath.Join(tmpDir, "model")
	p := &puller{pullCfg: &config.PullConfig{Concurrency: 1}, hook: status.NewHook(ctx)}

	err = p.Pull(ctx, "example.com/ns/model:latest", targetDir, true, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "fetch model")
}
