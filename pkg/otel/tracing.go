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
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/azex-ai/ledger"

// AttributePolicy controls which span attributes StartSpan forwards to
// whatever tracer provider the consumer has configured (I-N16). Call sites
// across postgres/*.go pass every attribute they have on hand -- amount,
// account holder, idempotency key included -- and rely entirely on this
// package to decide what actually reaches an exporter, so tightening or
// loosening the policy never means touching a call site.
//
// Before this existed, configuring a global tracer provider for ANY reason
// (a request-latency dashboard, an unrelated service's APM rollout sharing
// the same process) meant every PostJournal/Reserve call started exporting
// exact amounts and account_holder ids to whatever backend that provider
// was wired to -- silently, with no policy layer and no way to opt out
// short of not tracing at all.
type AttributePolicy int32

const (
	// PolicyMinimal drops the attribute keys known to carry a financial
	// amount, an account identifier, or a value a downstream system could
	// replay (idempotency_key), and keeps everything else -- uid-shaped
	// identifiers and bounded enums such as journal_type_uid, template_code,
	// currency_uid, source, to_status. This is the default.
	PolicyMinimal AttributePolicy = iota
	// PolicyFull forwards every attribute a call site passes, unfiltered.
	// Opt in only once you have confirmed your APM vendor's trust boundary
	// makes per-transaction amounts and account ids acceptable to export.
	PolicyFull
)

// policy is process-wide (mirroring otel.SetTracerProvider's own global,
// process-wide configuration model) and set once at startup, before
// traffic -- but stored atomically so a concurrent StartSpan is never a
// data race regardless.
var policy atomic.Int32

// SetAttributePolicy sets the process-wide span attribute filtering policy
// applied by every subsequent StartSpan call. Safe to call concurrently
// with StartSpan.
func SetAttributePolicy(p AttributePolicy) { policy.Store(int32(p)) }

// sensitiveAttributeKeys is PolicyMinimal's drop list. Deliberately an
// explicit named set rather than a pattern match: a new attribute key at a
// future call site defaults to KEPT, matching the existing convention that
// everything attached here is uid/enum-shaped unless it is one of these
// four known exceptions.
var sensitiveAttributeKeys = map[attribute.Key]struct{}{
	"amount":          {},
	"actual_amount":   {},
	"account_holder":  {},
	"actor_id":        {},
	"idempotency_key": {},
}

// filterAttributes applies the current AttributePolicy. Returns attrs
// unmodified under PolicyFull (including a nil/empty slice, and without
// allocating).
func filterAttributes(attrs []attribute.KeyValue) []attribute.KeyValue {
	if AttributePolicy(policy.Load()) == PolicyFull || len(attrs) == 0 {
		return attrs
	}
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		if _, sensitive := sensitiveAttributeKeys[a.Key]; sensitive {
			continue
		}
		out = append(out, a)
	}
	return out
}

// StartSpan starts a new span with the given name and optional attributes,
// filtered through the current AttributePolicy (default PolicyMinimal). The
// caller must end the span when done:
//
//	ctx, span := otel.StartSpan(ctx, "ledger.store.method", ...)
//	defer span.End()
//
// If no tracer provider has been registered, the otel SDK's built-in no-op
// tracer is used automatically — this function never blocks or panics.
func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	tracer := otel.GetTracerProvider().Tracer(tracerName)
	opts := []trace.SpanStartOption{}
	attrs = filterAttributes(attrs)
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
