package service

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	modctlBackend "github.com/modelpack/modctl/pkg/backend"
	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/config/auth"
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
