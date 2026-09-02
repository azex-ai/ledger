package delivery

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// ctxRecordingPoller records whether each bookkeeping call arrived with a
// live context, and refuses the call when it did not — the same way a real
// store does (pgx returns ctx.Err() before touching the wire).
type ctxRecordingPoller struct {
	events []PendingEvent

	markDeliveredCalls int
	markRetryCalls     int
	lastErr            error
}

func (p *ctxRecordingPoller) GetPendingEvents(_ context.Context, _ int) ([]PendingEvent, error) {
	// Deliberately ignores ctx: the batch was already claimed in a real run,
	// and this test is about what happens to the bookkeeping afterwards.
	return p.events, nil
}

func (p *ctxRecordingPoller) MarkDelivered(ctx context.Context, _ int64, _ time.Time) error {
	if err := ctx.Err(); err != nil {
		p.lastErr = err
		return err
	}
	p.markDeliveredCalls++
	return nil
}

func (p *ctxRecordingPoller) MarkRetry(ctx context.Context, _ int64, _ time.Time, _ time.Time) error {
	if err := ctx.Err(); err != nil {
		p.lastErr = err
		return err
	}
	p.markRetryCalls++
	return nil
}

func (p *ctxRecordingPoller) MarkDead(ctx context.Context, _ int64, _ time.Time) error {
	if err := ctx.Err(); err != nil {
		p.lastErr = err
		return err
	}
	return nil
}

var _ EventPoller = (*ctxRecordingPoller)(nil)

func onePendingEvent() []PendingEvent {
	return []PendingEvent{{
		Event:      core.Event{UID: "11111111-1111-1111-1111-111111111111"},
		InternalID: 1,
		ClaimToken: time.Now(),
	}}
}

// TestLocalDispatcher_MarkDeliveredSurvivesCancelledParent pins B-m2: the
// handler already succeeded, so the outcome must be recorded even though the
// parent context was cancelled (worker shutdown mid-batch). Passing the
// cancelled ctx straight through -- what local.go used to do, against
// cleanup_context.go's own "use this at every release-on-the-way-out call
// site" rule -- lost the outcome and let the lease expire into a redelivery.
func TestLocalDispatcher_MarkDeliveredSurvivesCancelledParent(t *testing.T) {
	poller := &ctxRecordingPoller{events: onePendingEvent()}
	d := NewLocalDispatcher(poller, core.NopLogger())
	d.OnEvent(func(context.Context, core.Event) error { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	n, err := d.ProcessBatch(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 1, poller.markDeliveredCalls,
		"MarkDelivered must run on a detached context: the delivery already happened, and dropping its outcome turns a completed delivery into a redelivery (last ctx error: %v)", poller.lastErr)
}

// TestLocalDispatcher_MarkRetrySurvivesCancelledParent is the failure-path
// half: a handler error during shutdown must still be recorded as a retry
// schedule rather than left to the lease.
func TestLocalDispatcher_MarkRetrySurvivesCancelledParent(t *testing.T) {
	poller := &ctxRecordingPoller{events: onePendingEvent()}
	d := NewLocalDispatcher(poller, core.NopLogger())
	d.OnEvent(func(context.Context, core.Event) error { return errors.New("handler failed") })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	n, err := d.ProcessBatch(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 1, poller.markRetryCalls,
		"MarkRetry must run on a detached context (last ctx error: %v)", poller.lastErr)
}
