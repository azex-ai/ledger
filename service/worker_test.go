package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"

	"github.com/azex-ai/ledger/core"
)

func TestWorker_StartsAndStops(t *testing.T) {
	engine := core.NewEngine()

	// Minimal mocks — services won't do real work
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
		&mockExpiredDepositFinder{},
		&mockDepositExpirer{},
		&mockExpiredWithdrawalFinder{},
		&mockWithdrawalFailer{},
		engine,
	)
	reconcileSvc := NewReconciliationService(
		&mockGlobalSummer{debit: decimal.Zero, credit: decimal.Zero},
		&mockAccountEntrySummer{},
		&mockCheckpointReader{},
		&mockClassificationLister{},
		engine,
	)
	snapshotSvc := NewSnapshotService(
		&mockCheckpointLister{},
		&mockSnapshotWriter{},
		engine,
	)
	systemRollupSvc := NewSystemRollupService(
		&mockCheckpointAggregator{},
		&mockSystemRollupWriter{},
		engine,
	)

	config := WorkerConfig{
		RollupInterval:       10 * time.Millisecond,
		RollupBatchSize:      10,
		ExpirationInterval:   10 * time.Millisecond,
		ExpirationBatchSize:  10,
		ReconcileInterval:    10 * time.Millisecond,
		SnapshotInterval:     10 * time.Millisecond,
		SystemRollupInterval: 10 * time.Millisecond,
	}

	worker := NewWorker(rollupSvc, expirationSvc, reconcileSvc, snapshotSvc, systemRollupSvc, config, engine)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := worker.Run(ctx)
	assert.NoError(t, err)
}

func TestWorker_RollupRunsAtInterval(t *testing.T) {
	var callCount atomic.Int64

	engine := core.NewEngine()
	queue := &countingRollupQueuer{count: &callCount}

	rollupSvc := NewRollupService(
		queue,
		newMockCheckpointRW(),
		&mockEntrySummer{},
		&mockClassificationLister{},
		engine,
	)
	expirationSvc := NewExpirationService(
		&mockExpiredReservationFinder{},
		&mockReservationReleaser{},
		&mockExpiredDepositFinder{},
		&mockDepositExpirer{},
		&mockExpiredWithdrawalFinder{},
		&mockWithdrawalFailer{},
		engine,
	)
	reconcileSvc := NewReconciliationService(
		&mockGlobalSummer{debit: decimal.Zero, credit: decimal.Zero},
		&mockAccountEntrySummer{},
		&mockCheckpointReader{},
		&mockClassificationLister{},
		engine,
	)
	snapshotSvc := NewSnapshotService(
		&mockCheckpointLister{},
		&mockSnapshotWriter{},
		engine,
	)
	systemRollupSvc := NewSystemRollupService(
		&mockCheckpointAggregator{},
		&mockSystemRollupWriter{},
		engine,
	)

	config := WorkerConfig{
		RollupInterval:       10 * time.Millisecond,
		RollupBatchSize:      10,
		ExpirationInterval:   time.Hour, // won't fire
		ExpirationBatchSize:  10,
		ReconcileInterval:    time.Hour,
		SnapshotInterval:     time.Hour,
		SystemRollupInterval: time.Hour,
	}

	worker := NewWorker(rollupSvc, expirationSvc, reconcileSvc, snapshotSvc, systemRollupSvc, config, engine)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_ = worker.Run(ctx)

	// Should have been called multiple times
	assert.Greater(t, callCount.Load(), int64(1), "rollup should have been called multiple times")
}

// countingRollupQueuer counts DequeueRollupBatch calls.
type countingRollupQueuer struct {
	count *atomic.Int64
}

func (c *countingRollupQueuer) DequeueRollupBatch(_ context.Context, _ int) ([]core.RollupQueueItem, error) {
	c.count.Add(1)
	return nil, nil
}

func (c *countingRollupQueuer) MarkRollupProcessed(_ context.Context, _ int64) error { return nil }
func (c *countingRollupQueuer) CountPendingRollups(_ context.Context) (int64, error) { return 0, nil }
