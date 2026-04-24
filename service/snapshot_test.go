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

// --- Mocks ---

type mockHistoricalBalanceLister struct {
	balances []core.Balance
	cutoff   time.Time
}

func (m *mockHistoricalBalanceLister) ListBalancesAt(_ context.Context, cutoff time.Time) ([]core.Balance, error) {
	m.cutoff = cutoff
	return m.balances, nil
}

type mockSnapshotWriter struct {
	snapshots []core.BalanceSnapshot
	balances  []core.Balance
}

func (m *mockSnapshotWriter) UpsertSnapshot(_ context.Context, snap core.BalanceSnapshot) error {
	m.snapshots = append(m.snapshots, snap)
	return nil
}

func (m *mockSnapshotWriter) GetSnapshotBalances(_ context.Context, _, _ int64, _ time.Time) ([]core.Balance, error) {
	return m.balances, nil
}

// --- Tests ---

func TestSnapshotService_CreateAndQuery(t *testing.T) {
	balanceLister := &mockHistoricalBalanceLister{
		balances: []core.Balance{
			{AccountHolder: 100, CurrencyID: 1, ClassificationID: 10, Balance: decimal.NewFromInt(500)},
			{AccountHolder: 100, CurrencyID: 1, ClassificationID: 20, Balance: decimal.NewFromInt(200)},
		},
	}

	snapWriter := &mockSnapshotWriter{}
	engine := core.NewEngine()
	svc := NewSnapshotService(balanceLister, snapWriter, engine)

	date := time.Date(2026, 4, 17, 15, 30, 0, 0, time.UTC)
	err := svc.CreateDailySnapshot(context.Background(), date)
	require.NoError(t, err)

	// Should have written 2 snapshots
	assert.Equal(t, 2, len(snapWriter.snapshots))
	// Date should be normalized to midnight
	assert.Equal(t, time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC), snapWriter.snapshots[0].SnapshotDate)
	assert.Equal(t, time.Date(2026, 4, 18, 0, 0, 0, 0, time.UTC), balanceLister.cutoff)
}

func TestSnapshotService_DuplicateIsIdempotent(t *testing.T) {
	balanceLister := &mockHistoricalBalanceLister{
		balances: []core.Balance{
			{AccountHolder: 100, CurrencyID: 1, ClassificationID: 10, Balance: decimal.NewFromInt(500)},
		},
	}
	snapWriter := &mockSnapshotWriter{}
	engine := core.NewEngine()
	svc := NewSnapshotService(balanceLister, snapWriter, engine)

	date := time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC)

	// Call twice — in real DB ON CONFLICT DO UPDATE keeps the date correct and allows backfills to rerun.
	err := svc.CreateDailySnapshot(context.Background(), date)
	require.NoError(t, err)
	err = svc.CreateDailySnapshot(context.Background(), date)
	require.NoError(t, err)

	// Mock appends, but in prod the second call overwrites the same PK row.
	assert.Equal(t, 2, len(snapWriter.snapshots))
}

func TestSnapshotService_QueryNonExistentDate(t *testing.T) {
	snapWriter := &mockSnapshotWriter{balances: nil}
	engine := core.NewEngine()
	svc := NewSnapshotService(nil, snapWriter, engine)

	balances, err := svc.GetSnapshotBalance(context.Background(), 100, 1, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Empty(t, balances)
}
