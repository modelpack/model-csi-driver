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

func TestModelArtifactInspect_InvalidResult(t *testing.T) {
	tmpDir := t.TempDir()
	b, err := backend.New(filepath.Join(tmpDir, "modctl"))
	require.NoError(t, err)

	patch := gomonkey.ApplyMethod(b, "Inspect",
		func(backend.Backend, context.Context, string, *modctlConfig.Inspect) (interface{}, error) {
			return "invalid", nil
		})
	defer patch.Reset()

	modelArtifact := NewModelArtifact(b, "test/model:latest", true)
	artifact, err := modelArtifact.Inspect(context.Background(), "test/model:latest")
	require.Nil(t, artifact)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid inspected result: test/model:latest")
}

func TestModelArtifactInspect_BackendError(t *testing.T) {
	tmpDir := t.TempDir()
	b, err := backend.New(filepath.Join(tmpDir, "modctl"))
	require.NoError(t, err)

	patch := gomonkey.ApplyMethod(b, "Inspect",
		func(backend.Backend, context.Context, string, *modctlConfig.Inspect) (interface{}, error) {
			return nil, os.ErrNotExist
		})
	defer patch.Reset()

	modelArtifact := NewModelArtifact(b, "test/model:latest", true)
	artifact, err := modelArtifact.Inspect(context.Background(), "test/model:latest")
	require.Nil(t, artifact)
	require.Error(t, err)
	require.Contains(t, err.Error(), "inspect model: test/model:latest")
}

func TestModelArtifactGetPatterns_InspectError(t *testing.T) {
	tmpDir := t.TempDir()
	b, err := backend.New(filepath.Join(tmpDir, "modctl"))
	require.NoError(t, err)

	patch := gomonkey.ApplyMethod(b, "Inspect",
		func(backend.Backend, context.Context, string, *modctlConfig.Inspect) (interface{}, error) {
			return nil, os.ErrPermission
		})
	defer patch.Reset()

	modelArtifact := NewModelArtifact(b, "test/model:latest", true)
	paths, total, err := modelArtifact.GetPatterns(context.Background(), false, nil)
	require.Nil(t, paths)
	require.Zero(t, total)
	require.Error(t, err)
	require.Contains(t, err.Error(), "get layers for model: test/model:latest")
}

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
						Filepath:  "bar.zoo.safetensors",
					},
				},
			}, nil
		})
	defer patch.Reset()

	modelArtifact := NewModelArtifact(b, "test/model:latest", true)

	size, err := modelArtifact.GetSize(ctx, false, nil)
	require.NoError(t, err)
	require.Equal(t, int64(5*1024*1024), size)

	size, err = modelArtifact.GetSize(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, int64(2*1024*1024), size)

	paths, total, err := modelArtifact.GetPatterns(ctx, false, nil)
	require.NoError(t, err)
	require.Equal(t, 3, total)
	require.Equal(t, []string{"foo.safetensors", "README.md", "bar.zoo.safetensors"}, paths)

	paths, total, err = modelArtifact.GetPatterns(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 3, total)
	require.Equal(t, []string{"README.md"}, paths)

	// exclude_file_patterns > exclude_model_weights:
	// negation pattern "!foo.safetensors" forces inclusion of that weight file
	// even though exclude_model_weights=true would normally omit it.
	paths, _, err = modelArtifact.GetPatterns(ctx, true, []string{"!foo.safetensors"})
	require.NoError(t, err)
	require.Equal(t, []string{"foo.safetensors", "README.md"}, paths)

	// Exclude by glob pattern only (no exclude_model_weights)
	paths, _, err = modelArtifact.GetPatterns(ctx, false, []string{"*.safetensors"})
	require.NoError(t, err)
	require.Equal(t, []string{"README.md"}, paths)

	// Exclude by glob, then negate a specific file: last match wins.
	paths, _, err = modelArtifact.GetPatterns(ctx, false, []string{"*.safetensors", "!foo.safetensors"})
	require.NoError(t, err)
	require.Equal(t, []string{"foo.safetensors", "README.md"}, paths)
}

func TestMatchFilePatterns(t *testing.T) {
	tests := []struct {
		name         string
		filename     string
		patterns     []string
		wantMatched  bool
		wantExcluded bool
	}{
		{
			name:         "no patterns",
			filename:     "model.safetensors",
			patterns:     nil,
			wantMatched:  false,
			wantExcluded: false,
		},
		{
			name:         "exact match excludes",
			filename:     "model.safetensors.index.json",
			patterns:     []string{"model.safetensors.index.json"},
			wantMatched:  true,
			wantExcluded: true,
		},
		{
			name:         "glob match excludes",
			filename:     "model-00001-of-00003.safetensors",
			patterns:     []string{"*.safetensors"},
			wantMatched:  true,
			wantExcluded: true,
		},
		{
			name:         "negation overrides earlier exclude",
			filename:     "tiktoken.model",
			patterns:     []string{"*.model", "!tiktoken.model"},
			wantMatched:  true,
			wantExcluded: false,
		},
		{
			name:         "last matching pattern wins (exclude after negate)",
			filename:     "tiktoken.model",
			patterns:     []string{"!tiktoken.model", "*.model"},
			wantMatched:  true,
			wantExcluded: true,
		},
		{
			name:         "no match returns unmatched",
			filename:     "README.md",
			patterns:     []string{"*.safetensors"},
			wantMatched:  false,
			wantExcluded: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			matched, excluded := matchFilePatterns(tc.filename, tc.patterns)
			require.Equal(t, tc.wantMatched, matched)
			require.Equal(t, tc.wantExcluded, excluded)
		})
	}
}
