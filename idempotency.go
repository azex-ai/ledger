package ledger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/azex-ai/ledger/core"
)

// NewIdempotencyKey generates an idempotency key in the form "<scope>:<16-byte-hex>".
// The random suffix is produced by crypto/rand so it is safe for concurrent use
// and free from timestamp collisions.
//
// # Generate once, outside the retry loop
//
// A key identifies one logical submission, not one attempt. Generating a
// fresh key on retry does not replay the original write — it posts a second,
// independent one. A first attempt that times out after the journal landed,
// retried with a new key, is a double entry with no error anywhere
// (api-contract.md §9: "generated once by the initiator and reused across
// retries"). This holds identically in library mode and HTTP mode.
//
// So bind the key to a variable before the loop, never inline it into the
// input literal:
//
//	key := ledger.NewIdempotencyKey("deposit")
//	for attempt := 0; attempt < 3; attempt++ {
//	    _, err = svc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
//	        IdempotencyKey: key, // the SAME key every attempt
//	        // ...
//	    })
//	    if err == nil || !core.IsRetryable(err) {
//	        break
//	    }
//	}
//
// RetryIdempotent does exactly that and makes the mistake unexpressible.
func NewIdempotencyKey(scope string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is a system-level error (e.g. exhausted entropy pool).
		// Panic is appropriate here — the process cannot generate safe random IDs.
		panic(fmt.Sprintf("ledger: NewIdempotencyKey: crypto/rand failed: %v", err))
	}
	return scope + ":" + hex.EncodeToString(b[:])
}

// RetryIdempotent runs fn up to attempts times against ONE idempotency key,
// generated once before the first attempt and handed to every one of them.
// It exists so the two primitives this library already offers —
// NewIdempotencyKey and core.IsRetryable — cannot be combined into the
// duplicate-write shape described on NewIdempotencyKey: the key is not
// reachable from inside fn's caller, so there is nowhere to regenerate it.
//
//	err := ledger.RetryIdempotent(ctx, "deposit", 3, func(ctx context.Context, key string) error {
//	    _, err := svc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
//	        IdempotencyKey: key,
//	        // ...
//	    })
//	    return err
//	})
//
// It retries only while core.IsRetryable says the failure is transient, and
// stops immediately on any other error — a rejected input or an insufficient
// balance does not become correct by being sent again. Backoff starts at
// 50ms and doubles, capped at 2s; ctx cancellation aborts the wait and is
// returned. attempts <= 0 is treated as 1.
//
// Deliberately not a policy: nothing in this library calls it. Callers with
// their own retry/backoff machinery should keep it and simply hoist the key
// out of the loop themselves.
func RetryIdempotent(ctx context.Context, scope string, attempts int, fn func(ctx context.Context, idempotencyKey string) error) error {
	if attempts <= 0 {
		attempts = 1
	}
	key := NewIdempotencyKey(scope)

	const (
		baseBackoff = 50 * time.Millisecond
		maxBackoff  = 2 * time.Second
	)

	var err error
	backoff := baseBackoff
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("ledger: retry %q: aborted after %d attempt(s): %w (last error: %v)", scope, attempt, ctx.Err(), err)
			case <-timer.C:
			}
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
		}

		err = fn(ctx, key)
		if err == nil {
			return nil
		}
		if !core.IsRetryable(err) {
			return err
		}
	}
	return fmt.Errorf("ledger: retry %q: still failing after %d attempt(s): %w", scope, attempts, err)
}
