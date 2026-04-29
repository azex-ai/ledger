package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/service/delivery"
)

// ---------------------------------------------------------------------------
// Fake EventPoller for in-process subscription tests
// ---------------------------------------------------------------------------

// fakeEventPoller hands out a fixed batch of events once, then returns empty.
type fakeEventPoller struct {
	events    []core.Event
	delivered []int64
}

func (f *fakeEventPoller) GetPendingEvents(_ context.Context, limit int) ([]core.Event, error) {
	if len(f.events) == 0 {
		return nil, nil
	}
	n := limit
	if n > len(f.events) {
		n = len(f.events)
	}
	batch := f.events[:n]
	f.events = f.events[n:]
	return batch, nil
}

func (f *fakeEventPoller) MarkDelivered(_ context.Context, id int64) error {
	f.delivered = append(f.delivered, id)
	return nil
}

func (f *fakeEventPoller) MarkRetry(_ context.Context, _ int64, _ time.Time) error { return nil }
func (f *fakeEventPoller) MarkDead(_ context.Context, _ int64) error               { return nil }

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestWorker_Subscribe_HandlerReceivesEvent verifies that a handler registered
// via Worker.Subscribe is invoked when the worker's event_callback loop fires.
func TestWorker_Subscribe_HandlerReceivesEvent(t *testing.T) {
	engine := core.NewEngine()

	poller := &fakeEventPoller{
		events: []core.Event{
			{ID: 1, ClassificationCode: "deposit", BookingID: 10, ToStatus: "confirmed"},
		},
	}

	var received atomic.Int64

	// Build a minimal worker.
	worker := newMinimalWorker(engine)
	worker.SetLocalPoller(poller)
	worker.Subscribe(func(_ context.Context, e core.Event) error {
		received.Store(e.ID)
		return nil
	})

	// Give event_callback one tick — use a very short interval.
	worker.config.EventDeliveryInterval = 5 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := worker.Run(ctx)
	require.NoError(t, err)

	// Handler must have been invoked with event ID 1.
	assert.Equal(t, int64(1), received.Load(), "expected handler to receive event ID 1")
	// Event must have been marked delivered.
	assert.Equal(t, []int64{1}, poller.delivered)
}

// TestWorker_Subscribe_HandlerErrorDoesNotBlockQueue verifies that when a
// handler returns an error the event is still marked delivered and subsequent
// events are processed.
func TestWorker_Subscribe_HandlerErrorDoesNotBlockQueue(t *testing.T) {
	engine := core.NewEngine()

	poller := &fakeEventPoller{
		events: []core.Event{
			{ID: 1, ClassificationCode: "deposit", BookingID: 10, ToStatus: "confirmed"},
			{ID: 2, ClassificationCode: "deposit", BookingID: 11, ToStatus: "confirmed"},
		},
	}

	var processedCount atomic.Int64

	worker := newMinimalWorker(engine)
	worker.SetLocalPoller(poller)
	worker.Subscribe(func(_ context.Context, e core.Event) error {
		processedCount.Add(1)
		if e.ID == 1 {
			return assert.AnError // handler fails for event 1
		}
		return nil
	})
	worker.config.EventDeliveryInterval = 5 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	err := worker.Run(ctx)
	require.NoError(t, err)

	// Both events should have been processed despite the error on event 1.
	assert.Equal(t, int64(2), processedCount.Load())
	assert.Equal(t, []int64{1, 2}, poller.delivered)
}

// TestLocalDispatcher_ProcessBatch_NilPollerError verifies that ProcessBatch
// returns an error when no poller has been configured.
func TestLocalDispatcher_ProcessBatch_NilPollerError(t *testing.T) {
	d := delivery.NewLocalDispatcher(nil, core.NewEngine().Logger())
	_, err := d.ProcessBatch(context.Background(), 10)
	require.Error(t, err, "expected error when poller is nil")
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// newMinimalWorker builds a Worker backed by stub services suitable for
// testing only the event_callback path.
func newMinimalWorker(engine *core.Engine) *Worker {
	rollupSvc := NewRollupService(
		&mockRollupQueuer{},
		newMockCheckpointRW(),
		&mockEntrySummer{},
		&mockClassificationLister{},
		engine,
	)
	expirationSvc := NewExpirationService(
		&mockExpiredReservationFinder{},
		&mockReservationReleaser{},
		nil, nil,
		engine,
	)
	reconcileSvc := NewReconciliationService(
		&mockGlobalSummer{},
		&mockAccountEntrySummer{},
		&mockCheckpointReader{},
		&mockClassificationLister{},
		engine,
	)
	snapshotSvc := NewSnapshotService(
		&mockHistoricalBalanceLister{},
		&mockSnapshotWriter{},
		engine,
	)
	systemRollupSvc := NewSystemRollupService(
		&mockCheckpointAggregator{},
		&mockSystemRollupWriter{},
		engine,
	)

	config := WorkerConfig{
		RollupInterval:         time.Hour, // won't fire
		RollupBatchSize:        10,
		ExpirationInterval:     time.Hour,
		ExpirationBatchSize:    10,
		ReconcileInterval:      time.Hour,
		SnapshotInterval:       time.Hour,
		SystemRollupInterval:   time.Hour,
		EventDeliveryInterval:  time.Hour, // webhook path disabled
		EventDeliveryBatchSize: 10,
	}

	return NewWorker(rollupSvc, expirationSvc, reconcileSvc, snapshotSvc, systemRollupSvc, config, engine)
}
