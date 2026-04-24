package service

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// --- Mock implementations ---

type mockRollupQueuer struct {
	items     []core.RollupQueueItem
	processed []int64
	released  []int64
	pending   int64
}

func (m *mockRollupQueuer) DequeueRollupBatch(_ context.Context, batchSize int) ([]core.RollupQueueItem, error) {
	if batchSize > len(m.items) {
		batchSize = len(m.items)
	}
	result := m.items[:batchSize]
	m.items = m.items[batchSize:]
	return result, nil
}

func (m *mockRollupQueuer) MarkRollupProcessed(_ context.Context, id int64) error {
	m.processed = append(m.processed, id)
	return nil
}

func (m *mockRollupQueuer) ReleaseRollupClaim(_ context.Context, id int64) error {
	m.released = append(m.released, id)
	return nil
}

func (m *mockRollupQueuer) CountPendingRollups(_ context.Context) (int64, error) {
	return m.pending, nil
}

type mockCheckpointRW struct {
	checkpoints map[checkpointKey]*core.BalanceCheckpoint
}

type checkpointKey struct {
	holder, currencyID, classificationID int64
}

func newMockCheckpointRW() *mockCheckpointRW {
	return &mockCheckpointRW{
		checkpoints: make(map[checkpointKey]*core.BalanceCheckpoint),
	}
}

func (m *mockCheckpointRW) GetCheckpoint(_ context.Context, holder, currencyID, classificationID int64) (*core.BalanceCheckpoint, error) {
	cp := m.checkpoints[checkpointKey{holder, currencyID, classificationID}]
	return cp, nil
}

func (m *mockCheckpointRW) UpsertCheckpoint(_ context.Context, cp core.BalanceCheckpoint) error {
	m.checkpoints[checkpointKey{cp.AccountHolder, cp.CurrencyID, cp.ClassificationID}] = &cp
	return nil
}

type mockEntrySummer struct {
	debitByClass  map[int64]decimal.Decimal
	creditByClass map[int64]decimal.Decimal
	maxEntryID    int64
	maxEntryAt    time.Time
	err           error
}

func (m *mockEntrySummer) SumEntriesSince(_ context.Context, _, _, _ int64) (map[int64]decimal.Decimal, map[int64]decimal.Decimal, int64, time.Time, error) {
	return m.debitByClass, m.creditByClass, m.maxEntryID, m.maxEntryAt, m.err
}

type mockClassificationLister struct {
	classifications []core.Classification
}

func (m *mockClassificationLister) ListClassifications(_ context.Context, _ bool) ([]core.Classification, error) {
	return m.classifications, nil
}

// --- Tests ---

func TestRollupService_ProcessSingleItem(t *testing.T) {
	queue := &mockRollupQueuer{
		items: []core.RollupQueueItem{
			{ID: 1, AccountHolder: 100, CurrencyID: 1, ClassificationID: 10},
		},
	}
	cpRW := newMockCheckpointRW()
	now := time.Now()
	entries := &mockEntrySummer{
		debitByClass:  map[int64]decimal.Decimal{10: decimal.NewFromInt(500)},
		creditByClass: map[int64]decimal.Decimal{10: decimal.NewFromInt(200)},
		maxEntryID:    42,
		maxEntryAt:    now,
	}
	cls := &mockClassificationLister{
		classifications: []core.Classification{
			{ID: 10, Code: "asset", NormalSide: core.NormalSideDebit},
		},
	}

	engine := core.NewEngine()
	svc := NewRollupService(queue, cpRW, entries, cls, engine)

	processed, err := svc.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)

	// Checkpoint should be updated: debit-normal => 500 - 200 = 300
	cp := cpRW.checkpoints[checkpointKey{100, 1, 10}]
	require.NotNil(t, cp)
	assert.True(t, cp.Balance.Equal(decimal.NewFromInt(300)))
	assert.Equal(t, int64(42), cp.LastEntryID)

	// Item should be marked processed
	assert.Equal(t, []int64{1}, queue.processed)
}

func TestRollupService_CreditNormalBalance(t *testing.T) {
	queue := &mockRollupQueuer{
		items: []core.RollupQueueItem{
			{ID: 2, AccountHolder: 200, CurrencyID: 1, ClassificationID: 20},
		},
	}
	cpRW := newMockCheckpointRW()
	// Pre-existing checkpoint with balance 100
	cpRW.checkpoints[checkpointKey{200, 1, 20}] = &core.BalanceCheckpoint{
		AccountHolder:    200,
		CurrencyID:       1,
		ClassificationID: 20,
		Balance:          decimal.NewFromInt(100),
		LastEntryID:      10,
		UpdatedAt:        time.Now().Add(-time.Hour),
	}

	now := time.Now()
	entries := &mockEntrySummer{
		debitByClass:  map[int64]decimal.Decimal{20: decimal.NewFromInt(50)},
		creditByClass: map[int64]decimal.Decimal{20: decimal.NewFromInt(150)},
		maxEntryID:    20,
		maxEntryAt:    now,
	}
	cls := &mockClassificationLister{
		classifications: []core.Classification{
			{ID: 20, Code: "liability", NormalSide: core.NormalSideCredit},
		},
	}

	engine := core.NewEngine()
	svc := NewRollupService(queue, cpRW, entries, cls, engine)

	processed, err := svc.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)

	// Credit-normal: delta = credit - debit = 150 - 50 = 100
	// New balance = 100 + 100 = 200
	cp := cpRW.checkpoints[checkpointKey{200, 1, 20}]
	require.NotNil(t, cp)
	assert.True(t, cp.Balance.Equal(decimal.NewFromInt(200)))
}

func TestRollupService_EmptyQueue(t *testing.T) {
	queue := &mockRollupQueuer{items: nil}
	cpRW := newMockCheckpointRW()
	entries := &mockEntrySummer{}
	cls := &mockClassificationLister{}
	engine := core.NewEngine()
	svc := NewRollupService(queue, cpRW, entries, cls, engine)

	processed, err := svc.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 0, processed)
}

func TestRollupService_DriftDetection(t *testing.T) {
	queue := &mockRollupQueuer{
		items: []core.RollupQueueItem{
			{ID: 3, AccountHolder: 300, CurrencyID: 1, ClassificationID: 30},
		},
	}
	cpRW := newMockCheckpointRW()
	cpRW.checkpoints[checkpointKey{300, 1, 30}] = &core.BalanceCheckpoint{
		AccountHolder:    300,
		CurrencyID:       1,
		ClassificationID: 30,
		Balance:          decimal.NewFromInt(10),
		LastEntryID:      5,
		UpdatedAt:        time.Now().Add(-time.Hour),
	}

	entries := &mockEntrySummer{
		debitByClass:  map[int64]decimal.Decimal{30: decimal.NewFromInt(5)},
		creditByClass: map[int64]decimal.Decimal{30: decimal.NewFromInt(100)},
		maxEntryID:    15,
		maxEntryAt:    time.Now(),
	}
	cls := &mockClassificationLister{
		classifications: []core.Classification{
			{ID: 30, Code: "asset", NormalSide: core.NormalSideDebit},
		},
	}

	// Use a recording metrics to verify drift is emitted
	metrics := &recordingMetrics{}
	engine := core.NewEngine(core.WithMetrics(metrics))
	svc := NewRollupService(queue, cpRW, entries, cls, engine)

	processed, err := svc.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)

	// Balance = 10 + (5 - 100) = -85 (negative on debit-normal = drift)
	assert.True(t, metrics.balanceDriftCalled)
}

func TestRollupService_ReleasesClaimOnProcessError(t *testing.T) {
	queue := &mockRollupQueuer{
		items: []core.RollupQueueItem{
			{ID: 4, AccountHolder: 400, CurrencyID: 1, ClassificationID: 40},
		},
	}
	cpRW := newMockCheckpointRW()
	entries := &mockEntrySummer{
		err: assert.AnError,
	}
	cls := &mockClassificationLister{
		classifications: []core.Classification{
			{ID: 40, Code: "asset", NormalSide: core.NormalSideDebit},
		},
	}

	engine := core.NewEngine()
	svc := NewRollupService(queue, cpRW, entries, cls, engine)

	processed, err := svc.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	assert.Zero(t, processed)
	assert.Equal(t, []int64{4}, queue.released)
}

// recordingMetrics captures specific metric calls for testing.
type recordingMetrics struct {
	core.Metrics
	balanceDriftCalled bool
	rollupProcessed    int
}

func (m *recordingMetrics) JournalPosted(string)                  {}
func (m *recordingMetrics) JournalFailed(string, string)          {}
func (m *recordingMetrics) ReserveCreated()                       {}
func (m *recordingMetrics) ReserveSettled()                       {}
func (m *recordingMetrics) ReserveReleased()                      {}
func (m *recordingMetrics) ReconcileCompleted(bool)               {}
func (m *recordingMetrics) IdempotencyCollision(string)           {}
func (m *recordingMetrics) TemplateFailed(string, string)         {}
func (m *recordingMetrics) DepositConfirmed(string)               {}
func (m *recordingMetrics) WithdrawConfirmed(string)              {}
func (m *recordingMetrics) JournalLatency(time.Duration)          {}
func (m *recordingMetrics) SnapshotLatency(time.Duration)         {}
func (m *recordingMetrics) JournalEntryCount(string, int)         {}
func (m *recordingMetrics) PendingRollups(int64)                  {}
func (m *recordingMetrics) ActiveReservations(int64)              {}
func (m *recordingMetrics) CheckpointAge(string, time.Duration)   {}
func (m *recordingMetrics) ReconcileGap(int64, decimal.Decimal)   {}
func (m *recordingMetrics) ReservedAmount(int64, decimal.Decimal) {}
func (m *recordingMetrics) RollupProcessed(count int)             { m.rollupProcessed += count }
func (m *recordingMetrics) RollupLatency(time.Duration)           {}
func (m *recordingMetrics) BalanceDrift(_ string, _ int64, _ decimal.Decimal) {
	m.balanceDriftCalled = true
}
