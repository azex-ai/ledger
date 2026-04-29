package delivery

import (
	"context"
	"fmt"

	"github.com/azex-ai/ledger/core"
)

// LocalDispatcher delivers events to in-process handlers by polling the same
// pending-events queue used by WebhookDeliverer.  It is intended for library
// mode where the caller registers callbacks via Worker.Subscribe() instead of
// configuring HTTP webhook endpoints.
//
// Claim-lease semantics: GetPendingEvents claims each event row for the
// duration of the lease by bumping next_attempt_at.  If the process crashes
// mid-batch the lease expires and the events become visible again.
//
// Error handling: if a handler returns an error, the event is logged and
// marked delivered anyway — we do not block or retry indefinitely.  The
// rationale is that in-process handlers are trusted code that should handle
// their own retry logic; stalling the queue on a buggy handler is worse than
// a missed notification.
type LocalDispatcher struct {
	poller   EventPoller
	callback *CallbackDeliverer
	logger   core.Logger
}

// NewLocalDispatcher creates a LocalDispatcher backed by the given poller.
// Register handlers via the embedded CallbackDeliverer (exposed as Callback).
func NewLocalDispatcher(poller EventPoller, logger core.Logger) *LocalDispatcher {
	return &LocalDispatcher{
		poller:   poller,
		callback: NewCallbackDeliverer(),
		logger:   logger,
	}
}

// SetPoller replaces the event poller. Call before Worker.Run().
func (d *LocalDispatcher) SetPoller(poller EventPoller) {
	d.poller = poller
}

// OnEvent registers an in-process callback. Thread-safe to call before
// Worker.Run(); not safe to call concurrently with Run().
func (d *LocalDispatcher) OnEvent(fn func(context.Context, core.Event) error) {
	d.callback.OnEvent(fn)
}

// ProcessBatch polls up to batchSize pending events, invokes registered
// handlers, and marks each event delivered (regardless of handler errors).
// Returns the number of events processed.
func (d *LocalDispatcher) ProcessBatch(ctx context.Context, batchSize int) (int, error) {
	if d.poller == nil {
		return 0, fmt.Errorf("delivery: local: event poller not configured")
	}
	events, err := d.poller.GetPendingEvents(ctx, batchSize)
	if err != nil {
		return 0, fmt.Errorf("delivery: local: poll: %w", err)
	}
	if len(events) == 0 {
		return 0, nil
	}

	for _, evt := range events {
		if invokeErr := d.callback.Deliver(ctx, evt); invokeErr != nil {
			d.logger.Error("delivery: local: handler error (marking delivered anyway)",
				"event_id", evt.ID,
				"error", invokeErr,
			)
		}
		// Always mark delivered — do not let a bad handler block the queue.
		if markErr := d.poller.MarkDelivered(ctx, evt.ID); markErr != nil {
			d.logger.Error("delivery: local: mark delivered failed",
				"event_id", evt.ID,
				"error", markErr,
			)
		}
	}

	return len(events), nil
}

