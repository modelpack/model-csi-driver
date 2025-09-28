package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/modelpack/modctl/pkg/backend"
	modctlConfig "github.com/modelpack/modctl/pkg/config"
	modelspec "github.com/modelpack/model-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

func TestModelArtifact(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "model-artifact-test-")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tmpDir) }()

	ctx := context.Background()
	b, err := backend.New(filepath.Join(tmpDir, "modctl"))
	require.NoError(t, err)
	patch := gomonkey.ApplyMethod(b, "Inspect",
		func(backend.Backend, context.Context, string, *modctlConfig.Inspect) (interface{}, error) {
			return &backend.InspectedModelArtifact{
				Layers: []backend.InspectedModelArtifactLayer{
					{
						MediaType: modelspec.MediaTypeModelWeightRaw,
						Digest:    "sha256:layer1",
						Size:      3 * 1024 * 1024,
						Filepath:  "foo.safetensors",
					},
					{
						MediaType: modelspec.MediaTypeModelDocRaw,
						Digest:    "sha256:layer2",
						Size:      2 * 1024 * 1024,
						Filepath:  "README.md",
					},
					{
						MediaType: modelspec.MediaTypeModelWeightRaw,
						Digest:    "sha256:layer1",
						Size:      3 * 1024 * 1024,
						Filepath:  "zoo.safetensors",
					},
				},
			}, nil
		})
	defer patch.Reset()

	modelArtifact := NewModelArtifact(b, "test/model:latest", true)

	size, err := modelArtifact.GetSize(ctx, false)
	require.NoError(t, err)
	require.Equal(t, int64(5*1024*1024), size)

	size, err = modelArtifact.GetSize(ctx, true)
	require.NoError(t, err)
	require.Equal(t, int64(2*1024*1024), size)

	paths, err := modelArtifact.GetPatterns(ctx, false)
	require.NoError(t, err)
	require.Equal(t, []string{"foo.safetensors", "README.md", "zoo.safetensors"}, paths)

	paths, err = modelArtifact.GetPatterns(ctx, true)
	require.NoError(t, err)
	require.Equal(t, []string{"README.md"}, paths)
}
