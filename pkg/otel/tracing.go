// Package otel provides lightweight helpers for OpenTelemetry trace
// instrumentation. It wraps the standard otel SDK patterns so that store
// methods only need a single call to start a span and record errors.
//
// When no tracer provider is configured by the caller, the otel SDK
// automatically uses a no-op tracer — so there is zero overhead in
// production deployments that have not installed an exporter.
package otel

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/azex-ai/ledger"

// StartSpan starts a new span with the given name and optional attributes.
// The caller must end the span when done:
//
//	ctx, span := otel.StartSpan(ctx, "ledger.store.method", ...)
//	defer span.End()
//
// If no tracer provider has been registered, the otel SDK's built-in no-op
// tracer is used automatically — this function never blocks or panics.
func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	tracer := otel.GetTracerProvider().Tracer(tracerName)
	opts := []trace.SpanStartOption{}
	if len(attrs) > 0 {
		opts = append(opts, trace.WithAttributes(attrs...))
	}
	return tracer.Start(ctx, name, opts...)
}

// RecordError records err on span and sets the span status to Error.
// If err is nil, it is a no-op.
func RecordError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}
