package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// --- Mock implementations ---

type mockRollupQueuer struct {
	items             []RollupQueueItem
	processed         []int64
	released          []int64
	enqueued          []RollupQueueItem
	pending           int64
	stuck             int64
	enqueueErr        error // when set, EnqueueRollup returns it (after recording the call)
	releaseErr        error // when set, ReleaseRollupClaim returns it (after recording the call)
	lastReleaseCtxErr error // ctx.Err() observed by the most recent ReleaseRollupClaim call
}

func (m *mockRollupQueuer) DequeueRollupBatch(_ context.Context, batchSize int) ([]RollupQueueItem, error) {
	if batchSize > len(m.items) {
		batchSize = len(m.items)
	}
	result := m.items[:batchSize]
	m.items = m.items[batchSize:]
	return result, nil
}

func (m *mockRollupQueuer) MarkRollupProcessed(_ context.Context, id int64, _ time.Time) (bool, error) {
	m.processed = append(m.processed, id)
	return true, nil
}

func (m *mockRollupQueuer) ReleaseRollupClaim(ctx context.Context, id int64, _ time.Time) error {
	m.released = append(m.released, id)
	m.lastReleaseCtxErr = ctx.Err()
	if m.releaseErr != nil {
		return m.releaseErr
	}
	return nil
}

func (m *mockRollupQueuer) CountPendingRollups(_ context.Context) (int64, error) {
	return m.pending, nil
}

func (m *mockRollupQueuer) CountStuckRollups(_ context.Context) (int64, error) {
	return m.stuck, nil
}

func (m *mockRollupQueuer) EnqueueRollup(_ context.Context, holder, currencyID, classificationID int64) error {
	m.enqueued = append(m.enqueued, RollupQueueItem{
		AccountHolder:    holder,
		CurrencyID:       currencyID,
		ClassificationID: classificationID,
	})
	return m.enqueueErr
}

type mockCheckpointRW struct {
	checkpoints map[checkpointKey]*BalanceCheckpoint
}

type checkpointKey struct {
	holder, currencyID, classificationID int64
}

func newMockCheckpointRW() *mockCheckpointRW {
	return &mockCheckpointRW{
		checkpoints: make(map[checkpointKey]*BalanceCheckpoint),
	}
}

func (m *mockCheckpointRW) GetCheckpoint(_ context.Context, holder, currencyID, classificationID int64) (*BalanceCheckpoint, error) {
	cp := m.checkpoints[checkpointKey{holder, currencyID, classificationID}]
	return cp, nil
}

func (m *mockCheckpointRW) UpsertCheckpoint(_ context.Context, cp BalanceCheckpoint) error {
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

// sinceAwareEntrySummer returns a different result depending on sinceEntryID, so
// tests can model entries that arrive AFTER the first rollup snapshot (i.e. the
// post-processing re-check in processItem reads a different "since" than the
// initial sum). Used to pin the coalesced-enqueue recovery (Q4).
type sinceAwareEntrySummer struct {
	bySince map[int64]struct {
		debit  map[int64]decimal.Decimal
		credit map[int64]decimal.Decimal
		maxID  int64
	}
	calls []int64 // records each `since` argument, in call order
}

func (s *sinceAwareEntrySummer) SumEntriesSince(_ context.Context, _, _, since int64) (map[int64]decimal.Decimal, map[int64]decimal.Decimal, int64, time.Time, error) {
	s.calls = append(s.calls, since)
	r := s.bySince[since]
	return r.debit, r.credit, r.maxID, time.Time{}, nil
}

type mockClassificationLister struct {
	classifications []ClassificationDim
}

func (m *mockClassificationLister) ClassificationDims(_ context.Context) ([]ClassificationDim, error) {
	return m.classifications, nil
}

// CurrencyIDByUID parses the test convention "cur-<id>" back to the id.
func (m *mockClassificationLister) CurrencyIDByUID(_ context.Context, uid string) (int64, error) {
	var id int64
	if _, err := fmt.Sscanf(uid, "cur-%d", &id); err != nil {
		return 0, core.ErrNotFound
	}
	return id, nil
}

func (m *mockClassificationLister) CurrencyUIDByID(_ context.Context, id int64) (string, error) {
	return fmt.Sprintf("cur-%d", id), nil
}

// --- Tests ---

func TestRollupService_ProcessSingleItem(t *testing.T) {
	queue := &mockRollupQueuer{
		items: []RollupQueueItem{
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
		classifications: []ClassificationDim{
			{ID: 10, UID: "cls-10", Code: "asset", NormalSide: core.NormalSideDebit},
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
		items: []RollupQueueItem{
			{ID: 2, AccountHolder: 200, CurrencyID: 1, ClassificationID: 20},
		},
	}
	cpRW := newMockCheckpointRW()
	// Pre-existing checkpoint with balance 100
	cpRW.checkpoints[checkpointKey{200, 1, 20}] = &BalanceCheckpoint{
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
		classifications: []ClassificationDim{
			{ID: 20, UID: "cls-20", Code: "liability", NormalSide: core.NormalSideCredit},
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

// TestRollupService_ReenqueuesCoalescedEntries pins the Q4 fix: when a journal
// for the dimension lands after the rollup snapshot (so its EnqueueRollup was
// coalesced away by the still-pending queue row), processItem must re-enqueue
// the dimension after marking processed, so the checkpoint eventually catches
// up instead of lagging forever.
func TestRollupService_ReenqueuesCoalescedEntries(t *testing.T) {
	queue := &mockRollupQueuer{
		items: []RollupQueueItem{
			{ID: 1, AccountHolder: 100, CurrencyID: 1, ClassificationID: 10},
		},
	}
	cpRW := newMockCheckpointRW()
	entries := &sinceAwareEntrySummer{
		bySince: map[int64]struct {
			debit  map[int64]decimal.Decimal
			credit map[int64]decimal.Decimal
			maxID  int64
		}{
			// Initial sum (no checkpoint yet → since 0): class 10 up to entry 42.
			0: {debit: map[int64]decimal.Decimal{10: decimal.NewFromInt(500)}, credit: map[int64]decimal.Decimal{10: decimal.NewFromInt(200)}, maxID: 42},
			// Re-check (since = 42): a new class-10 entry arrived during processing.
			42: {debit: map[int64]decimal.Decimal{10: decimal.NewFromInt(30)}, credit: map[int64]decimal.Decimal{}, maxID: 50},
		},
	}
	cls := &mockClassificationLister{
		classifications: []ClassificationDim{{ID: 10, UID: "cls-10", Code: "asset", NormalSide: core.NormalSideDebit}},
	}

	svc := NewRollupService(queue, cpRW, entries, cls, core.NewEngine())
	processed, err := svc.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
	assert.Equal(t, []int64{1}, queue.processed)

	// The coalesced entry must have triggered a re-enqueue of the same dimension.
	require.Len(t, queue.enqueued, 1)
	assert.Equal(t, RollupQueueItem{AccountHolder: 100, CurrencyID: 1, ClassificationID: 10}, queue.enqueued[0])

	// Non-tautology guard: the re-check MUST read entries past the checkpoint we
	// just wrote (since = maxEntryID = 42), not re-read from the original since
	// (0). If processItem regressed to passing sinceEntryID, the re-enqueue
	// assertion above would still pass (class 10 is present under both snapshots),
	// so we pin the actual `since` arguments here.
	assert.Equal(t, []int64{0, 42}, entries.calls)
}

// TestRollupService_NoReenqueueWhenNothingNew pins the other half: when no new
// entries for the dimension exist past the checkpoint, processItem must NOT
// re-enqueue (otherwise a hot sibling classification would churn this one).
func TestRollupService_NoReenqueueWhenNothingNew(t *testing.T) {
	queue := &mockRollupQueuer{
		items: []RollupQueueItem{
			{ID: 1, AccountHolder: 100, CurrencyID: 1, ClassificationID: 10},
		},
	}
	cpRW := newMockCheckpointRW()
	entries := &sinceAwareEntrySummer{
		bySince: map[int64]struct {
			debit  map[int64]decimal.Decimal
			credit map[int64]decimal.Decimal
			maxID  int64
		}{
			0:  {debit: map[int64]decimal.Decimal{10: decimal.NewFromInt(500)}, credit: map[int64]decimal.Decimal{10: decimal.NewFromInt(200)}, maxID: 42},
			42: {debit: map[int64]decimal.Decimal{}, credit: map[int64]decimal.Decimal{}, maxID: 0},
		},
	}
	cls := &mockClassificationLister{
		classifications: []ClassificationDim{{ID: 10, UID: "cls-10", Code: "asset", NormalSide: core.NormalSideDebit}},
	}

	svc := NewRollupService(queue, cpRW, entries, cls, core.NewEngine())
	processed, err := svc.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
	assert.Empty(t, queue.enqueued)
}

// TestRollupService_ReenqueueFailureDoesNotFailRollup pins the best-effort
// contract of the coalesced-enqueue recovery: when the re-enqueue itself fails,
// the rollup must still report success. The checkpoint was already committed and
// balances stay correct via the delta path, so a failed re-enqueue only delays
// re-materialization until the next journal for this dimension — it must never
// turn a successful rollup into a failure (which would orphan the processed row).
func TestRollupService_ReenqueueFailureDoesNotFailRollup(t *testing.T) {
	queue := &mockRollupQueuer{
		items: []RollupQueueItem{
			{ID: 1, AccountHolder: 100, CurrencyID: 1, ClassificationID: 10},
		},
		enqueueErr: errors.New("db unavailable"),
	}
	cpRW := newMockCheckpointRW()
	entries := &sinceAwareEntrySummer{
		bySince: map[int64]struct {
			debit  map[int64]decimal.Decimal
			credit map[int64]decimal.Decimal
			maxID  int64
		}{
			0:  {debit: map[int64]decimal.Decimal{10: decimal.NewFromInt(500)}, credit: map[int64]decimal.Decimal{10: decimal.NewFromInt(200)}, maxID: 42},
			42: {debit: map[int64]decimal.Decimal{10: decimal.NewFromInt(30)}, credit: map[int64]decimal.Decimal{}, maxID: 50},
		},
	}
	cls := &mockClassificationLister{
		classifications: []ClassificationDim{{ID: 10, UID: "cls-10", Code: "asset", NormalSide: core.NormalSideDebit}},
	}

	svc := NewRollupService(queue, cpRW, entries, cls, core.NewEngine())
	processed, err := svc.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
	// Rollup succeeded: the item was marked processed and the checkpoint written,
	// even though the best-effort re-enqueue attempt errored.
	assert.Equal(t, []int64{1}, queue.processed)
	assert.Empty(t, queue.released)
	require.Len(t, queue.enqueued, 1) // the attempt was made (and failed)
	cp := cpRW.checkpoints[checkpointKey{holder: 100, currencyID: 1, classificationID: 10}]
	require.NotNil(t, cp)
	assert.Equal(t, int64(42), cp.LastEntryID)
}

func TestRollupService_DriftDetection(t *testing.T) {
	queue := &mockRollupQueuer{
		items: []RollupQueueItem{
			{ID: 3, AccountHolder: 300, CurrencyID: 1, ClassificationID: 30},
		},
	}
	cpRW := newMockCheckpointRW()
	cpRW.checkpoints[checkpointKey{300, 1, 30}] = &BalanceCheckpoint{
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
		classifications: []ClassificationDim{
			{ID: 30, UID: "cls-30", Code: "asset", NormalSide: core.NormalSideDebit},
		},
	}

	// Use a recording metrics to verify drift is emitted
	metrics := &recordingMetrics{}
	engine := core.NewEngine(core.WithMetrics(metrics))
	svc := NewRollupService(queue, cpRW, entries, cls, engine)

	processed, err := svc.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, processed)

	// Balance = 10 + (5 - 100) = -85 (negative on debit-normal = violation).
	// BalanceDrift's documented contract (core.Metrics) is "drift between
	// expected and actual balance": the expected floor for a debit-normal
	// account is 0, so the reported drift must be the positive magnitude of
	// the shortfall (85), NOT the signed balance itself (-85) -- passing the
	// balance verbatim was operability.md's Major finding (service/rollup.go
	// previously fed newBalance straight into a metric documented as "drift").
	require.True(t, metrics.balanceDriftCalled)
	require.Len(t, metrics.balanceDriftCalls, 1)
	assert.True(t, metrics.balanceDriftCalls[0].Equal(decimal.NewFromInt(85)),
		"want drift magnitude 85, got %s (a signed -85 here means the raw balance leaked through instead of a drift)",
		metrics.balanceDriftCalls[0])
}

// TestRollupService_DriftDetection_ClearsOnceHealthy pins the other half of
// the same fix: once a rollup for this item is no longer in violation, the
// gauge must report back to zero, not stay pinned at the last violation it
// ever observed. Before this fix, BalanceDrift was only ever called on the
// violation branch -- nothing in service/rollup.go ever set it back to zero,
// so a real Prometheus gauge would stay red on a dashboard forever after a
// single transient negative balance, even once fixed (working-agreements
// §3: a signal that cannot clear is indistinguishable from "still broken").
func TestRollupService_DriftDetection_ClearsOnceHealthy(t *testing.T) {
	queue := &mockRollupQueuer{
		items: []RollupQueueItem{
			{ID: 3, AccountHolder: 300, CurrencyID: 1, ClassificationID: 30},
		},
	}
	cpRW := newMockCheckpointRW()
	cpRW.checkpoints[checkpointKey{300, 1, 30}] = &BalanceCheckpoint{
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
		classifications: []ClassificationDim{
			{ID: 30, UID: "cls-30", Code: "asset", NormalSide: core.NormalSideDebit},
		},
	}

	metrics := &recordingMetrics{}
	engine := core.NewEngine(core.WithMetrics(metrics))
	svc := NewRollupService(queue, cpRW, entries, cls, engine)

	// First batch: balance goes to 10 + (5 - 100) = -85, a violation.
	processed, err := svc.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, 1, processed)
	require.Len(t, metrics.balanceDriftCalls, 1)
	require.False(t, metrics.balanceDriftCalls[0].IsZero(), "first rollup must report the violation")

	// Second batch: a large credit-side entry brings the balance back
	// positive. The checkpoint now reflects -85 + 200 = 115.
	queue.items = []RollupQueueItem{
		{ID: 6, AccountHolder: 300, CurrencyID: 1, ClassificationID: 30},
	}
	entries.debitByClass = map[int64]decimal.Decimal{30: decimal.NewFromInt(200)}
	entries.creditByClass = map[int64]decimal.Decimal{30: decimal.NewFromInt(0)}
	entries.maxEntryID = 25

	processed, err = svc.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, 1, processed)

	require.Len(t, metrics.balanceDriftCalls, 2, "the second, healthy rollup must still report a reading")
	assert.True(t, metrics.balanceDriftCalls[1].IsZero(),
		"the gauge must clear back to zero once the account is no longer in violation, got %s",
		metrics.balanceDriftCalls[1])
}

// TestRollupService_NegativeBalanceDetected_SurvivesUnrelatedHealthyItem pins
// the M-3 fix (I-41 point 3, `.local/independent-review-2026-08-26.md`):
// BalanceDrift's Gauge is labelled (class, currency) WITHOUT holder, so an
// unrelated healthy holder sharing that label legitimately re-Sets the same
// series back to zero right after a genuine violation was reported for a
// DIFFERENT holder -- that is reproduced directly below (holder 300 violates,
// holder 301 is healthy, both under classification 30/currency 1), and the
// Gauge really does read 0 afterward. Before this fix, that Gauge reading was
// the ONLY signal a violation had occurred, so this exact sequence made a
// real, still-open violation for holder 300 invisible to anything alerting
// on the Gauge. NegativeBalanceDetected is monotonic and must still show the
// violation happened, regardless of what the Gauge reads afterward.
func TestRollupService_NegativeBalanceDetected_SurvivesUnrelatedHealthyItem(t *testing.T) {
	cpRW := newMockCheckpointRW()
	cls := &mockClassificationLister{
		classifications: []ClassificationDim{
			{ID: 30, UID: "cls-30", Code: "asset", NormalSide: core.NormalSideDebit},
		},
	}
	metrics := &recordingMetrics{}
	engine := core.NewEngine(core.WithMetrics(metrics))

	// A checkpoint must already exist for a dimension before processItem
	// reports drift for it at all (cp != nil gate) -- seed both holders'
	// starting checkpoints, same as TestRollupService_DriftDetection does
	// for its single holder.
	cpRW.checkpoints[checkpointKey{300, 1, 30}] = &BalanceCheckpoint{
		AccountHolder: 300, CurrencyID: 1, ClassificationID: 30, Balance: decimal.Zero,
	}
	cpRW.checkpoints[checkpointKey{301, 1, 30}] = &BalanceCheckpoint{
		AccountHolder: 301, CurrencyID: 1, ClassificationID: 30, Balance: decimal.Zero,
	}

	// First batch: holder 300 goes to 0 + (0 - 50) = -50, a violation.
	queue := &mockRollupQueuer{
		items: []RollupQueueItem{
			{ID: 1, AccountHolder: 300, CurrencyID: 1, ClassificationID: 30},
		},
	}
	entries := &mockEntrySummer{
		debitByClass:  map[int64]decimal.Decimal{30: decimal.NewFromInt(0)},
		creditByClass: map[int64]decimal.Decimal{30: decimal.NewFromInt(50)},
		maxEntryID:    1,
		maxEntryAt:    time.Now(),
	}
	svc := NewRollupService(queue, cpRW, entries, cls, engine)
	processed, err := svc.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, 1, processed)
	require.Equal(t, 1, metrics.negativeBalanceDetectedCalls, "the violation must be counted")
	require.Len(t, metrics.balanceDriftCalls, 1)
	require.False(t, metrics.balanceDriftCalls[0].IsZero())

	// Second batch: a DIFFERENT holder (301), same classification/currency
	// label, healthy from the start -- nothing about holder 300 changed.
	queue2 := &mockRollupQueuer{
		items: []RollupQueueItem{
			{ID: 2, AccountHolder: 301, CurrencyID: 1, ClassificationID: 30},
		},
	}
	entries2 := &mockEntrySummer{
		debitByClass:  map[int64]decimal.Decimal{30: decimal.NewFromInt(100)},
		creditByClass: map[int64]decimal.Decimal{30: decimal.NewFromInt(0)},
		maxEntryID:    2,
		maxEntryAt:    time.Now(),
	}
	svc2 := NewRollupService(queue2, cpRW, entries2, cls, engine)
	processed2, err := svc2.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, 1, processed2)

	// Reproduces the exact shape M-3 flagged: the Gauge's second reading is
	// zero -- holder 301's healthy item shares holder 300's (class,
	// currency) label and legitimately re-Sets it. This assertion is not
	// the bug; it is the precondition the bug depended on.
	require.Len(t, metrics.balanceDriftCalls, 2)
	assert.True(t, metrics.balanceDriftCalls[1].IsZero(),
		"holder 301's healthy item shares holder 300's label and legitimately clears the Gauge -- this is expected, not the fix")

	// The fix: the monotonic counter must NOT have been reset by holder
	// 301's healthy item, and must still show exactly the one violation
	// holder 300 produced.
	assert.Equal(t, 1, metrics.negativeBalanceDetectedCalls,
		"NegativeBalanceDetected must still report holder 300's violation even though the Gauge for the same label already read back to zero")
}

func TestRollupService_ReleasesClaimOnProcessError(t *testing.T) {
	queue := &mockRollupQueuer{
		items: []RollupQueueItem{
			{ID: 4, AccountHolder: 400, CurrencyID: 1, ClassificationID: 40},
		},
	}
	cpRW := newMockCheckpointRW()
	entries := &mockEntrySummer{
		err: assert.AnError,
	}
	cls := &mockClassificationLister{
		classifications: []ClassificationDim{
			{ID: 40, UID: "cls-40", Code: "asset", NormalSide: core.NormalSideDebit},
		},
	}

	metrics := &recordingMetrics{}
	engine := core.NewEngine(core.WithMetrics(metrics))
	svc := NewRollupService(queue, cpRW, entries, cls, engine)

	processed, err := svc.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	assert.Zero(t, processed)
	assert.Equal(t, []int64{4}, queue.released)
	assert.Equal(t, 1, metrics.rollupItemFailed, "RollupItemFailed must be emitted when a claim is released after a failed processing attempt")
}

// TestRollupService_CountsFailureEvenWhenReleaseItselfFails pins I-M10's
// third bullet: RollupItemFailed used to live in ReleaseRollupClaim's
// success branch only, so the moment this signal matters most -- the
// release call ITSELF failing (DB contention) -- produced no metric at
// all. It must now fire regardless of whether the release succeeds.
func TestRollupService_CountsFailureEvenWhenReleaseItselfFails(t *testing.T) {
	queue := &mockRollupQueuer{
		items: []RollupQueueItem{
			{ID: 4, AccountHolder: 400, CurrencyID: 1, ClassificationID: 40},
		},
		releaseErr: assert.AnError,
	}
	cpRW := newMockCheckpointRW()
	entries := &mockEntrySummer{err: assert.AnError}
	cls := &mockClassificationLister{
		classifications: []ClassificationDim{
			{ID: 40, UID: "cls-40", Code: "asset", NormalSide: core.NormalSideDebit},
		},
	}

	metrics := &recordingMetrics{}
	engine := core.NewEngine(core.WithMetrics(metrics))
	svc := NewRollupService(queue, cpRW, entries, cls, engine)

	processed, err := svc.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	assert.Zero(t, processed)
	assert.Equal(t, 1, metrics.rollupItemFailed, "RollupItemFailed must still be emitted when ReleaseRollupClaim itself errors")
}

// TestRollupService_ReleasesClaimAfterParentCtxCancelled verifies that the
// claim-release cleanup path still runs (and succeeds) even when the parent
// ctx passed to ProcessBatch was already cancelled — e.g. worker shutdown
// racing a batch. Passing the cancelled ctx straight through to the release
// call would fail immediately (ctx.Err() != nil), leaking the claim until its
// lease expires; cleanupContext must detach from that cancellation.
func TestRollupService_ReleasesClaimAfterParentCtxCancelled(t *testing.T) {
	queue := &mockRollupQueuer{
		items: []RollupQueueItem{
			{ID: 5, AccountHolder: 500, CurrencyID: 1, ClassificationID: 50},
		},
	}
	cpRW := newMockCheckpointRW()
	entries := &mockEntrySummer{
		err: assert.AnError,
	}
	cls := &mockClassificationLister{
		classifications: []ClassificationDim{
			{ID: 50, UID: "cls-50", Code: "asset", NormalSide: core.NormalSideDebit},
		},
	}

	engine := core.NewEngine()
	svc := NewRollupService(queue, cpRW, entries, cls, engine)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate shutdown having already fired before this batch runs

	processed, err := svc.ProcessBatch(ctx, 10)
	require.NoError(t, err)
	assert.Zero(t, processed)
	require.Equal(t, []int64{5}, queue.released, "claim must still be released even though the parent ctx was cancelled")
	assert.NoError(t, queue.lastReleaseCtxErr, "release must run on a detached ctx, not the cancelled parent")
}

// recordingMetrics captures specific metric calls for testing.
type recordingMetrics struct {
	core.Metrics
	balanceDriftCalled           bool
	balanceDriftCalls            []decimal.Decimal // every value passed to BalanceDrift, in call order
	rollupProcessed              int
	rollupItemFailed             int
	negativeBalanceDetectedCalls int // count of NegativeBalanceDetected calls
	stuckRollups                 []int64 // every value passed to StuckRollups, in call order
	pendingRollups               []int64 // ditto for PendingRollups
}

func (m *recordingMetrics) JournalPosted(string)                   {}
func (m *recordingMetrics) JournalFailed(string, string)           {}
func (m *recordingMetrics) ReserveCreated()                        {}
func (m *recordingMetrics) ReserveSettled()                        {}
func (m *recordingMetrics) ReserveReleased()                       {}
func (m *recordingMetrics) ReconcileCompleted(bool)                {}
func (m *recordingMetrics) IdempotencyCollision(string)            {}
func (m *recordingMetrics) TemplateFailed(string, string)          {}
func (m *recordingMetrics) BookingTransitioned(string, string)     {}
func (m *recordingMetrics) JournalLatency(time.Duration)           {}
func (m *recordingMetrics) SnapshotLatency(time.Duration)          {}
func (m *recordingMetrics) JournalEntryCount(string, int)          {}
func (m *recordingMetrics) PendingRollups(n int64)                 { m.pendingRollups = append(m.pendingRollups, n) }
func (m *recordingMetrics) StuckRollups(n int64)                   { m.stuckRollups = append(m.stuckRollups, n) }
func (m *recordingMetrics) ActiveReservations(int64)               {}
func (m *recordingMetrics) CheckpointAge(string, time.Duration)    {}
func (m *recordingMetrics) ReconcileGap(string, decimal.Decimal)   {}
func (m *recordingMetrics) ReservedAmount(string, decimal.Decimal) {}
func (m *recordingMetrics) RollupProcessed(count int)              { m.rollupProcessed += count }
func (m *recordingMetrics) RollupItemFailed()                      { m.rollupItemFailed++ }
func (m *recordingMetrics) RollupLatency(time.Duration)            {}
func (m *recordingMetrics) BalanceDrift(_ string, _ string, delta decimal.Decimal) {
	m.balanceDriftCalled = true
	m.balanceDriftCalls = append(m.balanceDriftCalls, delta)
}
func (m *recordingMetrics) NegativeBalanceDetected(string, string) {
	m.negativeBalanceDetectedCalls++
}

// TestRollup_ReportsStuckAndPendingSeparately is F-5's pin (2026-09-03
// independent review).
//
// StuckRollups had no behaviour pin at all. The reviewer switched its
// emission off -- `if stuck, err := s.queue.CountStuckRollups(ctx); err ==
// nil && false` -- and `go test ./...`, postgres included, stayed green.
// The two gates that exist for this both said yes: the coverage gate found
// the call expression (an unreachable one, but AST does not evaluate), and
// the census gate found the name on recordingMetrics' own empty method
// declaration, which every mock of a wide interface has to write.
//
// The two gauges are asserted separately because that separation is the
// signal's whole reason for existing (B-m10): pending drains as the queue
// is worked, stuck does not and never will until an operator resets the
// item (cmd/ledger-cli's `rollup reset-claim`). An alert on pending alone
// goes quiet while the stuck items sit there.
func TestRollup_ReportsStuckAndPendingSeparately(t *testing.T) {
	queue := &mockRollupQueuer{
		items:   []RollupQueueItem{{ID: 1, AccountHolder: 900, CurrencyID: 1, ClassificationID: 30}},
		pending: 7,
		stuck:   3,
	}
	cpRW := newMockCheckpointRW()
	entries := &mockEntrySummer{
		debitByClass:  map[int64]decimal.Decimal{30: decimal.NewFromInt(10)},
		creditByClass: map[int64]decimal.Decimal{},
		maxEntryID:    5,
		maxEntryAt:    time.Now(),
	}
	cls := &mockClassificationLister{classifications: []ClassificationDim{
		{ID: 30, UID: "cls-30", Code: "asset", NormalSide: core.NormalSideDebit},
	}}
	metrics := &recordingMetrics{}

	svc := NewRollupService(queue, cpRW, entries, cls, core.NewEngine(core.WithMetrics(metrics)))
	_, err := svc.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)

	require.Equal(t, []int64{3}, metrics.stuckRollups,
		"the stuck-item gauge must be emitted once per tick with the queue's count. Without this assertion the emission "+
			"can be switched off and every gate stays green (F-5)")
	require.Equal(t, []int64{7}, metrics.pendingRollups,
		"pending and stuck are separate gauges on purpose: pending drains as the queue is worked and stuck does not, "+
			"so reporting one for the other makes a permanently stuck item look like transient backlog")
}

// TestRollup_ReportsQueueDepthOnAnEmptyTick pins the case the gauges exist
// for (team-lead ruling, 2026-09-03).
//
// ProcessBatch used to return early when DequeueRollupBatch handed back
// nothing, before either gauge was emitted. StuckRollups counts items that
// have exhausted their retries -- which is exactly what DequeueRollupBatch
// does NOT hand back -- so a queue that is entirely stuck dequeues nothing,
// and both gauges went unwritten at the moment they were the signal. A
// gauge that stops being written goes stale rather than to zero, so the
// dashboard keeps showing the last healthy value.
func TestRollup_ReportsQueueDepthOnAnEmptyTick(t *testing.T) {
	queue := &mockRollupQueuer{stuck: 4} // nothing dequeueable, four wedged
	metrics := &recordingMetrics{}
	svc := NewRollupService(queue, newMockCheckpointRW(), &mockEntrySummer{}, &mockClassificationLister{},
		core.NewEngine(core.WithMetrics(metrics)))

	processed, err := svc.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	require.Zero(t, processed, "the fixture must dequeue nothing -- that is the case under test")

	require.Equal(t, []int64{4}, metrics.stuckRollups,
		"a tick that dequeued nothing must still report the stuck count. Four items wedged and zero dequeueable is "+
			"precisely the state this gauge exists to alarm on, and it is the state in which the early return used to "+
			"skip it")
	require.Equal(t, []int64{0}, metrics.pendingRollups,
		"pending must be reported as the zero it is, not left unwritten -- an un-emitted gauge goes stale on the "+
			"dashboard rather than dropping to zero")
}
