package delivery

import (
	"context"
	"fmt"
	"time"

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
// Error handling: if any handler returns an error the event is scheduled for
// retry (bounded by max_attempts, after which the store transitions it to
// 'dead') — the same at-least-once semantics as WebhookDeliverer. Handlers
// must therefore be idempotent per event UID. A permanently failing handler
// parks its events in 'dead' rather than stalling the queue forever.
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
// handlers, marks each fully-handled event delivered, and schedules failed
// ones for retry. Returns the number of events processed.
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
		if invokeErr := d.callback.Deliver(ctx, evt.Event); invokeErr != nil {
			d.logger.Error("delivery: local: handler error, scheduling retry",
				"event_id", evt.InternalID,
				"attempts", evt.Attempts,
				"error", invokeErr,
			)
			if retryErr := d.poller.MarkRetry(ctx, evt.InternalID, evt.ClaimToken, time.Now().Add(retryDelay(evt.Attempts))); retryErr != nil {
				d.logger.Error("delivery: local: mark retry failed",
					"event_id", evt.InternalID,
					"error", retryErr,
				)
			}
			continue
		}
		if markErr := d.poller.MarkDelivered(ctx, evt.InternalID, evt.ClaimToken); markErr != nil {
			d.logger.Error("delivery: local: mark delivered failed",
				"event_id", evt.InternalID,
				"error", markErr,
			)
		}
	}

	return len(events), nil
}
