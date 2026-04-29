package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// setupTrendFixture creates currency, classification, snapshot, and journal entries
// for use in balance trend tests. Returns (currencyID, classificationID).
func setupTrendFixture(t *testing.T, pool interface {
	Exec(ctx context.Context, sql string, args ...any) (interface{ RowsAffected() int64 }, error)
}) (int64, int64) {
	t.Helper()
	return 0, 0
}

func TestBalanceTrends_GapFill(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	classStore := postgres.NewClassificationStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)
	trendsStore := postgres.NewBalanceTrendsStore(pool, ledgerStore)

	// Create currency and classification.
	usdt, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{Code: "USDT-TREND", Name: "Tether USD Trend"})
	require.NoError(t, err)

	wallet, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code:       "wallet_trend",
		Name:       "Wallet Trend",
		NormalSide: core.NormalSideDebit,
	})
	require.NoError(t, err)

	system, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code:       "custodial_trend",
		Name:       "Custodial Trend",
		NormalSide: core.NormalSideCredit,
		IsSystem:   true,
	})
	require.NoError(t, err)

	jt, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{Code: "deposit_trend", Name: "Deposit Trend"})
	require.NoError(t, err)

	userID := int64(9001)
	amount := decimal.NewFromInt(500)

	// Post a journal to generate entries.
	_, err = ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jt.ID,
		IdempotencyKey: postgrestest.UniqueKey("trend-j1"),
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyID: usdt.ID, ClassificationID: wallet.ID, EntryType: core.EntryTypeDebit, Amount: amount},
			{AccountHolder: -userID, CurrencyID: usdt.ID, ClassificationID: system.ID, EntryType: core.EntryTypeCredit, Amount: amount},
		},
		Source: "trend_test",
	})
	require.NoError(t, err)

	// Insert a snapshot for 3 days ago so we have a known historical balance.
	threeDaysAgo := time.Now().UTC().AddDate(0, 0, -3)
	threeDaysAgoDate := time.Date(threeDaysAgo.Year(), threeDaysAgo.Month(), threeDaysAgo.Day(), 0, 0, 0, 0, time.UTC)

	_, err = pool.Exec(ctx,
		`INSERT INTO balance_snapshots (account_holder, currency_id, classification_id, snapshot_date, balance)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT DO NOTHING`,
		userID, usdt.ID, wallet.ID, threeDaysAgoDate, "250.000000000000000000",
	)
	require.NoError(t, err)

	// Query trends for 5 days ending today.
	from := time.Now().UTC().AddDate(0, 0, -4)
	until := time.Now().UTC()

	points, err := trendsStore.GetBalanceTrends(ctx, core.BalanceTrendFilter{
		AccountHolder:    userID,
		CurrencyID:       usdt.ID,
		ClassificationID: wallet.ID,
		From:             from,
		Until:            until,
	})
	require.NoError(t, err)

	// We expect 5 data points (inclusive range).
	assert.Len(t, points, 5, "expected one point per day")

	// Points should be ordered ascending by date.
	for i := 1; i < len(points); i++ {
		assert.True(t, !points[i].Date.Before(points[i-1].Date),
			"points should be ordered ascending: %s < %s", points[i-1].Date, points[i].Date)
	}

	// The snapshot at day -3 should appear; days before it (day -4) are forward-filled
	// to zero (no prior snapshot); days after it are forward-filled to the snapshot value.
	day0 := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	assert.Equal(t, day0, points[0].Date)

	// Day -4 (index 0): no snapshot before this, so balance should be 0.
	assert.True(t, points[0].Balance.IsZero(), "day -4 balance: expected 0, got %s", points[0].Balance)

	// Day -3 (index 1): snapshot exists with balance 250.
	assert.True(t, points[1].Balance.Equal(decimal.NewFromInt(250)),
		"day -3 balance: expected 250, got %s", points[1].Balance)

	// Day -2 and -1 (index 2, 3): should be forward-filled from day -3 → 250.
	assert.True(t, points[2].Balance.Equal(decimal.NewFromInt(250)),
		"day -2 balance (gap-fill): expected 250, got %s", points[2].Balance)
	assert.True(t, points[3].Balance.Equal(decimal.NewFromInt(250)),
		"day -1 balance (gap-fill): expected 250, got %s", points[3].Balance)

	// Today (index 4): overridden by live balance (checkpoint+delta).
	// We posted 500 today so live balance should be 500.
	liveBalance := points[4].Balance
	assert.True(t, liveBalance.Equal(decimal.NewFromInt(500)),
		"today live balance override: expected 500, got %s", liveBalance)
}

func TestBalanceTrends_NoSnapshots(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)
	trendsStore := postgres.NewBalanceTrendsStore(pool, ledgerStore)

	usdt, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{Code: "USDT-NOSN", Name: "USDT NoSnapshot"})
	require.NoError(t, err)

	// Query trends for an account that has no entries or snapshots.
	from := time.Now().UTC().AddDate(0, 0, -2)
	until := time.Now().UTC()

	points, err := trendsStore.GetBalanceTrends(ctx, core.BalanceTrendFilter{
		AccountHolder:    int64(99999),
		CurrencyID:       usdt.ID,
		ClassificationID: 0, // all
		From:             from,
		Until:            until,
	})
	require.NoError(t, err)
	// Should have 3 points (day -2, -1, today), all zero.
	assert.Len(t, points, 3)
	for _, p := range points {
		assert.True(t, p.Balance.IsZero(), "empty account balance should be zero, got %s", p.Balance)
		assert.True(t, p.Inflow.IsZero())
		assert.True(t, p.Outflow.IsZero())
	}
}

func TestBalanceTrends_UntilBeforeFrom(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	trendsStore := postgres.NewBalanceTrendsStore(pool, ledgerStore)

	_, err := trendsStore.GetBalanceTrends(ctx, core.BalanceTrendFilter{
		AccountHolder: 1,
		CurrencyID:    1,
		From:          time.Now(),
		Until:         time.Now().AddDate(0, 0, -1), // until before from
	})
	assert.Error(t, err)
}
