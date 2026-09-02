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

	// journal_type_uid is not in sensitiveAttributeKeys (I-N16) -- this test's
	// own intent is span status/error recording, not attribute filtering
	// (that has its own tests below), so it deliberately avoids a key that
	// PolicyMinimal (the default, unchanged by this test) would drop.
	ctx, span := ledgerotel.StartSpan(context.Background(), "ledger.test.with_error",
		attribute.String("journal_type_uid", "test-jt-123"),
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
		if attr.Key == "journal_type_uid" && attr.Value.AsString() == "test-jt-123" {
			found = true
		}
	}
	if !found {
		t.Error("journal_type_uid attribute not found on span")
	}
}

// TestStartSpan_DefaultPolicyDropsSensitiveAttributes pins I-N16: the
// default AttributePolicy (PolicyMinimal, never having called
// SetAttributePolicy) must strip amount/actual_amount/account_holder/
// actor_id/idempotency_key before a span reaches any configured exporter --
// before this fix, configuring a tracer provider for ANY reason started
// exporting every one of these on every PostJournal/Reserve call, with no
// policy layer and no opt-out short of not tracing at all.
func TestStartSpan_DefaultPolicyDropsSensitiveAttributes(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otelglobal.SetTracerProvider(tp)
	t.Cleanup(func() { otelglobal.SetTracerProvider(otelglobal.GetTracerProvider()) })

	_, span := ledgerotel.StartSpan(context.Background(), "ledger.test.sensitive",
		attribute.String("amount", "123.45"),
		attribute.String("actual_amount", "123.45"),
		attribute.Int64("account_holder", 42),
		attribute.Int64("actor_id", 7),
		attribute.String("idempotency_key", "should-be-dropped"),
		attribute.String("journal_type_uid", "kept-uid"),
	)
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	seen := map[string]bool{}
	for _, a := range spans[0].Attributes {
		seen[string(a.Key)] = true
	}
	for _, dropped := range []string{"amount", "actual_amount", "account_holder", "actor_id", "idempotency_key"} {
		if seen[dropped] {
			t.Errorf("PolicyMinimal (default) must drop attribute %q, but it was present on the span", dropped)
		}
	}
	if !seen["journal_type_uid"] {
		t.Error("PolicyMinimal must keep non-sensitive attributes like journal_type_uid")
	}
}

// TestStartSpan_PolicyFullKeepsEverything pins the opt-out half: a consumer
// that explicitly calls SetAttributePolicy(PolicyFull) gets every attribute
// a call site passes, unfiltered.
func TestStartSpan_PolicyFullKeepsEverything(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otelglobal.SetTracerProvider(tp)
	ledgerotel.SetAttributePolicy(ledgerotel.PolicyFull)
	t.Cleanup(func() {
		otelglobal.SetTracerProvider(otelglobal.GetTracerProvider())
		ledgerotel.SetAttributePolicy(ledgerotel.PolicyMinimal)
	})

	_, span := ledgerotel.StartSpan(context.Background(), "ledger.test.full",
		attribute.String("amount", "123.45"),
		attribute.Int64("account_holder", 42),
	)
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	seen := map[string]bool{}
	for _, a := range spans[0].Attributes {
		seen[string(a.Key)] = true
	}
	if !seen["amount"] || !seen["account_holder"] {
		t.Error("PolicyFull must forward every attribute a call site passes, including amount/account_holder")
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
