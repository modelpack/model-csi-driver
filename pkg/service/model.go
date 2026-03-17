package service

import (
	"context"
	"path/filepath"
	"sync"
	"time"

	gitignore "github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/modelpack/modctl/pkg/backend"
	modctlConfig "github.com/modelpack/modctl/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/modelpack/model-csi-driver/pkg/utils"
	"github.com/pkg/errors"
)

type ModelArtifact struct {
	Reference string

	b         backend.Backend
	plainHTTP bool

	mutex    sync.Mutex
	artifact *backend.InspectedModelArtifact
}

func isWeightLayer(layer backend.InspectedModelArtifactLayer) bool {
	// For *.safetensors files
	if filepath.Ext(layer.Filepath) == ".safetensors" {
		return true
	}

	// For safetensors index file
	if layer.Filepath == "model.safetensors.index.json" {
		return true
	}

	return false
}

// matchFilePatterns matches filename against gitignore-style patterns using
// github.com/go-git/go-git/v5/plumbing/format/gitignore.
// Patterns are processed in order; the last matching pattern wins.
// A pattern prefixed with "!" negates the match (i.e. forces inclusion).
// Returns (matched=true, excluded=true) when the file should be excluded,
// (matched=true, excluded=false) when a negation pattern overrides, and
// (matched=false, _) when no pattern matches (caller applies fallback logic).
func matchFilePatterns(filename string, patterns []string) (matched bool, excluded bool) {
	for _, p := range patterns {
		result := gitignore.ParsePattern(p, nil).Match([]string{filename}, false)
		if result == gitignore.NoMatch {
			continue
		}
		matched = true
		excluded = result == gitignore.Exclude
	}
	return
}

func NewModelArtifact(b backend.Backend, reference string, plainHTTP bool) *ModelArtifact {
	return &ModelArtifact{
		Reference: reference,
		b:         b,
		plainHTTP: plainHTTP,
	}
}

func (m *ModelArtifact) Inspect(ctx context.Context, reference string) (*backend.InspectedModelArtifact, error) {
	start := time.Now()
	defer func() {
		logger.Logger().WithContext(ctx).Infof(
			"inspected model %s, duration: %s", reference, time.Since(start),
		)
	}()
	var result any
	if err := utils.WithRetry(ctx, func() error {
		var err error
		result, err = m.b.Inspect(ctx, reference, &modctlConfig.Inspect{
			Remote:    true,
			Insecure:  true,
			PlainHTTP: m.plainHTTP,
		})
		return err
	}, 3, 1*time.Second); err != nil {
		return nil, errors.Wrapf(err, "inspect model: %s", reference)
	}

	artifact, ok := result.(*backend.InspectedModelArtifact)
	if !ok {
		return nil, errors.Errorf("invalid inspected result: %s", reference)
	}

	return artifact, nil
}

func (m *ModelArtifact) inspect(ctx context.Context) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if m.artifact != nil {
		return nil
	}

	artifact, err := m.Inspect(ctx, m.Reference)
	if err != nil {
		return errors.Wrapf(err, "inspect model: %s", m.Reference)
	}

	m.artifact = artifact

	return nil
}

func (m *ModelArtifact) getLayers(ctx context.Context, excludeWeights bool, excludeFilePatterns []string) (
	[]backend.InspectedModelArtifactLayer, int, error,
) {
	if err := m.inspect(ctx); err != nil {
		return nil, 0, errors.Wrapf(err, "inspect model: %s", m.Reference)
	}

	layers := []backend.InspectedModelArtifactLayer{}
	for idx := range m.artifact.Layers {
		layer := m.artifact.Layers[idx]

		// If no filtering is requested, include all layers without further checks.
		if !excludeWeights && len(excludeFilePatterns) == 0 {
			layers = append(layers, layer)
			continue
		}

		if layer.Filepath == "" {
			logger.Logger().WithContext(ctx).Warnf(
				"layer %s has no file path, skip", layer.Digest,
			)
			continue
		}

		filename := filepath.Base(layer.Filepath)

		// exclude_file_patterns takes precedence over exclude_model_weights.
		if matched, excluded := matchFilePatterns(filename, excludeFilePatterns); matched {
			if !excluded {
				layers = append(layers, layer)
			}
			continue
		}

		// Fallback: apply weight-based exclusion.
		if !excludeWeights || !isWeightLayer(layer) {
			layers = append(layers, layer)
		}
	}

	return layers, len(m.artifact.Layers), nil
}

func (m *ModelArtifact) GetSize(ctx context.Context, excludeWeights bool, excludeFilePatterns []string) (int64, error) {
	layers, _, err := m.getLayers(ctx, excludeWeights, excludeFilePatterns)
	if err != nil {
		return 0, errors.Wrapf(err, "get layers for model: %s", m.Reference)
	}

	totalSize := int64(0)
	digestMap := make(map[string]bool)
	for idx := range layers {
		layer := layers[idx]
		if _, exists := digestMap[layer.Digest]; exists {
			continue
		}
		totalSize += layer.Size
		digestMap[layer.Digest] = true
	}

	return totalSize, nil
}

func (m *ModelArtifact) GetPatterns(ctx context.Context, excludeWeights bool, excludeFilePatterns []string) ([]string, int, error) {
	layers, total, err := m.getLayers(ctx, excludeWeights, excludeFilePatterns)
	if err != nil {
		return nil, 0, errors.Wrapf(err, "get layers for model: %s", m.Reference)
	}

	paths := []string{}
	for idx := range layers {
		paths = append(paths, layers[idx].Filepath)
	}

	return paths, total, nil
}
