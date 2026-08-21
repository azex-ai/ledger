package service_test

import (
	"context"
	"fmt"
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
// failing the test if it's missing (every one of the 11 checks must always
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
	reconcileAdapter := postgres.NewReconcileAdapter(pgpool)

	currencyUID := postgrestest.SeedCurrency(t, pgpool, "USDT", "Tether USD")
	classUID := postgrestest.SeedClassification(t, pgpool, "wallet_c2", "Wallet Check2", "debit", false)
	sysClassUID := postgrestest.SeedClassification(t, pgpool, "custodial_c2", "Custodial Check2", "credit", true)
	jtUID := postgrestest.SeedJournalType(t, pgpool, "c2_deposit", "Check2 Deposit")
	holderID := int64(9001)

	// Post a journal and materialize its checkpoint via the real rollup path
	// so we start from a genuinely correct checkpoint, not a hand-crafted one.
	seedJournal(t, pgpool, jtUID, holderID, currencyUID, classUID, sysClassUID,
		decimal.NewFromInt(500), time.Now(), postgrestest.UniqueKey("c2-dep"))
	require.NoError(t, rollup.EnqueueRollup(ctx, holderID, postgrestest.InternalID(t, pgpool, "currencies", currencyUID), postgrestest.InternalID(t, pgpool, "classifications", classUID)))

	engine := core.NewEngine()
	rollupSvc := service.NewRollupService(rollup, rollup, rollup, rollup, engine)
	processed, err := rollupSvc.ProcessBatch(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 1, processed, "rollup must materialize exactly one checkpoint")

	basic := service.NewReconciliationService(rollup, rollup, rollup, rollup, engine)
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
		holderID, postgrestest.InternalID(t, pgpool, "currencies", currencyUID), postgrestest.InternalID(t, pgpool, "classifications", classUID),
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
	reconcileAdapter := postgres.NewReconcileAdapter(pgpool)

	currencyUID := postgrestest.SeedCurrency(t, pgpool, "USDC", "USD Coin")
	classUID := postgrestest.SeedClassification(t, pgpool, "wallet_c2b", "Wallet Check2b", "debit", false)
	sysClassUID := postgrestest.SeedClassification(t, pgpool, "custodial_c2b", "Custodial Check2b", "credit", true)
	jtUID := postgrestest.SeedJournalType(t, pgpool, "c2b_deposit", "Check2b Deposit")

	engine := core.NewEngine()
	rollupSvc := service.NewRollupService(rollup, rollup, rollup, rollup, engine)

	// Materialize checkpoints for 3 distinct holders.
	for i := int64(1); i <= 3; i++ {
		holderID := 9100 + i
		seedJournal(t, pgpool, jtUID, holderID, currencyUID, classUID, sysClassUID,
			decimal.NewFromInt(100), time.Now(), postgrestest.UniqueKey("c2b-dep"))
		require.NoError(t, rollup.EnqueueRollup(ctx, holderID, postgrestest.InternalID(t, pgpool, "currencies", currencyUID), postgrestest.InternalID(t, pgpool, "classifications", classUID)))
	}
	processed, err := rollupSvc.ProcessBatch(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 3, processed)

	basic := service.NewReconciliationService(rollup, rollup, rollup, rollup, engine)
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
	assert.False(t, check2.Complete, "a capped scan must not claim full coverage")
	assert.False(t, report.FullCoverage, "one incomplete check must sink the report's coverage signal")
}

// TestFullReconciliation_Check2ScansSystemHolderCheckpoints is the DB-backed pin
// for the keyset cursor fix. Check #2 paginated with `account_holder > cursor`
// starting from zero, so every negative holder — i.e. the entire system /
// custodial side (core.SystemHolder is the negation of the user holder) — was
// excluded on the first page of every run, forever. A forged custodial
// checkpoint is exactly the kind of tampering that hides there, so this test
// corrupts only the system side and requires the check to catch it. Unlike the
// unit test, this exercises the real SQL predicate.
func TestFullReconciliation_Check2ScansSystemHolderCheckpoints(t *testing.T) {
	pgpool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rollup := postgres.NewRollupAdapter(pgpool)
	reconcileAdapter := postgres.NewReconcileAdapter(pgpool)

	currencyUID := postgrestest.SeedCurrency(t, pgpool, "USDT", "Tether")
	classUID := postgrestest.SeedClassification(t, pgpool, "wallet_c2c", "Wallet Check2c", "debit", false)
	sysClassUID := postgrestest.SeedClassification(t, pgpool, "custodial_c2c", "Custodial Check2c", "credit", true)
	jtUID := postgrestest.SeedJournalType(t, pgpool, "c2c_deposit", "Check2c Deposit")

	engine := core.NewEngine()
	rollupSvc := service.NewRollupService(rollup, rollup, rollup, rollup, engine)

	const holderID = int64(9300)
	seedJournal(t, pgpool, jtUID, holderID, currencyUID, classUID, sysClassUID,
		decimal.NewFromInt(100), time.Now(), postgrestest.UniqueKey("c2c-dep"))

	currencyID := postgrestest.InternalID(t, pgpool, "currencies", currencyUID)
	userClassID := postgrestest.InternalID(t, pgpool, "classifications", classUID)
	sysClassID := postgrestest.InternalID(t, pgpool, "classifications", sysClassUID)

	// Materialize checkpoints on BOTH sides of zero.
	require.NoError(t, rollup.EnqueueRollup(ctx, holderID, currencyID, userClassID))
	require.NoError(t, rollup.EnqueueRollup(ctx, -holderID, currencyID, sysClassID))
	processed, err := rollupSvc.ProcessBatch(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 2, processed)

	basic := service.NewReconciliationService(rollup, rollup, rollup, rollup, engine)
	full := service.NewFullReconciliationService(basic, reconcileAdapter, service.FullReconciliationConfig{}, engine)

	// Sanity: clean before tampering.
	report, err := full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	require.True(t, findCheck(t, report, "checkpoint_balance").Passed)

	// Tamper with the SYSTEM-side checkpoint only.
	_, err = pgpool.Exec(ctx,
		"UPDATE balance_checkpoints SET balance = balance + 777 WHERE account_holder=$1 AND currency_id=$2 AND classification_id=$3",
		-holderID, currencyID, sysClassID,
	)
	require.NoError(t, err)

	report, err = full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	check2 := findCheck(t, report, "checkpoint_balance")
	assert.False(t, check2.Passed, "drift on a system (negative) holder must be caught")

	var driftFound bool
	for _, f := range check2.Findings {
		if strings.Contains(f.Description, "checkpoint balance drift") &&
			strings.Contains(f.Description, fmt.Sprintf("holder %d", -holderID)) {
			driftFound = true
			assert.Contains(t, f.Detail, "777")
		}
	}
	assert.True(t, driftFound, "expected drift finding for the system holder, got: %+v", check2.Findings)
}
