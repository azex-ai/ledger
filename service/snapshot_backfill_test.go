package service

// F-M3 (2026-09-02 audit): SnapshotBackfillService.CheckAndBackfillOnStartup
// had zero test coverage -- only mentioned in ledger.go's doc comment. The
// whole method could be swapped for `return nil` and `go test ./...` stayed
// green.

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

type mockSnapshotCountReader struct {
	count    int64
	earliest time.Time
}

func (m *mockSnapshotCountReader) CountSnapshots(_ context.Context) (int64, error) {
	return m.count, nil
}

func (m *mockSnapshotCountReader) EarliestJournalDate(_ context.Context) (time.Time, error) {
	return m.earliest, nil
}

type mockSparseSnapshotter struct {
	inserted int
}

func (m *mockSparseSnapshotter) UpsertSnapshotSparse(_ context.Context, _ core.BalanceSnapshot) (bool, error) {
	m.inserted++
	return true, nil
}

// TestSnapshotBackfillService_CheckAndBackfillOnStartup_FillsFromEarliestJournal
// pins F-M3: with zero existing snapshots and journal activity starting
// several days ago, startup must actually backfill -- one sparse snapshot
// write per (day, balance dimension). A no-op implementation
// (`return nil` on line 1) calls neither CountSnapshots' sibling nor
// UpsertSnapshotSparse and goes red on the assertion below.
func TestSnapshotBackfillService_CheckAndBackfillOnStartup_FillsFromEarliestJournal(t *testing.T) {
	yesterday := time.Now().UTC().AddDate(0, 0, -1)
	earliest := yesterday.AddDate(0, 0, -2) // 3 days of history, inclusive
	lister := &mockHistoricalBalanceLister{
		balances: []core.Balance{
			{AccountHolder: 100, CurrencyUID: "cur-1", ClassificationUID: "cls-10", Balance: decimal.NewFromInt(500)},
		},
	}
	counter := &mockSnapshotCountReader{count: 0, earliest: earliest}
	sparse := &mockSparseSnapshotter{}
	engine := core.NewEngine()
	svc := NewSnapshotBackfillService(lister, counter, sparse, engine)

	require.NoError(t, svc.CheckAndBackfillOnStartup(context.Background()))
	assert.Equal(t, 3, sparse.inserted, "one snapshot per day from earliest through yesterday (inclusive)")
}

// TestSnapshotBackfillService_CheckAndBackfillOnStartup_SkipsWhenSnapshotsExist
// pins the idempotent short-circuit: if any snapshot already exists, startup
// must not backfill at all.
func TestSnapshotBackfillService_CheckAndBackfillOnStartup_SkipsWhenSnapshotsExist(t *testing.T) {
	lister := &mockHistoricalBalanceLister{
		balances: []core.Balance{{AccountHolder: 100, CurrencyUID: "cur-1", ClassificationUID: "cls-10", Balance: decimal.NewFromInt(500)}},
	}
	counter := &mockSnapshotCountReader{count: 5, earliest: time.Now().UTC().AddDate(0, 0, -10)}
	sparse := &mockSparseSnapshotter{}
	engine := core.NewEngine()
	svc := NewSnapshotBackfillService(lister, counter, sparse, engine)

	require.NoError(t, svc.CheckAndBackfillOnStartup(context.Background()))
	assert.Equal(t, 0, sparse.inserted, "must not backfill when snapshots already exist")
}

// TestSnapshotBackfillService_CheckAndBackfillOnStartup_NoJournalsIsANoOp pins
// the other early-return: an empty ledger (EarliestJournalDate zero value)
// has nothing to backfill.
func TestSnapshotBackfillService_CheckAndBackfillOnStartup_NoJournalsIsANoOp(t *testing.T) {
	lister := &mockHistoricalBalanceLister{}
	counter := &mockSnapshotCountReader{count: 0, earliest: time.Time{}}
	sparse := &mockSparseSnapshotter{}
	engine := core.NewEngine()
	svc := NewSnapshotBackfillService(lister, counter, sparse, engine)

	require.NoError(t, svc.CheckAndBackfillOnStartup(context.Background()))
	assert.Equal(t, 0, sparse.inserted)
}
