package delivery

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// TestLocalDispatcher_ProcessBatch_SuccessEmitsDeliveredMetric pins I-N12:
// library-mode (Worker.Subscribe) event delivery must emit the same
// EventDelivered/EventDeliveryFailed/EventDead signals as WebhookDeliverer --
// CAPACITY.md's event-delivery SLO is written against those counters
// regardless of which deliverer produced them.
func TestLocalDispatcher_ProcessBatch_SuccessEmitsDeliveredMetric(t *testing.T) {
	poller := &mockEventPoller{
		events: []PendingEvent{{InternalID: 1, Event: core.Event{UID: "evt-1"}}},
	}
	metrics := &recordingMetrics{}
	d := NewLocalDispatcher(poller, core.NopLogger(), metrics)
	d.OnEvent(func(context.Context, core.Event) error { return nil })

	n, err := d.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []int64{1}, poller.delivered)
	assert.Equal(t, 1, metrics.delivered)
	assert.Equal(t, 0, metrics.deliveryFailed)
	assert.Equal(t, 0, metrics.dead)
}

func TestLocalDispatcher_ProcessBatch_HandlerErrorEmitsFailedMetric(t *testing.T) {
	poller := &mockEventPoller{
		events: []PendingEvent{{InternalID: 2, Event: core.Event{UID: "evt-2", Attempts: 0, MaxAttempts: 10}}},
	}
	metrics := &recordingMetrics{}
	d := NewLocalDispatcher(poller, core.NopLogger(), metrics)
	d.OnEvent(func(context.Context, core.Event) error { return assertError })

	_, err := d.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, []int64{2}, poller.retried)
	assert.Equal(t, 1, metrics.deliveryFailed)
	assert.Equal(t, 0, metrics.dead, "not yet at max attempts")
}

func TestLocalDispatcher_ProcessBatch_LastAttemptEmitsDeadMetric(t *testing.T) {
	poller := &mockEventPoller{
		events: []PendingEvent{{InternalID: 3, Event: core.Event{UID: "evt-3", Attempts: 9, MaxAttempts: 10}}},
	}
	metrics := &recordingMetrics{}
	d := NewLocalDispatcher(poller, core.NopLogger(), metrics)
	d.OnEvent(func(context.Context, core.Event) error { return assertError })

	_, err := d.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, metrics.deliveryFailed)
	assert.Equal(t, 1, metrics.dead)
}

// TestLocalDispatcher_ProcessBatch_HandlerPanicIsRecovered pins I-M9: a
// consumer-supplied Subscribe handler panicking must not crash the process --
// it is converted into an ordinary handler error (scheduled for retry) like
// any other, exactly as a webhook handler bug only ever surfaces as an HTTP
// 500 rather than taking the process down.
func TestLocalDispatcher_ProcessBatch_HandlerPanicIsRecovered(t *testing.T) {
	poller := &mockEventPoller{
		events: []PendingEvent{{InternalID: 4, Event: core.Event{UID: "evt-4", MaxAttempts: 10}}},
	}
	metrics := &recordingMetrics{}
	d := NewLocalDispatcher(poller, core.NopLogger(), metrics)
	d.OnEvent(func(context.Context, core.Event) error { panic("boom") })

	assert.NotPanics(t, func() {
		_, err := d.ProcessBatch(context.Background(), 10)
		require.NoError(t, err)
	})
	assert.Equal(t, []int64{4}, poller.retried, "a panicking handler must be treated as a failure, not silently dropped")
	assert.Equal(t, 1, metrics.deliveryFailed)
}

func TestLocalDispatcher_ProcessBatch_NilMetricsDefaultsToNop(t *testing.T) {
	poller := &mockEventPoller{
		events: []PendingEvent{{InternalID: 5, Event: core.Event{UID: "evt-5"}}},
	}
	d := NewLocalDispatcher(poller, core.NopLogger(), nil)
	d.OnEvent(func(context.Context, core.Event) error { return nil })

	assert.NotPanics(t, func() {
		_, err := d.ProcessBatch(context.Background(), 10)
		require.NoError(t, err)
	})
}

var assertError = &staticError{"handler failed"}

type staticError struct{ msg string }

func (e *staticError) Error() string { return e.msg }
