package service_test

// TestFullReconciliation_Check1DetectsRealGlobalImbalance closes the
// test-credibility.md PLAUSIBLE gap for check #1 ("global_dr_cr_equality",
// service/reconcile.go:478): every existing test for this check
// (service/reconcile_test.go's TestReconciliationService_Imbalanced,
// service/reconcile_full_test.go's mock-based suite) drives it through a
// mocked GlobalSummer, so none of them exercise the REAL
// SumGlobalDebitCreditByCurrency SQL against real Postgres data. Verified:
// grep across service/*_test.go and postgres/*_test.go found no DB-backed
// test for this check at all before this file.
//
// Mirrors postgres/journal_balance_trigger_test.go's established pattern
// (disable the per-currency balance trigger for exactly the crafting
// statement, re-enable immediately) rather than inventing a new one: a
// genuine global imbalance is impossible to produce any other way once
// trg_check_journal_currency_balance is active, since every existing
// journal is individually balanced and a sum of balanced things is
// balanced.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

// withGlobalBalanceTriggerDisabled is the service_test package's copy of
// postgres_test's withBalanceTriggerDisabled (unexported there, so it can't
// be imported directly). Each statement inside fn must run on its own
// connection-level transaction: the trigger is DEFERRABLE INITIALLY
// DEFERRED, firing at COMMIT, so disable/craft/enable inside one transaction
// would still let it fire.
func withGlobalBalanceTriggerDisabled(t *testing.T, pool *pgxpool.Pool, ctx context.Context, fn func()) {
	t.Helper()

	_, err := pool.Exec(ctx, `ALTER TABLE journal_entries DISABLE TRIGGER trg_check_journal_currency_balance`)
	require.NoError(t, err, "disabling the balance trigger is the only way to craft a global imbalance")

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

func TestFullReconciliation_Check1DetectsRealGlobalImbalance(t *testing.T) {
	pgpool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rollup := postgres.NewRollupAdapter(pgpool)
	reconcileAdapter := postgres.NewReconcileAdapter(pgpool)

	currencyUID := postgrestest.SeedCurrency(t, pgpool, "USDT", "Tether Check1")
	classUID := postgrestest.SeedClassification(t, pgpool, "wallet_check1", "Wallet Check1", "debit", false)
	sysClassUID := postgrestest.SeedClassification(t, pgpool, "custodial_check1", "Custodial Check1", "credit", true)
	jtUID := postgrestest.SeedJournalType(t, pgpool, "check1_deposit", "Check1 Deposit")

	currencyID := postgrestest.InternalID(t, pgpool, "currencies", currencyUID)
	classID := postgrestest.InternalID(t, pgpool, "classifications", classUID)

	engine := core.NewEngine()
	basic := service.NewReconciliationService(rollup, rollup, rollup, rollup, engine)
	full := service.NewFullReconciliationService(basic, reconcileAdapter, service.FullReconciliationConfig{}, engine)

	holder := int64(9501)
	journalID := seedJournal(t, pgpool, jtUID, holder, currencyUID, classUID, sysClassUID,
		decimal.NewFromInt(300), time.Now(), postgrestest.UniqueKey("check1-legit"))

	// Sanity: clean before tampering -- the legitimate journal alone must
	// pass the global equality check.
	report, err := full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	check1 := findCheck(t, report, "global_dr_cr_equality")
	assert.True(t, check1.Passed, "a genuinely balanced journal must pass check #1")

	// Craft a REAL global imbalance: an extra, unmatched debit leg with no
	// offsetting credit anywhere. Unlike the per-journal fleet scan's own
	// test fixture (two journals whose drifts cancel), this one does NOT
	// cancel -- it is exactly the disaster class check #1 exists to catch
	// (RUNBOOK.md §1: "the headline disaster").
	withGlobalBalanceTriggerDisabled(t, pgpool, ctx, func() {
		_, err := pgpool.Exec(ctx,
			`INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, effective_at)
			 VALUES ($1, $2, $3, $4, 'debit', $5, now())`,
			journalID, holder, currencyID, classID, decimal.NewFromInt(75),
		)
		require.NoError(t, err, "crafting the corrupted fixture must itself succeed")
	})

	report, err = full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	check1 = findCheck(t, report, "global_dr_cr_equality")
	assert.False(t, check1.Passed, "a real, uncancelled global debit/credit imbalance must fail check #1")
	require.NotEmpty(t, check1.Findings)

	// runCheck1JournalBalance's Finding.Detail format is
	// "debit=%s credit=%s gap=%s" (service/reconcile.go); the crafted
	// imbalance's gap is exactly 75.
	var sawGap bool
	for _, f := range check1.Findings {
		if strings.Contains(f.Detail, "gap=75") {
			sawGap = true
		}
	}
	assert.True(t, sawGap, "expected a finding naming the drift amount; got: %+v", check1.Findings)
	assert.False(t, report.OverallPassed, "a real global imbalance must sink OverallPassed")
}
