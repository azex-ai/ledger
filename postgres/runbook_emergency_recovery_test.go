package postgres_test

// Pins the M-5 fix (docs/plans/2026-08-26-audit-remediation-contracts.md's
// follow-on fix-backend-1 batch, `.local/independent-review-2026-08-26.md`
// Major M-5): docs/RUNBOOK.md §9 step 4's emergency-recovery GRANT used to be
// a single blanket `GRANT INSERT ON journals, journal_entries, events,
// bookings TO ledger_app` -- which silently undoes migration 008 (I-42)'s
// column-level restriction the moment an operator follows the runbook after
// an emergency freeze. There is no compiled code path that "runs" a
// markdown file, so this test pins the two SQL fragments directly: the old
// (naive, blanket) recovery step really does reopen the id column, and the
// corrected (column-level, re-running 008's own DO loop) recovery step does
// not. Read them side by side with docs/RUNBOOK.md §9 -- that file's
// "restore privileges" step must match the "corrected" phase below, never
// the "naive" one.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

func TestRunbookEmergencyRecovery_NaiveGrantReopensIDColumn(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	appPool := newAppPool(t, pool, "roles-test-app-runbook-naive-not-a-real-secret") //nolint:gosec // test-only credential

	curID := postgrestest.SeedCurrency(t, pool, "USDT-RBN", "Tether USD Runbook Naive")
	jtID := postgrestest.SeedJournalType(t, pool, "transfer_rbn", "Transfer Runbook Naive")
	wallet := postgrestest.SeedClassification(t, pool, "wallet_rbn", "Wallet Runbook Naive", "debit", false)
	custodial := postgrestest.SeedClassification(t, pool, "custodial_rbn", "Custodial Runbook Naive", "credit", true)
	holder := int64(9401)

	ledgerStore := postgres.NewLedgerStore(pool)
	j, err := ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("runbook-naive-anchor"),
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: wallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: curID, ClassificationUID: custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	})
	require.NoError(t, err)

	var journalID, currencyID, walletID int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM journals WHERE uid = $1::uuid`, j.UID).Scan(&journalID))
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM currencies WHERE uid = $1::uuid`, curID).Scan(&currencyID))
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM classifications WHERE uid = $1::uuid`, wallet).Scan(&walletID))

	// Step 2 of RUNBOOK.md §9: freeze.
	_, err = pool.Exec(ctx, `REVOKE INSERT ON journals, journal_entries, events, bookings FROM ledger_app`)
	require.NoError(t, err)

	// The OLD, buggy form of step 4: a single blanket GRANT naming
	// journal_entries alongside the other three tables. This is exactly what
	// docs/RUNBOOK.md §9 said before this fix.
	_, err = pool.Exec(ctx, `GRANT INSERT ON journals, journal_entries, events, bookings TO ledger_app`)
	require.NoError(t, err)

	// An explicit-id INSERT as ledger_app -- the exact attack migration 008
	// exists to refuse -- must now succeed again, proving the naive recovery
	// step silently undid 008's protection. Run inside a transaction that is
	// always rolled back: this test only needs to observe whether the
	// statement is *permitted*, not leave a forged row behind.
	tx, err := appPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, `
		INSERT INTO journal_entries (id, journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at, effective_at)
		VALUES (9223372036854775807, $1, $2, $3, $4, 'debit', 1, $5, $5)`,
		journalID, holder, currencyID, walletID, time.Now())
	require.NoError(t, err, "the naive blanket GRANT must reopen explicit-id INSERT on journal_entries -- if this now fails, the vulnerability this test pins no longer reproduces and the test itself needs updating, not silently left green for the wrong reason")
}

func TestRunbookEmergencyRecovery_CorrectedGrantKeepsIDColumnClosed(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	appPool := newAppPool(t, pool, "roles-test-app-runbook-fixed-not-a-real-secret") //nolint:gosec // test-only credential

	curID := postgrestest.SeedCurrency(t, pool, "USDT-RBF", "Tether USD Runbook Fixed")
	jtID := postgrestest.SeedJournalType(t, pool, "transfer_rbf", "Transfer Runbook Fixed")
	wallet := postgrestest.SeedClassification(t, pool, "wallet_rbf", "Wallet Runbook Fixed", "debit", false)
	custodial := postgrestest.SeedClassification(t, pool, "custodial_rbf", "Custodial Runbook Fixed", "credit", true)
	holder := int64(9402)

	ledgerStore := postgres.NewLedgerStore(pool)
	j, err := ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("runbook-fixed-anchor"),
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: wallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: curID, ClassificationUID: custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	})
	require.NoError(t, err)

	var journalID, currencyID, walletID int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM journals WHERE uid = $1::uuid`, j.UID).Scan(&journalID))
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM currencies WHERE uid = $1::uuid`, curID).Scan(&currencyID))
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM classifications WHERE uid = $1::uuid`, wallet).Scan(&walletID))

	// Step 2 of RUNBOOK.md §9: freeze.
	_, err = pool.Exec(ctx, `REVOKE INSERT ON journals, journal_entries, events, bookings FROM ledger_app`)
	require.NoError(t, err)

	// The FIXED step 4: journals/events/bookings get their table-level GRANT
	// back directly; journal_entries is restored through migration 008's own
	// DO loop instead (docs/RUNBOOK.md §9, corrected).
	_, err = pool.Exec(ctx, `GRANT INSERT ON journals, events, bookings TO ledger_app`)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `
		DO $$
		DECLARE
		    r RECORD;
		BEGIN
		    FOR r IN
		        SELECT c.relname
		        FROM pg_partition_tree('journal_entries'::regclass) pt
		        JOIN pg_class c ON c.oid = pt.relid
		    LOOP
		        EXECUTE format(
		            'GRANT INSERT (journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at, effective_at) ON public.%I TO ledger_app',
		            r.relname);
		    END LOOP;
		END $$;`)
	require.NoError(t, err)

	// The same explicit-id INSERT must still be refused at the ACL layer --
	// the corrected recovery step never reopened the id column.
	tx, err := appPool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `
		INSERT INTO journal_entries (id, journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at, effective_at)
		VALUES (9223372036854775807, $1, $2, $3, $4, 'debit', 1, $5, $5)`,
		journalID, holder, currencyID, walletID, time.Now())
	assertPermissionDenied(t, err)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, "42501", pgErr.Code)
	// The failed statement above aborts this transaction (SQLSTATE 25P02 on
	// any further use) -- roll it back before checking the id-omitting path
	// in a fresh transaction, rather than reusing a poisoned one.
	require.NoError(t, tx.Rollback(ctx))

	// The column-omitting production write path must still work normally --
	// the corrected recovery step must not be MORE restrictive than 008
	// itself, only exactly as restrictive.
	tx2, err := appPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx2.Rollback(ctx) }()
	_, err = tx2.Exec(ctx, `
		INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at, effective_at)
		VALUES ($1, $2, $3, $4, 'debit', 1, $5, $5)`,
		journalID, holder, currencyID, walletID, time.Now())
	require.NoError(t, err, "the corrected recovery step must not regress ordinary (id-omitting) inserts")
}
