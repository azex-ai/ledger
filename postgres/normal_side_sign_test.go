package postgres_test

// I-42 pins: the sign convergence collapsing 17 independent
// normal_side/entry_type sign implementations (7 Go, 10 SQL expressions in
// 3 shapes) into core.Sign (+ its wrappers core.SignedAmount / core.Delta)
// on the Go side and the ledger_signed_amount / ledger_signed_delta SQL
// functions (migration 009) on the SQL side.
//
// See docs/INVARIANTS.md I-42 and
// docs/audits/2026-08-25-financial-engineering/financial-correctness.md
// ("同一个符号语义有 17 处独立实现").

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
)

// TestLedgerSignedAmount_AgreesWithCoreSignedAmount pins that the SQL
// function migration 009 installs computes the identical sign core.Sign /
// core.SignedAmount does, for every valid (normal_side, entry_type)
// combination — the two implementations must never be allowed to drift back
// apart into two of the 17 independent copies this migration collapsed.
func TestLedgerSignedAmount_AgreesWithCoreSignedAmount(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	cases := []struct {
		name       string
		normalSide core.NormalSide
		entryType  core.EntryType
	}{
		{"debit-normal, debit entry", core.NormalSideDebit, core.EntryTypeDebit},
		{"debit-normal, credit entry", core.NormalSideDebit, core.EntryTypeCredit},
		{"credit-normal, credit entry", core.NormalSideCredit, core.EntryTypeCredit},
		{"credit-normal, debit entry", core.NormalSideCredit, core.EntryTypeDebit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want, err := core.SignedAmount(tc.normalSide, tc.entryType, decimal.NewFromInt(100))
			require.NoError(t, err)

			var got decimal.Decimal
			require.NoError(t, pool.QueryRow(ctx,
				"SELECT ledger_signed_amount($1, $2, 100::numeric)", string(tc.normalSide), string(tc.entryType),
			).Scan(&got))

			assert.True(t, want.Equal(got), "core.SignedAmount=%s ledger_signed_amount=%s", want, got)
		})
	}
}

// TestLedgerSignedAmount_RejectsUnknownNormalSide is the SQL-side half of
// the I-42 pin: an unrecognized normal_side must raise, not fall through to
// ELSE 0 (checkpoints.sql's ListComputedBalancesForHolders, pre-migration —
// the shape that silently excluded the entry from the balance) or an
// implicit debit-normal default (checkpoints.sql's ListBalancesAt,
// pre-migration).
func TestLedgerSignedAmount_RejectsUnknownNormalSide(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx, "SELECT ledger_signed_amount('lien', 'debit', 100::numeric)")
	require.Error(t, err)

	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "22023", pgErr.Code, "expected invalid_parameter_value (22023), got %s: %s", pgErr.Code, pgErr.Message)
	assert.Contains(t, pgErr.Message, "unknown normal_side")
}

// TestLedgerSignedAmount_NullEntryTypeIsNoContribution pins the "classification
// exists but has zero matching journal_entries" shape RecomputeCheckpointFromEntries
// and ListComputedBalancesForHolders both rely on: a LEFT JOIN with no match
// passes entry_type/amount as NULL for that row, and ledger_signed_amount
// must report "no contribution" (NULL, so SUM() ignores it and
// COALESCE(SUM(...), 0) yields 0) rather than raising the same error an
// unrecognized normal_side would. This is the regression this task actually
// hit while implementing the fix (TestVerifiedBalance_ZeroContributingJournalsIsDefinedZero
// failed against the first cut of this function, which had no such guard).
func TestLedgerSignedAmount_NullEntryTypeIsNoContribution(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	var got *decimal.Decimal
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT ledger_signed_amount('debit', NULL, NULL::numeric)",
	).Scan(&got))
	assert.Nil(t, got, "NULL entry_type must yield NULL, not an error and not 0-shadowed-as-a-value")

	// And the aggregate shape callers actually rely on: SUM ignores it, and
	// COALESCE(..., 0) turns the whole group into 0.
	var summed decimal.Decimal
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT COALESCE(SUM(ledger_signed_amount('debit', NULL, NULL::numeric)), 0::numeric)",
	).Scan(&summed))
	assert.True(t, summed.IsZero())
}

// TestLedgerSignedDelta_AgreesWithCoreDelta pins the pre-aggregated form
// (reconcile.sql's ReconcileNonNegativeBalances) against core.Delta.
func TestLedgerSignedDelta_AgreesWithCoreDelta(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	debitSum := decimal.NewFromInt(837)
	creditSum := decimal.NewFromInt(419)

	for _, ns := range []core.NormalSide{core.NormalSideDebit, core.NormalSideCredit} {
		want, err := core.Delta(ns, debitSum, creditSum)
		require.NoError(t, err)

		var got decimal.Decimal
		require.NoError(t, pool.QueryRow(ctx,
			"SELECT ledger_signed_delta($1, 837::numeric, 419::numeric)", string(ns),
		).Scan(&got))

		assert.True(t, want.Equal(got), "normal_side=%s core.Delta=%s ledger_signed_delta=%s", ns, want, got)
	}
}

// TestLedgerSignedDelta_RejectsUnknownNormalSide mirrors
// TestLedgerSignedAmount_RejectsUnknownNormalSide for the pre-aggregated
// form.
func TestLedgerSignedDelta_RejectsUnknownNormalSide(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx, "SELECT ledger_signed_delta('lien', 100::numeric, 50::numeric)")
	require.Error(t, err)

	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "22023", pgErr.Code)
}

// TestNormalSideDomain_StructurallyClosedByCheckConstraints backs up the
// migration 009 comment's claim that ledger_signed_amount's ELSE branch is
// unreachable for any row this codebase can produce: classifications.normal_side
// and journal_entries.entry_type both carry a CHECK (... IN ('debit',
// 'credit')) constraint (001_baseline.up.sql :169/:220/:331), so a third
// value can never enter either column in the first place. This is why the
// SQL-side rejection can afford to be a defense-in-depth backstop (a
// LANGUAGE plpgsql RAISE, deliberately kept off the common CASE branches so
// the surrounding SELECT stays inlinable — see the migration's comment)
// rather than a hot-path concern: the domain is closed one layer below it.
func TestNormalSideDomain_StructurallyClosedByCheckConstraints(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	t.Run("classifications.normal_side", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			"INSERT INTO classifications (uid, code, name, normal_side, is_system) VALUES (gen_random_uuid(), 'i42_bad_cls', 'Bad', 'lien', false)")
		require.Error(t, err)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "23514", pgErr.Code, "expected check_violation (23514), got %s: %s", pgErr.Code, pgErr.Message)
	})

	t.Run("journal_entries.entry_type", func(t *testing.T) {
		cur := postgrestest.SeedCurrency(t, pool, "I42BADENTRY", "I-42 bad entry currency")
		cls := postgrestest.SeedClassification(t, pool, "i42_bad_entry_cls", "Bad Entry Cls", "debit", false)
		jt := postgrestest.SeedJournalType(t, pool, "i42_bad_entry_jt", "I-42 bad entry journal type")
		curID := postgrestest.InternalID(t, pool, "currencies", cur)
		clsID := postgrestest.InternalID(t, pool, "classifications", cls)
		jtID := postgrestest.InternalID(t, pool, "journal_types", jt)

		// A minimal, otherwise-valid parent journal — journal_entries.journal_id
		// is a NOT NULL FK, so a row must exist to isolate entry_type as the
		// only thing under test.
		var journalID int64
		require.NoError(t, pool.QueryRow(ctx,
			`INSERT INTO journals (uid, journal_type_id, idempotency_key, total_debit, total_credit)
			 VALUES (gen_random_uuid(), $1, $2, 1, 1) RETURNING id`,
			jtID, postgrestest.UniqueKey("i42-bad-entry-journal"),
		).Scan(&journalID))

		_, err := pool.Exec(ctx,
			`INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, effective_at)
			 VALUES ($1, 9999, $2, $3, 'hold', 1, now())`, journalID, curID, clsID)
		require.Error(t, err)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "23514", pgErr.Code, "expected check_violation (23514), got %s: %s", pgErr.Code, pgErr.Message)
	})
}
