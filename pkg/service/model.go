package service

import (
	"sync"
	"time"

	legacymodelspec "github.com/dragonflyoss/model-spec/specs-go/v1"
	"github.com/modelpack/modctl/pkg/backend"
	modctlConfig "github.com/modelpack/modctl/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/modelpack/model-csi-driver/pkg/utils"
	modelspec "github.com/modelpack/model-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
)

type ModelArtifact struct {
	Reference string

	b         backend.Backend
	plainHTTP bool

	mutex    sync.Mutex
	artifact *backend.InspectedModelArtifact
}

func isSafetensorIndexFile(layer backend.InspectedModelArtifactLayer) bool {
	return layer.Filepath == safetensorIndexFilePath
}

func isWeightLayer(layer backend.InspectedModelArtifactLayer) bool {
	if layer.MediaType == modelspec.MediaTypeModelWeightRaw ||
		layer.MediaType == modelspec.MediaTypeModelWeight ||
		layer.MediaType == modelspec.MediaTypeModelWeightGzip ||
		layer.MediaType == modelspec.MediaTypeModelWeightZstd {
		return true
	}
	if layer.MediaType == legacymodelspec.MediaTypeModelWeightRaw ||
		layer.MediaType == legacymodelspec.MediaTypeModelWeight ||
		layer.MediaType == legacymodelspec.MediaTypeModelWeightGzip ||
		layer.MediaType == legacymodelspec.MediaTypeModelWeightZstd {
		return true
	}
	if isSafetensorIndexFile(layer) {
		return true
	}
	return false
}

func NewModelArtifact(b backend.Backend, reference string, plainHTTP bool) *ModelArtifact {
	return &ModelArtifact{
		Reference: reference,
		b:         b,
		plainHTTP: plainHTTP,
	}
}

func (m *ModelArtifact) inspect(ctx context.Context) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if m.artifact != nil {
		return nil
	}

	start := time.Now()
	defer func() {
		logger.Logger().WithContext(ctx).Infof(
			"inspected model %s, duration: %s", m.Reference, time.Since(start),
		)
	}()
	var result any
	if err := utils.WithRetry(ctx, func() error {
		var err error
		result, err = m.b.Inspect(ctx, m.Reference, &modctlConfig.Inspect{
			Remote:    true,
			Insecure:  true,
			PlainHTTP: m.plainHTTP,
		})
		return err
	}, 3, 1*time.Second); err != nil {
		return errors.Wrapf(err, "inspect model: %s", m.Reference)
	}

	artifact, ok := result.(*backend.InspectedModelArtifact)
	if !ok {
		return errors.Errorf("invalid inspected result: %s", m.Reference)
	}
	m.artifact = artifact

	return nil
}

func (m *ModelArtifact) getLayers(ctx context.Context, excludeWeights bool) ([]backend.InspectedModelArtifactLayer, error) {
	if err := m.inspect(ctx); err != nil {
		return nil, errors.Wrapf(err, "inspect model: %s", m.Reference)
	}

	layers := []backend.InspectedModelArtifactLayer{}
	for idx := range m.artifact.Layers {
		layer := m.artifact.Layers[idx]
		if excludeWeights {
			if layer.Filepath == "" {
				logger.Logger().WithContext(ctx).Warnf(
					"layer %s has no file path, skip", layer.Digest,
				)
				continue
			}
			if !isWeightLayer(layer) {
				layers = append(layers, layer)
			}
		} else {
			layers = append(layers, layer)
		}
	}

	return layers, nil
}

func (m *ModelArtifact) GetSize(ctx context.Context, excludeWeights bool) (int64, error) {
	layers, err := m.getLayers(ctx, excludeWeights)
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

func (m *ModelArtifact) GetPatterns(ctx context.Context, excludeWeights bool) ([]string, error) {
	layers, err := m.getLayers(ctx, excludeWeights)
	if err != nil {
		return nil, errors.Wrapf(err, "get layers for model: %s", m.Reference)
	}

	paths := []string{}
	for idx := range layers {
		paths = append(paths, layers[idx].Filepath)
	}

	return paths, nil
}
