package tracing

import (
	"context"
	stderrors "errors"
	"io"
	"time"

	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

var Tracer trace.Tracer

func Init(cfg *config.Config) error {
	if cfg.TraceEndpooint != "" {
		logrus.Infof("initializing otel trace on %s", cfg.TraceEndpooint)
	}
	_, err := setupOTelSDK(context.Background(), cfg.TraceEndpooint)
	if err != nil {
		return errors.Wrap(err, "failed to initialize OpenTelemetry SDK")
	}
	Tracer = otel.Tracer(cfg.ServiceName + "/otel/model")
	return nil
}

// setupOTelSDK bootstraps the OpenTelemetry pipeline.
// If it does not return an error, make sure to call shutdown for proper cleanup.
func setupOTelSDK(ctx context.Context, endpointURL string) (shutdown func(context.Context) error, err error) {
	var shutdownFuncs []func(context.Context) error

	// shutdown calls cleanup functions registered via shutdownFuncs.
	// The errors from the calls are joined.
	// Each registered cleanup will be invoked once.
	shutdown = func(ctx context.Context) error {
		var err error
		for _, fn := range shutdownFuncs {
			err = stderrors.Join(err, fn(ctx))
		}
		shutdownFuncs = nil
		return err
	}

	// handleErr calls shutdown for cleanup and makes sure that all errors are returned.
	handleErr := func(inErr error) {
		err = stderrors.Join(inErr, shutdown(ctx))
	}

	// Set up propagator.
	prop := newPropagator()
	otel.SetTextMapPropagator(prop)

	// Set up trace provider.
	tracerProvider, err := newTracerProvider(endpointURL)
	if err != nil {
		handleErr(err)
		return
	}
	shutdownFuncs = append(shutdownFuncs, tracerProvider.Shutdown)
	otel.SetTracerProvider(tracerProvider)

	return
}

func newPropagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
}

func newTracerProvider(endpointURL string) (*sdktrace.TracerProvider, error) {
	var err error
	var traceExporter sdktrace.SpanExporter

	if endpointURL == "" {
		traceExporter, err = stdouttrace.New(
			stdouttrace.WithWriter(io.Discard),
		)
		if err != nil {
			return nil, err
		}
	} else {
		traceExporter, err = otlptracehttp.New(context.Background(), otlptracehttp.WithEndpointURL(endpointURL))
		if err != nil {
			return nil, err
		}
	}

	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter,
			sdktrace.WithBatchTimeout(5*time.Second)),
	)
	return tracerProvider, nil
}
