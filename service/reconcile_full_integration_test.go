package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

// findCheck locates a named CheckResult in a full reconciliation report,
// failing the test if it's missing (every one of the 10 checks must always
// be present, so a missing check is itself a bug).
func findCheck(t *testing.T, report *core.ReconcileReport, name string) core.CheckResult {
	t.Helper()
	for _, c := range report.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q not found in report", name)
	return core.CheckResult{}
}

// TestFullReconciliation_Check2DetectsCheckpointDrift is a regression test for
// the fleet-wide checkpoint-vs-entries scan (check #2, "checkpoint_balance").
// Before this fix, check #2 was a placeholder that always reported Passed:true
// without inspecting any data. This test materializes a real checkpoint via
// the rollup path, corrupts it directly in the database (the class of bug the
// check exists to catch — e.g. a manual UPDATE or a rollup arithmetic bug),
// and asserts the check both detects the drift and reports the check clean
// beforehand.
func TestFullReconciliation_Check2DetectsCheckpointDrift(t *testing.T) {
	pgpool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rollup := postgres.NewRollupAdapter(pgpool)
	classStore := postgres.NewClassificationStore(pgpool)
	reconcileAdapter := postgres.NewReconcileAdapter(pgpool)

	currencyID := postgrestest.SeedCurrency(t, pgpool, "USDT", "Tether USD")
	classID := postgrestest.SeedClassification(t, pgpool, "wallet_c2", "Wallet Check2", "debit", false)
	sysClassID := postgrestest.SeedClassification(t, pgpool, "custodial_c2", "Custodial Check2", "credit", true)
	jtID := postgrestest.SeedJournalType(t, pgpool, "c2_deposit", "Check2 Deposit")
	holderID := int64(9001)

	// Post a journal and materialize its checkpoint via the real rollup path
	// so we start from a genuinely correct checkpoint, not a hand-crafted one.
	seedJournal(t, pgpool, jtID, holderID, currencyID, classID, sysClassID,
		decimal.NewFromInt(500), time.Now(), postgrestest.UniqueKey("c2-dep"))
	require.NoError(t, rollup.EnqueueRollup(ctx, holderID, currencyID, classID))

	engine := core.NewEngine()
	rollupSvc := service.NewRollupService(rollup, rollup, rollup, classStore, engine)
	processed, err := rollupSvc.ProcessBatch(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 1, processed, "rollup must materialize exactly one checkpoint")

	basic := service.NewReconciliationService(rollup, rollup, rollup, classStore, engine)
	full := service.NewFullReconciliationService(basic, reconcileAdapter, service.FullReconciliationConfig{}, engine)

	// Sanity: check #2 passes before we corrupt anything.
	report, err := full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	check2 := findCheck(t, report, "checkpoint_balance")
	assert.True(t, check2.Passed, "checkpoint_balance should pass on an untouched checkpoint")

	// Corrupt the checkpoint directly, independent of journal_entries — this
	// is exactly the class of drift #2 exists to catch (checkpoint no longer
	// matches a full recomputation from entries).
	_, err = pgpool.Exec(ctx,
		"UPDATE balance_checkpoints SET balance = balance + 999 WHERE account_holder=$1 AND currency_id=$2 AND classification_id=$3",
		holderID, currencyID, classID,
	)
	require.NoError(t, err)

	report, err = full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	check2 = findCheck(t, report, "checkpoint_balance")
	assert.False(t, check2.Passed, "checkpoint_balance must catch the injected drift")

	var driftFound bool
	for _, f := range check2.Findings {
		if strings.Contains(f.Description, "checkpoint balance drift") {
			driftFound = true
			assert.Contains(t, f.Detail, "999", "finding detail should surface the drift amount")
		}
	}
	assert.True(t, driftFound, "expected a checkpoint drift finding, got: %+v", check2.Findings)
}

// TestFullReconciliation_Check2ReportsPartialScanOnScanLimit verifies that
// when the configured Check2ScanLimit is smaller than the number of distinct
// (holder, currency) pairs, check #2 explicitly reports the scan as
// incomplete rather than silently claiming full coverage.
func TestFullReconciliation_Check2ReportsPartialScanOnScanLimit(t *testing.T) {
	pgpool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rollup := postgres.NewRollupAdapter(pgpool)
	classStore := postgres.NewClassificationStore(pgpool)
	reconcileAdapter := postgres.NewReconcileAdapter(pgpool)

	currencyID := postgrestest.SeedCurrency(t, pgpool, "USDC", "USD Coin")
	classID := postgrestest.SeedClassification(t, pgpool, "wallet_c2b", "Wallet Check2b", "debit", false)
	sysClassID := postgrestest.SeedClassification(t, pgpool, "custodial_c2b", "Custodial Check2b", "credit", true)
	jtID := postgrestest.SeedJournalType(t, pgpool, "c2b_deposit", "Check2b Deposit")

	engine := core.NewEngine()
	rollupSvc := service.NewRollupService(rollup, rollup, rollup, classStore, engine)

	// Materialize checkpoints for 3 distinct holders.
	for i := int64(1); i <= 3; i++ {
		holderID := 9100 + i
		seedJournal(t, pgpool, jtID, holderID, currencyID, classID, sysClassID,
			decimal.NewFromInt(100), time.Now(), postgrestest.UniqueKey("c2b-dep"))
		require.NoError(t, rollup.EnqueueRollup(ctx, holderID, currencyID, classID))
	}
	processed, err := rollupSvc.ProcessBatch(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 3, processed)

	basic := service.NewReconciliationService(rollup, rollup, rollup, classStore, engine)
	full := service.NewFullReconciliationService(basic, reconcileAdapter, service.FullReconciliationConfig{
		Check2ScanLimit: 2, // fewer than the 3 pairs that exist
	}, engine)

	report, err := full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	check2 := findCheck(t, report, "checkpoint_balance")

	var partialFound bool
	for _, f := range check2.Findings {
		if strings.Contains(f.Description, "checkpoint scan incomplete") {
			partialFound = true
		}
	}
	assert.True(t, partialFound, "capped scan must report itself as incomplete, not silently pass as if fully covered; got: %+v", check2.Findings)
}
