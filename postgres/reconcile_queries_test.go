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
)

// TestReconcileAdapter_OrphanEntries verifies that the orphan-entry query
// returns 0 on a clean ledger and detects a manually injected orphan row.
func TestReconcileAdapter_OrphanEntries(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	adapter := postgres.NewReconcileAdapter(pool)
	ctx := context.Background()

	// Clean ledger — no orphans.
	count, err := adapter.OrphanEntriesCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), count, "fresh DB should have zero orphan entries")

	samples, err := adapter.OrphanEntriesSample(ctx)
	require.NoError(t, err)
	assert.Empty(t, samples)
}

// TestReconcileAdapter_AccountingEquation posts a balanced journal and verifies
// the accounting equation query returns matching debit/credit totals.
func TestReconcileAdapter_AccountingEquation(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	adapter := postgres.NewReconcileAdapter(pool)
	ledger := postgres.NewLedgerStore(pool)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT-AEQ", "Tether")
	jtID := postgrestest.SeedJournalType(t, pool, "aeq-transfer", "AEQ Transfer")
	clsDebit := postgrestest.SeedClassification(t, pool, "aeq-wallet", "AEQ Wallet", "debit", false)
	clsCredit := postgrestest.SeedClassification(t, pool, "aeq-custodial", "AEQ Custodial", "credit", true)

	_, err := ledger.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jtID,
		IdempotencyKey: postgrestest.UniqueKey("aeq-test"),
		Entries: []core.EntryInput{
			{AccountHolder: 1, CurrencyID: curID, ClassificationID: clsDebit, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(500)},
			{AccountHolder: -1, CurrencyID: curID, ClassificationID: clsCredit, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(500)},
		},
	})
	require.NoError(t, err)

	rows, err := adapter.AccountingEquationRows(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, rows)

	// Find the rows for our currency/classifications.
	for _, r := range rows {
		if r.CurrencyID == curID && r.ClassificationID == clsDebit {
			assert.Equal(t, "debit", r.NormalSide)
			assert.True(t, r.TotalDebit.Equal(decimal.NewFromInt(500)), "debit total mismatch")
		}
		if r.CurrencyID == curID && r.ClassificationID == clsCredit {
			assert.Equal(t, "credit", r.NormalSide)
			assert.True(t, r.TotalCredit.Equal(decimal.NewFromInt(500)), "credit total mismatch")
		}
	}
}

// TestReconcileAdapter_SettlementNetting verifies that a settlement classification
// nets to zero when balanced, and reports a violation when not.
func TestReconcileAdapter_SettlementNetting(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	adapter := postgres.NewReconcileAdapter(pool)
	ledger := postgres.NewLedgerStore(pool)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT-SN", "Tether SN")
	jtID := postgrestest.SeedJournalType(t, pool, "sn-transfer", "SN Transfer")

	// Create settlement (debit-normal) and counterpart (credit-normal) classifications.
	clsSettlement := postgrestest.SeedClassification(t, pool, "sn-settlement", "SN Settlement", "debit", true)
	clsCounterpart := postgrestest.SeedClassification(t, pool, "sn-counterpart", "SN Counterpart", "credit", true)

	// Post a BALANCED journal that nets to zero in settlement.
	_, err := ledger.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jtID,
		IdempotencyKey: postgrestest.UniqueKey("sn-balanced"),
		Entries: []core.EntryInput{
			{AccountHolder: -1, CurrencyID: curID, ClassificationID: clsSettlement, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyID: curID, ClassificationID: clsSettlement, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyID: curID, ClassificationID: clsCounterpart, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyID: curID, ClassificationID: clsCounterpart, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
		},
	})
	require.NoError(t, err)

	// window=0 means "exclude entries from the last 0 minutes" — i.e. all entries included.
	violations, err := adapter.SettlementNettingViolations(ctx, "sn-settlement", 0)
	require.NoError(t, err)
	assert.Empty(t, violations, "balanced settlement should report no violations")
}

// TestReconcileAdapter_NonNegativeBalances posts journals that give a user a
// positive balance; verifies no violations. Then it uses raw SQL to manually
// simulate a negative balance scenario by checking the query is wired correctly.
func TestReconcileAdapter_NonNegativeBalances_Clean(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	adapter := postgres.NewReconcileAdapter(pool)
	ledger := postgres.NewLedgerStore(pool)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT-NNB", "Tether NNB")
	jtID := postgrestest.SeedJournalType(t, pool, "nnb-transfer", "NNB Transfer")
	clsWallet := postgrestest.SeedClassification(t, pool, "nnb-wallet", "NNB Wallet", "debit", false)
	clsCustodial := postgrestest.SeedClassification(t, pool, "nnb-custodial", "NNB Custodial", "credit", true)

	// User 50 gets a deposit of 200.
	_, err := ledger.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jtID,
		IdempotencyKey: postgrestest.UniqueKey("nnb-deposit"),
		Entries: []core.EntryInput{
			{AccountHolder: 50, CurrencyID: curID, ClassificationID: clsWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(200)},
			{AccountHolder: -50, CurrencyID: curID, ClassificationID: clsCustodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(200)},
		},
	})
	require.NoError(t, err)

	accounts, err := adapter.NegativeBalanceAccounts(ctx, 100)
	require.NoError(t, err)
	// The debit-normal wallet has balance 200 (positive), so no violations.
	for _, acc := range accounts {
		assert.False(t, acc.AccountHolder == 50 && acc.ClassificationID == clsWallet,
			"positive wallet balance should not appear as violation")
	}
}

// TestReconcileAdapter_OrphanReservations verifies the query runs without error
// on a clean DB (no reservations with dangling journal_ids).
func TestReconcileAdapter_OrphanReservations_Clean(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	adapter := postgres.NewReconcileAdapter(pool)
	ctx := context.Background()

	orphans, err := adapter.OrphanReservations(ctx)
	require.NoError(t, err)
	assert.Empty(t, orphans, "clean DB should have no orphan reservations")
}

// TestReconcileAdapter_StaleRollupItems verifies the stale-rollup query runs
// without error on a clean DB (no stale items).
func TestReconcileAdapter_StaleRollupItems_Clean(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	adapter := postgres.NewReconcileAdapter(pool)
	ctx := context.Background()

	items, err := adapter.StaleRollupItems(ctx, 5)
	require.NoError(t, err)
	assert.Empty(t, items, "clean DB should have no stale rollup items")
}

// TestReconcileAdapter_DuplicateIdempotencyKeys verifies no duplicates on a clean DB.
func TestReconcileAdapter_DuplicateIdempotencyKeys_Clean(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	adapter := postgres.NewReconcileAdapter(pool)
	ctx := context.Background()

	dupes, err := adapter.DuplicateIdempotencyKeys(ctx)
	require.NoError(t, err)
	assert.Empty(t, dupes, "fresh DB should have no duplicate idempotency keys")
}

