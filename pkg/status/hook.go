package status

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	oldModelspec "github.com/dragonflyoss/model-spec/specs-go/v1"
	"github.com/dustin/go-humanize"
	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/modelpack/model-csi-driver/pkg/metrics"
	"github.com/modelpack/model-csi-driver/pkg/tracing"
	modelspec "github.com/modelpack/model-spec/specs-go/v1"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.opentelemetry.io/otel/attribute"
	otelCodes "go.opentelemetry.io/otel/codes"
)

type HookManager struct {
	mutex sync.RWMutex
	hooks map[string]*Hook
}

func NewHookManager() *HookManager {
	return &HookManager{
		hooks: make(map[string]*Hook),
	}
}

func (hm *HookManager) GetProgress(key string) Progress {
	hm.mutex.RLock()
	defer hm.mutex.RUnlock()

	hook, exists := hm.hooks[key]
	if exists {
		return hook.GetProgress()
	}

	return Progress{}
}

func (hm *HookManager) Set(key string, hook *Hook) {
	hm.mutex.Lock()
	defer hm.mutex.Unlock()

	hm.hooks[key] = hook
}

func (hm *HookManager) Delete(key string) {
	hm.mutex.Lock()
	defer hm.mutex.Unlock()

	delete(hm.hooks, key)
}

type Hook struct {
	ctx      context.Context
	mutex    sync.RWMutex
	manifest *ocispec.Manifest
	pulled   atomic.Uint32
	progress map[digest.Digest]*ProgressItem
}

func NewHook(ctx context.Context) *Hook {
	return &Hook{
		ctx:      ctx,
		progress: make(map[digest.Digest]*ProgressItem),
	}
}

func (h *Hook) getProgressDesc() string {
	finished := h.pulled.Load()
	if h.manifest == nil {
		return fmt.Sprintf("%d/unknown", finished)
	}

	total := len(h.manifest.Layers)

	return fmt.Sprintf("%d/%d", finished, total)
}

func (h *Hook) BeforePullLayer(desc ocispec.Descriptor, manifest ocispec.Manifest) {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	filePath := ""
	if desc.Annotations != nil {
		if desc.Annotations[modelspec.AnnotationFilepath] != "" {
			filePath = fmt.Sprintf("/%s", desc.Annotations[modelspec.AnnotationFilepath])
		} else if desc.Annotations[oldModelspec.AnnotationFilepath] != "" {
			// Support old annotation for backward compatibility
			filePath = fmt.Sprintf("/%s", desc.Annotations[oldModelspec.AnnotationFilepath])
		}
	}

	_, span := tracing.Tracer.Start(h.ctx, "PullLayer")
	span.SetAttributes(attribute.String("digest", desc.Digest.String()))
	span.SetAttributes(attribute.String("media_type", desc.MediaType))
	span.SetAttributes(attribute.String("file_path", filePath))
	span.SetAttributes(attribute.Int64("size", desc.Size))

	h.manifest = &manifest
	h.progress[desc.Digest] = &ProgressItem{
		Digest:     desc.Digest,
		Path:       filePath,
		Size:       desc.Size,
		StartedAt:  time.Now(),
		FinishedAt: nil,
		Error:      nil,
		Span:       span,
	}
}

func (h *Hook) AfterPullLayer(desc ocispec.Descriptor, err error) {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	progress := h.progress[desc.Digest]
	if progress == nil {
		return
	}

	metrics.NodePullOpObserve("pull_layer", progress.Size, progress.StartedAt, err)

	var finishedAt *time.Time
	if err != nil {
		logger.WithContext(h.ctx).WithError(err).Errorf("failed to pull layer: %s%s (%s)", progress.Digest, progress.Path, h.getProgressDesc())
	} else {
		now := time.Now()
		finishedAt = &now
		h.pulled.Add(1)
		duration := time.Since(progress.StartedAt)
		logger.WithContext(h.ctx).Infof(
			"pulled layer: %s %s %s %s (%s) %s",
			desc.MediaType, progress.Digest, progress.Path, humanize.Bytes(uint64(progress.Size)), h.getProgressDesc(), duration,
		)
	}

	progress.FinishedAt = finishedAt
	progress.Error = err

	if err != nil {
		progress.Span.SetStatus(otelCodes.Error, "failed to pull layer")
		progress.Span.RecordError(err)
	}
	progress.Span.End()
}

func (h *Hook) getProgress() Progress {
	items := []ProgressItem{}
	for _, item := range h.progress {
		items = append(items, *item)
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].StartedAt.Equal(items[j].StartedAt) {
			return items[i].Digest < items[j].Digest
		}
		return items[i].StartedAt.Before(items[j].StartedAt)
	})

	total := 0
	if h.manifest != nil {
		digestMap := make(map[digest.Digest]bool)
		for idx := range h.manifest.Layers {
			layer := h.manifest.Layers[idx]
			digestMap[layer.Digest] = true
		}
		total = len(digestMap)
	}

	return Progress{
		Total: total,
		Items: items,
	}
}

func (h *Hook) GetProgress() Progress {
	h.mutex.RLock()
	defer h.mutex.RUnlock()

	return h.getProgress()
}
