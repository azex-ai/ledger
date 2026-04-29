package otel_test

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	ledgerotel "github.com/azex-ai/ledger/pkg/otel"

	otelglobal "go.opentelemetry.io/otel"
)

func TestStartSpan_NoOp(t *testing.T) {
	// Without any registered provider, the global SDK uses a no-op tracer.
	// StartSpan should return a valid (no-op) span without panicking.
	ctx, span := ledgerotel.StartSpan(context.Background(), "ledger.test.noop")
	defer span.End()

	if ctx == nil {
		t.Fatal("expected non-nil context")
	}
	if span == nil {
		t.Fatal("expected non-nil span")
	}
}

func TestStartSpan_WithAttributes(t *testing.T) {
	// No-op path: attributes accepted without panic.
	_, span := ledgerotel.StartSpan(context.Background(), "ledger.test.attrs",
		attribute.Int64("currency_id", 1),
		attribute.Int64("account_holder", 42),
	)
	defer span.End()

	if span == nil {
		t.Fatal("expected non-nil span")
	}
}

func TestRecordError_Nil(t *testing.T) {
	_, span := ledgerotel.StartSpan(context.Background(), "ledger.test.no_error")
	defer span.End()
	// nil error must be a no-op — this should not panic
	ledgerotel.RecordError(span, nil)
}

func TestRecordError_NonNil(t *testing.T) {
	// Wire an in-memory exporter so we can verify span status.
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
	)
	otelglobal.SetTracerProvider(tp)
	t.Cleanup(func() {
		// Reset to default no-op provider after test.
		otelglobal.SetTracerProvider(otelglobal.GetTracerProvider())
	})

	ctx, span := ledgerotel.StartSpan(context.Background(), "ledger.test.with_error",
		attribute.String("idempotency_key", "test-key-123"),
	)
	_ = ctx

	testErr := errors.New("insufficient balance")
	ledgerotel.RecordError(span, testErr)
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	s := spans[0]
	if s.Name != "ledger.test.with_error" {
		t.Errorf("unexpected span name: %q", s.Name)
	}
	if s.Status.Code != codes.Error {
		t.Errorf("expected Error status, got %v", s.Status.Code)
	}

	// Verify attribute is present.
	var found bool
	for _, attr := range s.Attributes {
		if attr.Key == "idempotency_key" && attr.Value.AsString() == "test-key-123" {
			found = true
		}
	}
	if !found {
		t.Error("idempotency_key attribute not found on span")
	}
}

func TestStartSpan_PropagatesContext(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
	)
	otelglobal.SetTracerProvider(tp)
	t.Cleanup(func() {
		otelglobal.SetTracerProvider(otelglobal.GetTracerProvider())
	})

	// Parent span.
	ctx, parent := ledgerotel.StartSpan(context.Background(), "ledger.parent")

	// Child span inherits the trace ID from parent.
	_, child := ledgerotel.StartSpan(ctx, "ledger.child")
	child.End()
	parent.End()

	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("expected 2 spans, got %d", len(spans))
	}

	var parentID, childParentID oteltrace.SpanID
	for _, s := range spans {
		if s.Name == "ledger.parent" {
			parentID = s.SpanContext.SpanID()
		}
		if s.Name == "ledger.child" {
			childParentID = s.Parent.SpanID()
		}
	}

	if parentID != childParentID {
		t.Errorf("child span parent ID %v does not match parent span ID %v", childParentID, parentID)
	}
}
