package tracing

import (
	"context"
	"testing"

	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestInit_EmptyEndpoint(t *testing.T) {
	cfg := config.NewWithRaw(&config.RawConfig{
		ServiceName: "test-service",
	})
	err := Init(cfg)
	require.NoError(t, err)
	require.NotNil(t, Tracer)
}

func TestInit_CalledTwice(t *testing.T) {
	cfg := config.NewWithRaw(&config.RawConfig{
		ServiceName: "test-service-2",
	})
	err := Init(cfg)
	require.NoError(t, err)

	err = Init(cfg)
	require.NoError(t, err)
	require.NotNil(t, Tracer)
}

func TestInit_WithEndpoint(t *testing.T) {
	cfg := config.NewWithRaw(&config.RawConfig{
		ServiceName:   "test-service-3",
		TraceEndpoint: "http://localhost:4318",
	})
	// otlptracehttp.New does not connect immediately, so this should succeed
	err := Init(cfg)
	require.NoError(t, err)
	require.NotNil(t, Tracer)
}

func TestNewPropagator(t *testing.T) {
	p := newPropagator()
	require.NotNil(t, p)
}

func TestNewTracerProvider_EmptyEndpoint(t *testing.T) {
	tp, err := newTracerProvider("")
	require.NoError(t, err)
	require.NotNil(t, tp)
}

func TestNewTracerProvider_WithEndpoint(t *testing.T) {
	// otlptracehttp.New is lazy - no actual connection until spans are flushed
	tp, err := newTracerProvider("http://localhost:4318")
	require.NoError(t, err)
	require.NotNil(t, tp)
}

func TestSetupOTelSDK_EmptyEndpoint(t *testing.T) {
	shutdown, err := setupOTelSDK(context.TODO(), "")
	require.NoError(t, err)
	require.NotNil(t, shutdown)
}

func TestSetupOTelSDK_WithEndpoint(t *testing.T) {
	shutdown, err := setupOTelSDK(context.TODO(), "http://localhost:4318")
	require.NoError(t, err)
	require.NotNil(t, shutdown)
}
