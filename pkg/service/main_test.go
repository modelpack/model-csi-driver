package service

import (
	"os"
	"testing"

	"go.opentelemetry.io/otel/trace/noop"

	"github.com/modelpack/model-csi-driver/pkg/tracing"
)

func TestMain(m *testing.M) {
	// Initialize a noop tracer so tests that call tracing.Tracer.Start don't panic.
	tracing.Tracer = noop.NewTracerProvider().Tracer("service-test")
	os.Exit(m.Run())
}
