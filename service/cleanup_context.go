package service

import (
	"context"
	"time"
)

// cleanupCtxTimeout bounds cleanup operations (releasing rollup claims,
// advisory locks) that must still run after the parent context was cancelled
// — e.g. on worker shutdown mid-batch. Five seconds is generous for a single
// UPDATE / pg_advisory_unlock round-trip but short enough not to noticeably
// delay shutdown.
const cleanupCtxTimeout = 5 * time.Second

// cleanupContext detaches from parent's cancellation so a cancelled parent
// (shutdown signal, deadline) doesn't abort the cleanup itself, while still
// carrying parent's values. The result is bounded by cleanupCtxTimeout so a
// hung cleanup can't block shutdown forever.
//
// Use this at every "release a claim / lock on the way out" call site instead
// of the ctx that was just cancelled — passing the cancelled ctx directly
// means the release call fails immediately (ctx.Err() != nil), leaking the
// claim/lock until its lease expires.
func cleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), cleanupCtxTimeout)
}
