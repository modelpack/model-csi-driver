package service

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	modctlBackend "github.com/modelpack/modctl/pkg/backend"
	"github.com/modelpack/model-csi-driver/pkg/cas"
	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/config/auth"
	"github.com/modelpack/model-csi-driver/pkg/status"
	"github.com/stretchr/testify/require"
)

// wrapHooks: returns the inner status.Hook when CAS / OwnerKey are absent.
func TestPullerWrapHooks_NoCAS(t *testing.T) {
	innerHook := status.NewHook(context.Background())
	p := &puller{hook: innerHook}
	require.Equal(t, cas.PullHooks(innerHook), p.wrapHooks(context.Background(), "/tmp/x"))
}

// wrapHooks: returns a CAS adapter when the store and owner key are set.
func TestPullerWrapHooks_WithCAS(t *testing.T) {
	store, err := cas.NewStore(t.TempDir())
	require.NoError(t, err)
	innerHook := status.NewHook(context.Background())
	p := &puller{hook: innerHook, deps: PullerDeps{CAS: store, OwnerKey: "vol__m1"}}
	wrapped := p.wrapHooks(context.Background(), "/tmp/x")
	_, ok := wrapped.(*cas.PullHook)
	require.True(t, ok)
}

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
