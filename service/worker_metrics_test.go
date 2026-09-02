package service

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// jobMetricsRecorder records every JobTick*/JobPanicked call, keyed by job
// name (I-M9/I-M10's pin). Embeds core.NoopMetrics by value (not the
// core.Metrics interface) so every method this test does not care about is
// a safe no-op rather than a nil-interface panic.
type jobMetricsRecorder struct {
	core.NoopMetrics
	mu        sync.Mutex
	completed map[string]int
	failed    map[string]int
	skipped   map[string]int
	panicked  map[string]int
}

func newJobMetricsRecorder() *jobMetricsRecorder {
	return &jobMetricsRecorder{
		completed: map[string]int{},
		failed:    map[string]int{},
		skipped:   map[string]int{},
		panicked:  map[string]int{},
	}
}

func (m *jobMetricsRecorder) JobTickCompleted(job string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.completed[job]++
}

func (m *jobMetricsRecorder) JobTickFailed(job string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failed[job]++
}

func (m *jobMetricsRecorder) JobTickSkippedLocked(job string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.skipped[job]++
}

func (m *jobMetricsRecorder) JobPanicked(job string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.panicked[job]++
}

func (m *jobMetricsRecorder) count(bucket map[string]int, job string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return bucket[job]
}

// newRollupControlledWorker builds a Worker with every dependency stubbed except the
// rollup queue, which the caller supplies -- the smallest wiring that lets a
// test control exactly one job's fn while every other job is configured to
// never fire (interval = 1h) within the test's short-lived ctx.
func newRollupControlledWorker(t *testing.T, engine *core.Engine, queue RollupQueuer) *Worker {
	t.Helper()
	rollupSvc := NewRollupService(queue, newMockCheckpointRW(), &mockEntrySummer{}, &mockClassificationLister{}, engine)
	expirationSvc := NewExpirationService(&mockExpiredReservationFinder{}, &mockReservationReleaser{}, nil, nil, nil, engine)
	reconcileSvc := NewReconciliationService(
		&mockGlobalSummer{totals: []CurrencyReconcileTotals{{CurrencyID: 1, Debit: decimal.Zero, Credit: decimal.Zero}}},
		&mockAccountEntrySummer{}, &mockCheckpointReader{}, &mockClassificationLister{}, engine,
	)
	snapshotSvc := NewSnapshotService(&mockHistoricalBalanceLister{}, &mockSnapshotWriter{}, engine)
	systemRollupSvc := NewSystemRollupService(&mockCheckpointAggregator{}, &mockSystemRollupWriter{}, engine)

	config := WorkerConfig{
		RollupInterval:       10 * time.Millisecond,
		RollupBatchSize:      10,
		ExpirationInterval:   time.Hour,
		ExpirationBatchSize:  10,
		ReconcileInterval:    time.Hour,
		SnapshotInterval:     time.Hour,
		SystemRollupInterval: time.Hour,
	}
	w := NewWorker(rollupSvc, expirationSvc, reconcileSvc, snapshotSvc, systemRollupSvc, config, engine)
	w.AllowSilent()
	return w
}

// panicOnFirstCallQueuer panics from DequeueRollupBatch, every call --
// standing in for a job function bug reaching all the way to Run/errgroup
// before this fix.
type panicOnFirstCallQueuer struct{ mockRollupQueuer }

func (q *panicOnFirstCallQueuer) DequeueRollupBatch(context.Context, int) ([]RollupQueueItem, error) {
	panic("simulated job bug")
}

// TestWorker_JobPanic_DoesNotCrashProcess pins I-M9: before this fix, a
// panicking job function propagated straight through errgroup.Wait and out
// of Run, taking down whatever process called `go worker.Run(ctx)` with it.
// This test itself is the regression signal -- without safeRun's recover,
// the panic reaches testing's own panic handler and this test process dies
// instead of failing cleanly.
func TestWorker_JobPanic_DoesNotCrashProcess(t *testing.T) {
	metrics := newJobMetricsRecorder()
	engine := core.NewEngine(core.WithMetrics(metrics))
	worker := newRollupControlledWorker(t, engine, &panicOnFirstCallQueuer{})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	err := worker.Run(ctx)
	require.NoError(t, err, "Run must return cleanly (via ctx cancellation), not propagate the job's panic")

	assert.Greater(t, metrics.count(metrics.panicked, "rollup"), 0, "JobPanicked(\"rollup\") must be emitted at least once")
}

// TestWorker_JobTick_CompletedAndFailedAccounting pins I-M10: the rollup
// job's tick accounting (JobTickCompleted/JobTickFailed) is emitted inline
// by its own closure in worker.go, one per tick, not double-counted or
// dropped.
func TestWorker_JobTick_CompletedAndFailedAccounting(t *testing.T) {
	metrics := newJobMetricsRecorder()
	engine := core.NewEngine(core.WithMetrics(metrics))

	t.Run("success", func(t *testing.T) {
		worker := newRollupControlledWorker(t, engine, &mockRollupQueuer{})
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
		defer cancel()
		require.NoError(t, worker.Run(ctx))
		assert.Greater(t, metrics.count(metrics.completed, "rollup"), 0)
		assert.Equal(t, 0, metrics.count(metrics.failed, "rollup"))
	})
}

// erroringQueuer fails ClassificationDims-triggered lookups via a queue that
// errors on dequeue, forcing RollupService.ProcessBatch to return an error
// every tick.
type erroringRollupQueuer struct{ mockRollupQueuer }

func (q *erroringRollupQueuer) DequeueRollupBatch(context.Context, int) ([]RollupQueueItem, error) {
	return nil, fmt.Errorf("simulated dequeue failure")
}

func TestWorker_JobTick_FailedAccounting(t *testing.T) {
	metrics := newJobMetricsRecorder()
	engine := core.NewEngine(core.WithMetrics(metrics))
	worker := newRollupControlledWorker(t, engine, &erroringRollupQueuer{})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	require.NoError(t, worker.Run(ctx))

	assert.Greater(t, metrics.count(metrics.failed, "rollup"), 0, "JobTickFailed(\"rollup\") must be emitted when ProcessBatch errors")
	assert.Equal(t, 0, metrics.count(metrics.completed, "rollup"))
}

// TestLockedJob_MetricsAccounting pins I-M10's LockedJob half: exactly one
// of JobTickCompleted/JobTickFailed/JobTickSkippedLocked per Run call.
func TestLockedJob_MetricsAccounting(t *testing.T) {
	metrics := newJobMetricsRecorder()

	t.Run("completed", func(t *testing.T) {
		lj := NewLockedJob("t", func(context.Context) error { return nil }, nil, core.NopLogger(), metrics)
		require.NoError(t, lj.Run(context.Background()))
		assert.Equal(t, 1, metrics.count(metrics.completed, "t"))
		assert.Equal(t, 0, metrics.count(metrics.failed, "t"))
		assert.Equal(t, 0, metrics.count(metrics.skipped, "t"))
	})

	t.Run("failed", func(t *testing.T) {
		lj := NewLockedJob("u", func(context.Context) error { return fmt.Errorf("boom") }, nil, core.NopLogger(), metrics)
		require.Error(t, lj.Run(context.Background()))
		assert.Equal(t, 0, metrics.count(metrics.completed, "u"))
		assert.Equal(t, 1, metrics.count(metrics.failed, "u"))
	})

	t.Run("skipped_locked", func(t *testing.T) {
		lj := &LockedJob{
			name:    "v",
			lockKey: advisoryLockKey("job:v"),
			fn:      func(context.Context) error { return nil },
			locker:  &neverAcquireLock{},
			logger:  core.NopLogger(),
			metrics: metrics,
		}
		require.NoError(t, lj.Run(context.Background()))
		assert.Equal(t, 1, metrics.count(metrics.skipped, "v"))
		assert.Equal(t, 0, metrics.count(metrics.completed, "v"))
	})
}
