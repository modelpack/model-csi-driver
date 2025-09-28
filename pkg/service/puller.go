package service

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/modelpack/modctl/pkg/backend"
	modctlConfig "github.com/modelpack/modctl/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/config/auth"
	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/modelpack/model-csi-driver/pkg/metrics"
	"github.com/modelpack/model-csi-driver/pkg/status"
	"github.com/modelpack/model-csi-driver/pkg/tracing"
	modelspec "github.com/modelpack/model-spec/specs-go/v1"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel/attribute"
	otelCodes "go.opentelemetry.io/otel/codes"
)

const (
	safetensorIndexFilePath = "model.safetensors.index.json"
)

type PullHook interface {
	BeforePullLayer(desc ocispec.Descriptor, manifest ocispec.Manifest)
	AfterPullLayer(desc ocispec.Descriptor, err error)
}

type Hook struct {
	ctx        context.Context
	mutex      sync.Mutex
	manifest   *ocispec.Manifest
	pulled     atomic.Uint32
	progress   map[digest.Digest]*status.ProgressItem
	progressCb func(progress status.Progress)
}

func NewHook(ctx context.Context, progressCb func(progress status.Progress)) *Hook {
	return &Hook{
		ctx:        ctx,
		progress:   make(map[digest.Digest]*status.ProgressItem),
		progressCb: progressCb,
	}
}

type Puller interface {
	Pull(ctx context.Context, reference, targetDir string, excludeModelWeights bool) error
}

var NewPuller = func(ctx context.Context, pullCfg *config.PullConfig, hook *Hook, diskQuotaChecker *DiskQuotaChecker) Puller {
	return &puller{
		pullCfg:          pullCfg,
		hook:             hook,
		diskQuotaChecker: diskQuotaChecker,
	}
}

type puller struct {
	pullCfg          *config.PullConfig
	hook             *Hook
	diskQuotaChecker *DiskQuotaChecker
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
	if desc.Annotations != nil && desc.Annotations[modelspec.AnnotationFilepath] != "" {
		filePath = fmt.Sprintf("/%s", desc.Annotations[modelspec.AnnotationFilepath])
	}

	_, span := tracing.Tracer.Start(h.ctx, "PullLayer")
	span.SetAttributes(attribute.String("digest", desc.Digest.String()))
	span.SetAttributes(attribute.String("media_type", desc.MediaType))
	span.SetAttributes(attribute.String("file_path", filePath))
	span.SetAttributes(attribute.Int64("size", desc.Size))

	h.manifest = &manifest
	h.progress[desc.Digest] = &status.ProgressItem{
		Digest:     desc.Digest,
		Path:       filePath,
		Size:       desc.Size,
		StartedAt:  time.Now(),
		FinishedAt: nil,
		Error:      nil,
		Span:       span,
	}

	h.progressCb(h.getProgress())
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

	h.progressCb(h.getProgress())
}

func (p *puller) checkLongPulling(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	recorded := map[digest.Digest]bool{}

	for {
		select {
		case <-ticker.C:
			p.hook.mutex.Lock()
			for _, progress := range p.hook.progress {
				if progress.FinishedAt == nil &&
					p.pullCfg.PullLayerTimeoutInSeconds > 0 &&
					time.Since(progress.StartedAt) > time.Duration(p.pullCfg.PullLayerTimeoutInSeconds)*time.Second &&
					!recorded[progress.Digest] {
					logger.WithContext(ctx).Warnf("pulling layer %s is taking too long: %s", progress.Digest, time.Since(progress.StartedAt))
					metrics.NodePullLayerTooLong.Inc()
					recorded[progress.Digest] = true
				}
			}
			p.hook.mutex.Unlock()
		case <-ctx.Done():
			return
		}
	}
}

func (h *Hook) getProgress() status.Progress {
	items := []status.ProgressItem{}
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
		total = len(h.manifest.Layers)
	}
	return status.Progress{
		Total: total,
		Items: items,
	}
}

func (h *Hook) GetProgress() status.Progress {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	return h.getProgress()
}

func (p *puller) Pull(ctx context.Context, reference, targetDir string, excludeModelWeights bool) error {
	keyChain, err := auth.GetKeyChainByRef(reference)
	if err != nil {
		return errors.Wrapf(err, "get auth for model: %s", reference)
	}
	plainHTTP := keyChain.ServerScheme == "http"

	b, err := backend.New("")
	if err != nil {
		return errors.Wrap(err, "create modctl backend")
	}

	modelArtifact := NewModelArtifact(b, reference, plainHTTP)

	if p.diskQuotaChecker != nil {
		if err := p.diskQuotaChecker.Check(ctx, modelArtifact, excludeModelWeights); err != nil {
			return errors.Wrap(err, "check disk quota")
		}
	}

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return errors.Wrapf(err, "create model dir: %s", targetDir)
	}

	if !excludeModelWeights {
		go p.checkLongPulling(ctx)

		pullConfig := modctlConfig.NewPull()
		pullConfig.Concurrency = int(p.pullCfg.Concurrency)
		pullConfig.PlainHTTP = plainHTTP
		pullConfig.Proxy = p.pullCfg.ProxyURL
		pullConfig.DragonflyEndpoint = p.pullCfg.DragonflyEndpoint
		pullConfig.Insecure = true
		pullConfig.ExtractDir = targetDir
		pullConfig.ExtractFromRemote = true
		pullConfig.Hooks = p.hook
		pullConfig.ProgressWriter = io.Discard
		pullConfig.DisableProgress = true

		if err := b.Pull(ctx, reference, pullConfig); err != nil {
			logger.WithContext(ctx).WithError(err).Errorf("failed to pull model image: %s", reference)
			return errors.Wrap(err, "pull model image")
		}

		return nil
	}

	patterns, err := modelArtifact.GetPatterns(ctx, excludeModelWeights)
	if err != nil {
		return errors.Wrap(err, "get model file patterns without weights")
	}

	logger.WithContext(ctx).Infof(
		"fetching model without weights: %s, file patterns: %s",
		reference, strings.Join(patterns, ", "),
	)

	fetchConfig := modctlConfig.NewFetch()
	fetchConfig.Concurrency = int(p.pullCfg.Concurrency)
	fetchConfig.PlainHTTP = plainHTTP
	fetchConfig.Proxy = p.pullCfg.ProxyURL
	fetchConfig.Insecure = true
	fetchConfig.Output = targetDir
	fetchConfig.Patterns = patterns

	if err := b.Fetch(ctx, reference, fetchConfig); err != nil {
		logger.WithContext(ctx).WithError(err).Errorf("failed to fetch model: %s", reference)
		return errors.Wrap(err, "fetch model")
	}

	return nil
}
