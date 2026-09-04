package postgres_test

// Pins for I-1/I-24:
//   - the DB-layer deferred constraint trigger that refuses a per-currency
//     imbalance written by direct SQL
//     (docs/plans/2026-08-21-tamper-evident-ledger-design.md §2 C1, §5), and
//   - the fleet-wide "journal_dr_cr" per-journal reconcile scan that
//     complements it (§2 M1), for imbalances that predate the guard.
//
// Both tests need a journal that IS unbalanced, which the guard exists to
// make impossible. They produce one by disabling the trigger for exactly the
// crafting statement and re-enabling it immediately, rather than by migrating
// to a schema version that predates it: the schema is a single baseline and
// the guard is present from its first statement, so there is no "before" to
// travel back to.
//
// Disabling is a strictly better fixture than the old version travel was.
// Version travel proved "this guard did not exist at version N"; disabling
// proves "this guard, in the schema as shipped, is the thing doing the work"
// -- which is what the invariant actually claims. It also lets the legitimate
// journals be posted through store.PostJournal instead of hand-written SQL
// imitating an older table shape.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// withBalanceTriggerDisabled runs fn with the per-currency balance constraint
// trigger switched off, and switches it back on afterwards even if fn fails.
//
// Each statement runs on its own connection-level transaction, so the trigger
// is genuinely off at the moment fn's writes commit -- a DEFERRABLE INITIALLY
// DEFERRED trigger fires at COMMIT, so disabling and re-enabling inside one
// transaction with fn would let it fire anyway.
//
// This requires table ownership, which the test connection has and ledger_app
// deliberately does not (I-22). That is the point: crafting this corruption is
// not something the application credential can do.
func withBalanceTriggerDisabled(t *testing.T, pool *pgxpool.Pool, ctx context.Context, fn func()) {
	t.Helper()

	_, err := pool.Exec(ctx, `ALTER TABLE journal_entries DISABLE TRIGGER trg_check_journal_currency_balance`)
	require.NoError(t, err, "disabling the balance trigger is the only way to craft an unbalanced journal")

	reEnabled := false
	enable := func() {
		if reEnabled {
			return
		}
		reEnabled = true
		_, err := pool.Exec(ctx, `ALTER TABLE journal_entries ENABLE TRIGGER trg_check_journal_currency_balance`)
		require.NoError(t, err, "the trigger must come back on -- every assertion after this depends on it")
	}
	t.Cleanup(enable)

	fn()
	enable()
}

// postBalancedJournal posts a legitimate two-entry journal through the real
// write path and returns its internal id.
func postBalancedJournal(
	t *testing.T,
	store *postgres.LedgerStore,
	pool *pgxpool.Pool,
	ctx context.Context,
	deps invariantsFixture,
	holder int64,
	amount decimal.Decimal,
	key string,
) int64 {
	t.Helper()

	j, err := store.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: deps.JournalType,
		IdempotencyKey: postgrestest.UniqueKey(key),
		Source:         "balance-trigger-test",
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: deps.Currency, ClassificationUID: deps.MainWallet, EntryType: core.EntryTypeDebit, Amount: amount},
			{AccountHolder: -holder, CurrencyUID: deps.Currency, ClassificationUID: deps.Custodial, EntryType: core.EntryTypeCredit, Amount: amount},
		},
	})
	require.NoError(t, err)
	return postgrestest.InternalID(t, pool, "journals", j.UID)
}

// TestJournalBalanceTrigger_RejectsDirectSQLImbalance pins I-1/I-24: a direct
// SQL INSERT that leaves a journal unbalanced by currency is refused at commit
// by the database, not merely by application code.
//
// The first half -- the same INSERT succeeding with the trigger disabled -- is
// what keeps the second half from being vacuous. Without it, an INSERT that
// failed for any unrelated reason would look like the guard working.
func TestJournalBalanceTrigger_RejectsDirectSQLImbalance(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	store, deps := setupInvariantsFixture(t, pool, ctx)

	currencyID := postgrestest.InternalID(t, pool, "currencies", deps.Currency)
	classID := postgrestest.InternalID(t, pool, "classifications", deps.MainWallet)

	unbalance := func(journalID, holder int64) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
			 VALUES ($1, $2, $3, $4, 'debit', $5)`,
			journalID, holder, currencyID, classID, decimal.NewFromInt(50),
		)
		return err
	}

	journalA := postBalancedJournal(t, store, pool, ctx, deps, 4201, decimal.NewFromInt(100), "bal-trig-off")
	withBalanceTriggerDisabled(t, pool, ctx, func() {
		require.NoError(t, unbalance(journalA, 4201),
			"with the guard off, an extra debit entry with no matching credit goes in -- this is what the guard is preventing")
	})

	journalB := postBalancedJournal(t, store, pool, ctx, deps, 4202, decimal.NewFromInt(200), "bal-trig-on")
	err := unbalance(journalB, 4202)
	require.Error(t, err, "with the guard on, the identical direct-SQL insert must be rejected by the DB layer")
	assert.Contains(t, err.Error(), "unbalanced entries", "the trigger's error message should name the failure")
}

// TestUnbalancedJournalsFleetScan_CatchesWhatGlobalEqualityMisses pins I-24 /
// M1: the fleet-wide "journal_dr_cr" scan catches two individually-unbalanced
// journals that a GLOBAL debit==credit equality check cannot see, because
// summed together debit still equals credit.
//
// The guard is not retroactive -- it refuses new imbalances but says nothing
// about rows already present -- so this scan is what covers anything that got
// in before it, or around it.
func TestUnbalancedJournalsFleetScan_CatchesWhatGlobalEqualityMisses(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	store, deps := setupInvariantsFixture(t, pool, ctx)

	currencyID := postgrestest.InternalID(t, pool, "currencies", deps.Currency)
	classID := postgrestest.InternalID(t, pool, "classifications", deps.MainWallet)
	sysClassID := postgrestest.InternalID(t, pool, "classifications", deps.Custodial)

	journalA := postBalancedJournal(t, store, pool, ctx, deps, 4301, decimal.NewFromInt(150), "fleet-a")
	journalB := postBalancedJournal(t, store, pool, ctx, deps, 4302, decimal.NewFromInt(150), "fleet-b")

	// A gains an unmatched debit, B an unmatched credit of the same size, so
	// the two drifts cancel in any global sum.
	withBalanceTriggerDisabled(t, pool, ctx, func() {
		_, err := pool.Exec(ctx,
			`INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
			 VALUES ($1, $2, $3, $4, 'debit', $5)`,
			journalA, 4301, currencyID, classID, decimal.NewFromInt(50))
		require.NoError(t, err, "crafting the corrupted fixture must itself succeed")

		_, err = pool.Exec(ctx,
			`INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
			 VALUES ($1, $2, $3, $4, 'credit', $5)`,
			journalB, -4302, currencyID, sysClassID, decimal.NewFromInt(50))
		require.NoError(t, err, "crafting the corrupted fixture must itself succeed")
	})

	// The GLOBAL debit==credit equality check is blind to this: summed across
	// both journals, debit == credit still holds.
	var totalDebit, totalCredit decimal.Decimal
	err := pool.QueryRow(ctx, `
		SELECT
		  COALESCE(SUM(CASE WHEN entry_type='debit' THEN amount END), 0),
		  COALESCE(SUM(CASE WHEN entry_type='credit' THEN amount END), 0)
		FROM journal_entries
		WHERE journal_id IN ($1, $2)
	`, journalA, journalB).Scan(&totalDebit, &totalCredit)
	require.NoError(t, err)
	require.True(t, totalDebit.Equal(totalCredit),
		"the crafted fixture must be globally balanced (debit=%s credit=%s) -- that's the whole point of M1", totalDebit, totalCredit)

	// The fleet-wide per-journal scan must catch both individually.
	adapter := postgres.NewReconcileAdapter(pool)
	count, err := adapter.UnbalancedJournalsCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), count, "both journal A and journal B must be flagged individually, even though they net to zero globally")

	samples, err := adapter.UnbalancedJournalsSample(ctx)
	require.NoError(t, err)
	var sawA, sawB bool
	for _, s := range samples {
		switch s.JournalID {
		case journalA:
			sawA = true
			assert.True(t, s.Drift.Equal(decimal.NewFromInt(50)), "journal A should drift +50, got %s", s.Drift)
		case journalB:
			sawB = true
			assert.True(t, s.Drift.Equal(decimal.NewFromInt(-50)), "journal B should drift -50, got %s", s.Drift)
		}
	}
	assert.True(t, sawA, "expected journal A in the sample")
	assert.True(t, sawB, "expected journal B in the sample")
}
