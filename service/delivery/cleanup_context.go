package delivery

import (
	"context"
	"time"
)

// cleanupCtxTimeout bounds the "record the outcome on the way out" calls in
// this package that must still run after the parent context was cancelled --
// worker shutdown mid-batch. Five seconds is generous for a single UPDATE
// round-trip and short enough not to noticeably delay shutdown.
const cleanupCtxTimeout = 5 * time.Second

// cleanupContext detaches from parent's cancellation so a cancelled parent
// (shutdown signal, deadline) does not abort the bookkeeping itself, while
// still carrying parent's values. The result is bounded by cleanupCtxTimeout.
//
// Use it at every "record a claim's outcome on the way out" call site --
// MarkDelivered, MarkRetry, RecordDeliveryStatus -- instead of the ctx that
// may have just been cancelled. Passing the cancelled ctx made the release
// call fail immediately (ctx.Err() != nil), which loses the outcome of a
// delivery that already happened: the claim lease then expires and the event
// is redelivered. At-least-once semantics make that survivable rather than
// incorrect, which is exactly why nothing ever noticed
// (concurrency.md 2026-09-02 B-m2).
//
// NOT for the delivery attempt itself: sendHTTP must keep the original ctx,
// because a shutdown SHOULD abort an in-flight outbound request. Only the
// step that records something that has already happened gets detached.
//
// This is a second implementation of service.cleanupContext, which is
// package-private and therefore unreachable from this sub-package. The
// duplication is a package boundary, not drift -- the two are the same five
// lines with the same rationale, and service/cleanup_context.go's doc
// cross-references this one.
func cleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), cleanupCtxTimeout)
}
