package postgres_test

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

// TestRollup_BoundarySemanticsAcrossConsecutiveRollups pins two things the
// service-level unit tests cannot, because they mock SumEntriesSince:
//
//  1. The SQL boundary is `id > last_entry_id`, NOT `id >=`. A second rollup
//     must not re-count the entry sitting exactly at the previous checkpoint's
//     last_entry_id. If the SQL regressed to `id >=`, the boundary entry would
//     be folded into the delta a second time and the materialized balance would
//     over-count (invariant I-5 violation).
//  2. The REPEATABLE READ snapshot path in RollupAdapter.SumEntriesSince runs
//     end to end against real PostgreSQL (the unit tests only exercise mocks),
//     so the two-read snapshot wrapping is verified to produce a correct balance.
//
// Strategy: post a journal, materialize the checkpoint via ProcessBatch, post a
// second journal for the same dimensions, materialize again, then assert the
// computed balance equals the true sum of all entries. A double-count (>=) or
// under-count (split snapshot) would make GetBalance diverge from the sum.
func TestRollup_BoundarySemanticsAcrossConsecutiveRollups(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	classStore := postgres.NewClassificationStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)
	rollupAdapter := postgres.NewRollupAdapter(pool)
	engine := core.NewEngine()
	rollupSvc := service.NewRollupService(rollupAdapter, rollupAdapter, rollupAdapter, classStore, engine)

	usdt, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{
		Code: "USDT-RB", Name: "Tether Rollup Boundary",
	})
	require.NoError(t, err)

	custodial, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "custodial_rb", Name: "Custodial Rollup Boundary", NormalSide: core.NormalSideDebit, IsSystem: true,
	})
	require.NoError(t, err)

	wallet, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "main_wallet_rb", Name: "Main Wallet Rollup Boundary", NormalSide: core.NormalSideCredit,
	})
	require.NoError(t, err)

	jt, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{
		Code: "deposit_rb", Name: "Deposit Rollup Boundary",
	})
	require.NoError(t, err)

	userID := int64(7777)
	sysID := core.SystemAccountHolder(userID)

	postDeposit := func(amount int64, keySuffix string) {
		t.Helper()
		_, err := ledgerStore.PostJournal(ctx, core.JournalInput{
			JournalTypeID:  jt.ID,
			IdempotencyKey: postgrestest.UniqueKey("rb-" + keySuffix),
			Entries: []core.EntryInput{
				{AccountHolder: sysID, CurrencyID: usdt.ID, ClassificationID: custodial.ID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(amount)},
				{AccountHolder: userID, CurrencyID: usdt.ID, ClassificationID: wallet.ID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(amount)},
			},
			Source: "test",
		})
		require.NoError(t, err)
	}

	// Round 1: post 500, materialize. PostJournal auto-enqueues both dimensions.
	postDeposit(500, "j1")
	processed, err := rollupSvc.ProcessBatch(ctx, 10)
	require.NoError(t, err)
	require.GreaterOrEqual(t, processed, 1, "first rollup should materialize at least the two dimensions")

	// Checkpoints must now exist (otherwise round 2 wouldn't exercise the
	// since = last_entry_id boundary at all).
	var cpCount int
	require.NoError(t,
		pool.QueryRow(ctx,
			"SELECT count(*) FROM balance_checkpoints WHERE currency_id = $1 AND account_holder IN ($2, $3)",
			usdt.ID, userID, sysID,
		).Scan(&cpCount),
	)
	require.Equal(t, 2, cpCount, "both dimensions should have a checkpoint after the first rollup")

	// Round 2: post 300 for the SAME dimensions. The new entries have ids
	// strictly greater than the checkpoint's last_entry_id; the boundary entry
	// (the one whose id == last_entry_id) must NOT be re-summed.
	postDeposit(300, "j2")
	// Drain the queue fully — the post-processing re-check may re-enqueue.
	for range 5 {
		n, err := rollupSvc.ProcessBatch(ctx, 10)
		require.NoError(t, err)
		if n == 0 {
			break
		}
	}

	// Both balances must equal the true sum (500 + 300 = 800). A `>=` boundary
	// would over-count the round-1 entry; a split-snapshot under-count would
	// drop it.
	walletBal, err := ledgerStore.GetBalance(ctx, userID, usdt.ID, wallet.ID)
	require.NoError(t, err)
	assert.True(t, walletBal.Equal(decimal.NewFromInt(800)),
		"wallet balance must be 800 (no double-count, no under-count), got %s", walletBal)

	custodialBal, err := ledgerStore.GetBalance(ctx, sysID, usdt.ID, custodial.ID)
	require.NoError(t, err)
	assert.True(t, custodialBal.Equal(decimal.NewFromInt(800)),
		"custodial balance must be 800 (no double-count, no under-count), got %s", custodialBal)
}
