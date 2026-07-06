// Tracing bootstrap for service mode. pkg/otel spans go to the GLOBAL tracer
// provider, which is a no-op until something installs a real one — this file
// is that something. Enabled purely by the standard OpenTelemetry env vars:
// when OTEL_EXPORTER_OTLP_ENDPOINT (or the traces-specific variant) is set,
// an OTLP/HTTP exporter + batching provider is installed; otherwise tracing
// stays a zero-overhead no-op.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

// setupTracing installs the global OTLP tracer provider when configured.
// Returns a shutdown func (flushes buffered spans); no-op when disabled.
func setupTracing(ctx context.Context, logger *slog.Logger) (func(context.Context) error, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	}
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	// The exporter reads the full OTEL_EXPORTER_OTLP_* env surface itself
	// (endpoint, headers, TLS, compression) — we only gate on presence.
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("otel: create otlp exporter: %w", err)
	}

	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "ledgerd"
	}
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(serviceName),
	))
	if err != nil {
		return nil, fmt.Errorf("otel: build resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	logger.Info("tracing enabled", "exporter", "otlp/http", "endpoint", endpoint, "service_name", serviceName)
	return tp.Shutdown, nil
}
