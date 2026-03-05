package provider

import (
	"context"
	"net"
	"testing"

	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/service"
	"github.com/rexray/gocsi"
	"github.com/stretchr/testify/require"
	nooptrace "go.opentelemetry.io/otel/trace/noop"

	"github.com/modelpack/model-csi-driver/pkg/tracing"
)

func TestNew(t *testing.T) {
	// Initialize noop tracer so tracing.Tracer is not nil
	tracing.Tracer = nooptrace.NewTracerProvider().Tracer("provider-test")

	raw := &config.RawConfig{
		ServiceName: "test.csi.example.com",
		Mode:        "node",
		RootDir:     t.TempDir(),
	}
	cfg := config.NewWithRaw(raw)

	svc := &service.Service{}
	provider, err := New(cfg, svc)
	require.NoError(t, err)
	require.NotNil(t, provider)
}

func TestNew_BeforeServe(t *testing.T) {
	tracing.Tracer = nooptrace.NewTracerProvider().Tracer("provider-test")

	raw := &config.RawConfig{
		ServiceName: "test.csi.example.com",
		Mode:        "node",
		RootDir:     t.TempDir(),
	}
	cfg := config.NewWithRaw(raw)

	svc := &service.Service{}
	p, err := New(cfg, svc)
	require.NoError(t, err)

	sp := p.(*gocsi.StoragePlugin)
	// Call BeforeServe directly using a nil listener (the callback only logs)
	err = sp.BeforeServe(context.Background(), sp, (net.Listener)(nil))
	require.NoError(t, err)
}

