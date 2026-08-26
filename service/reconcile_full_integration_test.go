package service_test

import (
	"context"
	"fmt"
	"math"
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
// failing the test if it's missing (every check in the suite must always be
// present, so a missing check is itself a bug).
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

// TestFullReconciliation_Check2ResumesAcrossRuns is the DB-backed pin for
// C4b: the persisted resume cursor. Three (holder, currency) pairs exist;
// Check2ScanLimit=1 forces one run per pair. Run 1 finds a drift and stops
// (cursor persists at holder A, lap_dirty=true); run 2 resumes at holder B
// (clean) and still can't complete (cursor persists at holder B,
// lap_dirty=true carried forward); run 3 resumes at holder C (clean) and
// completes the lap -- and must still report Passed=false, because holder
// A's violation was found two runs ago in the SAME lap. On completion the
// cursor and lap_dirty reset in the DB.
func TestFullReconciliation_Check2ResumesAcrossRuns(t *testing.T) {
	pgpool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rollup := postgres.NewRollupAdapter(pgpool)
	reconcileAdapter := postgres.NewReconcileAdapter(pgpool)

	currencyUID := postgrestest.SeedCurrency(t, pgpool, "USDT", "Tether Resume")
	classUID := postgrestest.SeedClassification(t, pgpool, "wallet_resume", "Wallet Resume", "debit", false)
	sysClassUID := postgrestest.SeedClassification(t, pgpool, "custodial_resume", "Custodial Resume", "credit", true)
	jtUID := postgrestest.SeedJournalType(t, pgpool, "resume_deposit", "Resume Deposit")

	currencyID := postgrestest.InternalID(t, pgpool, "currencies", currencyUID)
	classID := postgrestest.InternalID(t, pgpool, "classifications", classUID)

	engine := core.NewEngine()
	rollupSvc := service.NewRollupService(rollup, rollup, rollup, rollup, engine)

	// Three holders, ascending, on the SAME (currency, classification) pair.
	// Only the positive (user) side is materialized here -- the negative
	// system side is irrelevant to this test's pagination-ordering point.
	holderA, holderB, holderC := int64(9401), int64(9402), int64(9403)
	for _, h := range []int64{holderA, holderB, holderC} {
		seedJournal(t, pgpool, jtUID, h, currencyUID, classUID, sysClassUID,
			decimal.NewFromInt(100), time.Now(), postgrestest.UniqueKey("resume-dep"))
		require.NoError(t, rollup.EnqueueRollup(ctx, h, currencyID, classID))
	}
	processed, err := rollupSvc.ProcessBatch(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 3, processed)

	// Corrupt holder A's checkpoint only.
	_, err = pgpool.Exec(ctx,
		"UPDATE balance_checkpoints SET balance = balance + 50 WHERE account_holder=$1 AND currency_id=$2 AND classification_id=$3",
		holderA, currencyID, classID,
	)
	require.NoError(t, err)

	basic := service.NewReconciliationService(rollup, rollup, rollup, rollup, engine)
	full := service.NewFullReconciliationService(basic, reconcileAdapter, service.FullReconciliationConfig{
		Check2ScanLimit: 1,
	}, engine)

	// Run 1: scans holder A only -- drift found, lap incomplete.
	report, err := full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	check2 := findCheck(t, report, "checkpoint_balance")
	assert.False(t, check2.Passed, "run 1 must catch holder A's drift")
	assert.False(t, check2.Complete, "run 1 is capped at 1 pair; two pairs remain")

	var afterHolder, afterCurrency int64
	var lapDirty bool
	require.NoError(t, pgpool.QueryRow(ctx,
		"SELECT after_holder, after_currency, lap_dirty FROM reconcile_scan_cursors WHERE check_name='checkpoint_balance'",
	).Scan(&afterHolder, &afterCurrency, &lapDirty))
	assert.Equal(t, holderA, afterHolder, "cursor must persist at the last pair actually scanned")
	assert.True(t, lapDirty, "the drift found this run must be persisted for the next run to carry forward")

	// Run 2: resumes at holder B (clean) -- still incomplete (holder C
	// remains), and must still report Passed=false (lap_dirty carried).
	report, err = full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	check2 = findCheck(t, report, "checkpoint_balance")
	assert.False(t, check2.Passed, "an earlier segment's drift must not be buried by run 2's clean slice")
	assert.False(t, check2.Complete, "holder C has not been scanned yet")

	require.NoError(t, pgpool.QueryRow(ctx,
		"SELECT after_holder, after_currency, lap_dirty FROM reconcile_scan_cursors WHERE check_name='checkpoint_balance'",
	).Scan(&afterHolder, &afterCurrency, &lapDirty))
	assert.Equal(t, holderB, afterHolder, "run 2 must resume from where run 1 stopped, not restart at the top")
	assert.True(t, lapDirty)

	// Run 3: resumes at holder C (clean) and scans exactly Check2ScanLimit
	// (1) pairs again. Note: when the scan limit is hit on a page that
	// happens to exactly exhaust the remaining pairs, this check cannot
	// locally distinguish "capped" from "coincidentally the last pair" (a
	// pre-existing ambiguity in the scan-limit bookkeeping, not something
	// this fix changes) -- it reports incomplete either way. Passed must
	// still be false regardless (holder A's drift, found two runs ago).
	report, err = full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	check2 = findCheck(t, report, "checkpoint_balance")
	assert.False(t, check2.Passed, "the earlier drift must not be buried by run 3's clean slice")

	require.NoError(t, pgpool.QueryRow(ctx,
		"SELECT after_holder, after_currency, lap_dirty FROM reconcile_scan_cursors WHERE check_name='checkpoint_balance'",
	).Scan(&afterHolder, &afterCurrency, &lapDirty))
	assert.Equal(t, holderC, afterHolder)
	assert.True(t, lapDirty)

	// Run 4: resumes past holder C. Zero pairs remain, so the lap ends here
	// either way -- but this run started from a resumed (non-fresh) cursor
	// and found nothing at all, which is EXACTLY the shape a tampered cursor
	// produces (threat-model.md's §4-3 Major:
	// `UPDATE reconcile_scan_cursors SET after_holder = <huge>, lap_dirty =
	// false` makes every page query return zero rows, indistinguishable
	// on the wire from a lap that legitimately finished at that exact
	// point). Go alone cannot tell "genuinely reached the tail" apart from
	// "a cursor moved there by something other than this scan" without an
	// independent count -- so this run reports Complete=false rather than
	// asserting coverage it cannot actually vouch for. The cursor and
	// lap_dirty still reset below: a legitimately-finished lap self-corrects
	// on the very next (now fresh) run at the cost of one conservative
	// under-claim, which is cheap compared to the alternative (an attacker
	// who resets the cursor before every scheduled run keeping this check
	// permanently green).
	report, err = full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	check2 = findCheck(t, report, "checkpoint_balance")
	assert.False(t, check2.Passed, "the run that completes the lap must still surface the earlier drift")
	assert.False(t, check2.Complete, "a resumed cursor finding zero pairs must not claim coverage it cannot vouch for")

	require.NoError(t, pgpool.QueryRow(ctx,
		"SELECT after_holder, after_currency, lap_dirty FROM reconcile_scan_cursors WHERE check_name='checkpoint_balance'",
	).Scan(&afterHolder, &afterCurrency, &lapDirty))
	assert.Equal(t, int64(math.MinInt64), afterHolder, "a completed lap must reset the cursor for the next lap")
	assert.Equal(t, int64(math.MinInt64), afterCurrency)
	assert.False(t, lapDirty, "lap_dirty must reset once the lap completes")
}

// TestFullReconciliation_DetectsSystemRollupDriftFromPoisonedCheckpoint
// is the DB-backed pin for M4/I-23's headline claim: system_rollups is
// derived FROM balance_checkpoints (RefreshSystemRollups ->
// AggregateCheckpointsByClassification), so it inherits checkpoint tampering
// wholesale. Comparing system_rollups only against itself (or against the
// checkpoints it was built from) would never catch this. Check #11 compares
// it against journal_entries directly and must catch it.
func TestFullReconciliation_DetectsSystemRollupDriftFromPoisonedCheckpoint(t *testing.T) {
	pgpool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rollup := postgres.NewRollupAdapter(pgpool)
	reconcileAdapter := postgres.NewReconcileAdapter(pgpool)

	currencyUID := postgrestest.SeedCurrency(t, pgpool, "USDT", "Tether SysRollup")
	classUID := postgrestest.SeedClassification(t, pgpool, "wallet_sr", "Wallet SysRollup", "debit", false)
	sysClassUID := postgrestest.SeedClassification(t, pgpool, "custodial_sr", "Custodial SysRollup", "credit", true)
	jtUID := postgrestest.SeedJournalType(t, pgpool, "sr_deposit", "SysRollup Deposit")

	currencyID := postgrestest.InternalID(t, pgpool, "currencies", currencyUID)
	classID := postgrestest.InternalID(t, pgpool, "classifications", classUID)
	holderID := int64(9500)

	seedJournal(t, pgpool, jtUID, holderID, currencyUID, classUID, sysClassUID,
		decimal.NewFromInt(300), time.Now(), postgrestest.UniqueKey("sr-dep"))
	require.NoError(t, rollup.EnqueueRollup(ctx, holderID, currencyID, classID))

	engine := core.NewEngine()
	rollupSvc := service.NewRollupService(rollup, rollup, rollup, rollup, engine)
	processed, err := rollupSvc.ProcessBatch(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 1, processed)

	// Populate system_rollups from the (currently correct) checkpoint.
	sysRollupSvc := service.NewSystemRollupService(rollup, rollup, engine)
	require.NoError(t, sysRollupSvc.RefreshSystemRollups(ctx))

	basic := service.NewReconciliationService(rollup, rollup, rollup, rollup, engine)
	full := service.NewFullReconciliationService(basic, reconcileAdapter, service.FullReconciliationConfig{}, engine)

	report, err := full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	require.True(t, findCheck(t, report, "system_rollup_integrity").Passed, "must be clean before tampering")

	// Poison the checkpoint, then refresh system_rollups from it -- this is
	// the exact mechanism by which system_rollups inherits checkpoint
	// tampering (RefreshSystemRollups sums balance_checkpoints, not entries).
	_, err = pgpool.Exec(ctx,
		"UPDATE balance_checkpoints SET balance = balance + 250 WHERE account_holder=$1 AND currency_id=$2 AND classification_id=$3",
		holderID, currencyID, classID,
	)
	require.NoError(t, err)
	require.NoError(t, sysRollupSvc.RefreshSystemRollups(ctx))

	report, err = full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	check11 := findCheck(t, report, "system_rollup_integrity")
	assert.False(t, check11.Passed, "system_rollups drift inherited from a poisoned checkpoint must be caught against entries directly")
	var driftFound bool
	for _, f := range check11.Findings {
		if strings.Contains(f.Detail, "250") {
			driftFound = true
		}
	}
	assert.True(t, driftFound, "got: %+v", check11.Findings)
}

// TestFullReconciliation_DetectsSnapshotDrift is the DB-backed pin for
// M4/I-23's balance_snapshots half: a snapshot row tampered independently of
// journal_entries must be caught by an entries-based recompute, not by
// comparing the snapshot only against itself.
func TestFullReconciliation_DetectsSnapshotDrift(t *testing.T) {
	pgpool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rollup := postgres.NewRollupAdapter(pgpool)
	reconcileAdapter := postgres.NewReconcileAdapter(pgpool)

	currencyUID := postgrestest.SeedCurrency(t, pgpool, "USDT", "Tether Snap")
	classUID := postgrestest.SeedClassification(t, pgpool, "wallet_snap", "Wallet Snap", "debit", false)
	sysClassUID := postgrestest.SeedClassification(t, pgpool, "custodial_snap", "Custodial Snap", "credit", true)
	jtUID := postgrestest.SeedJournalType(t, pgpool, "snap_deposit", "Snap Deposit")

	holderID := int64(9600)
	today := time.Now().UTC()
	seedJournal(t, pgpool, jtUID, holderID, currencyUID, classUID, sysClassUID,
		decimal.NewFromInt(400), today, postgrestest.UniqueKey("snap-dep"))

	engine := core.NewEngine()
	snapshotSvc := service.NewSnapshotService(rollup, rollup, engine)
	require.NoError(t, snapshotSvc.CreateDailySnapshot(ctx, today))

	basic := service.NewReconciliationService(rollup, rollup, rollup, rollup, engine)
	full := service.NewFullReconciliationService(basic, reconcileAdapter, service.FullReconciliationConfig{}, engine)

	report, err := full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	require.True(t, findCheck(t, report, "snapshot_integrity").Passed, "must be clean before tampering")

	classID := postgrestest.InternalID(t, pgpool, "classifications", classUID)
	currencyID := postgrestest.InternalID(t, pgpool, "currencies", currencyUID)
	snapshotDate := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	_, err = pgpool.Exec(ctx,
		"UPDATE balance_snapshots SET balance = balance + 150 WHERE account_holder=$1 AND currency_id=$2 AND classification_id=$3 AND snapshot_date=$4",
		holderID, currencyID, classID, snapshotDate,
	)
	require.NoError(t, err)

	report, err = full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	check12 := findCheck(t, report, "snapshot_integrity")
	assert.False(t, check12.Passed, "a tampered snapshot must be caught against a fresh entries-based recompute")
	var driftFound bool
	for _, f := range check12.Findings {
		if strings.Contains(f.Description, "holder 9600") && strings.Contains(f.Detail, "150") {
			driftFound = true
		}
	}
	assert.True(t, driftFound, "got: %+v", check12.Findings)
}

// TestFullReconciliation_RoleLessLiability_DetectsMistaggedClassification
// is the DB-backed pin for the M-4 fix (`.local/independent-review-2026-08-26.md`,
// docs/plans/2026-08-26-audit-remediation-contracts.md follow-on
// fix-backend-1 batch, board #43): GetTotalUserSideBalance (I-37) only sums
// liability from classifications tagged with a non-empty balance_role.
// Nothing enforced that a credit-normal, non-system classification actually
// carries one -- the independent review's strongest evidence for this being
// a real, recurring shape (not a theoretical worry) was that commit
// `6c83236` had to fix three pre-existing test fixtures that built their own
// "liability" classifications without a balance_role. This test reproduces
// exactly that shape against real Postgres and real journal_entries.
func TestFullReconciliation_RoleLessLiability_DetectsMistaggedClassification(t *testing.T) {
	pgpool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rollup := postgres.NewRollupAdapter(pgpool)
	reconcileAdapter := postgres.NewReconcileAdapter(pgpool)

	currencyUID := postgrestest.SeedCurrency(t, pgpool, "USDT", "Tether M4")
	jtUID := postgrestest.SeedJournalType(t, pgpool, "m4_deposit", "M4 Deposit")

	// The mistagged classification this check exists to catch: credit-normal
	// (liability-shaped -- posted to a positive/user holder), non-system, and
	// -- critically -- SeedClassification (unlike SeedClassificationWithRole)
	// leaves balance_role at its column default (''), the exact "forgot to
	// tag it" shape.
	mistaggedUID := postgrestest.SeedClassification(t, pgpool, "loyalty_points_m4", "Loyalty Points M4", "credit", false)
	// Its balancing system-side counterpart -- debit-normal, is_system=true,
	// arbitrary for this test's purposes (only the mistagged leg's exclusion
	// from Liability is under test).
	counterpartUID := postgrestest.SeedClassification(t, pgpool, "unbacked_m4", "Unbacked M4", "debit", true)

	// Legitimate, correctly-tagged liability classification (main_wallet
	// shape) -- must NOT be flagged (b-direction: no false positive on a
	// correctly-configured deployment).
	walletUID := postgrestest.SeedClassificationWithRole(t, pgpool, "main_wallet_m4", "Main Wallet M4", "debit", false, "available")
	custodialUID := postgrestest.SeedClassification(t, pgpool, "custodial_m4", "Custodial M4", "credit", true)

	holderA := int64(9700) // holds the mistagged liability
	holderB := int64(9701) // holds the legitimate, correctly-tagged wallet

	// holderA: CREDIT mistagged (100) / DEBIT counterpart (100) -- a nonzero
	// balance on the role-less credit-normal classification.
	tx, err := pgpool.Begin(ctx)
	require.NoError(t, err)
	var jID int64
	require.NoError(t, tx.QueryRow(ctx,
		`INSERT INTO journals (uid, journal_type_id, idempotency_key, total_debit, total_credit, actor_id, source, created_at, effective_at)
		 VALUES (gen_random_uuid(), (SELECT id FROM journal_types WHERE uid=$1::uuid), $2, 100, 100, 0, 'test', now(), now()) RETURNING id`,
		jtUID, postgrestest.UniqueKey("m4-mistagged")).Scan(&jID))
	_, err = tx.Exec(ctx,
		`INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at, effective_at)
		 VALUES ($1,$2,(SELECT id FROM currencies WHERE uid=$3::uuid),(SELECT id FROM classifications WHERE uid=$4::uuid),'credit',100,now(),now())`,
		jID, holderA, currencyUID, mistaggedUID)
	require.NoError(t, err)
	_, err = tx.Exec(ctx,
		`INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at, effective_at)
		 VALUES ($1,$2,(SELECT id FROM currencies WHERE uid=$3::uuid),(SELECT id FROM classifications WHERE uid=$4::uuid),'debit',100,now(),now())`,
		jID, -holderA, currencyUID, counterpartUID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	// holderB: legitimate deposit shape -- DEBIT main_wallet (200, role
	// "available") / CREDIT custodial (200, system, no role expected -- it is
	// is_system, excluded from GetTotalUserSideBalance's active CTE by the
	// account_holder > 0 filter regardless).
	seedJournal(t, pgpool, jtUID, holderB, currencyUID, walletUID, custodialUID, decimal.NewFromInt(200), time.Now(), postgrestest.UniqueKey("m4-legit"))

	eng := core.NewEngine()
	basic := service.NewReconciliationService(rollup, rollup, rollup, rollup, eng)
	full := service.NewFullReconciliationService(basic, reconcileAdapter, service.FullReconciliationConfig{}, eng)

	report, err := full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	check := findCheck(t, report, "role_less_liability")
	assert.False(t, check.Passed, "the mistagged classification's nonzero balance must be caught")

	var flagged bool
	for _, f := range check.Findings {
		if strings.Contains(f.Description, fmt.Sprintf("holder %d", holderA)) && strings.Contains(f.Description, "no balance_role") {
			flagged = true
			assert.Contains(t, f.Detail, "100")
		}
		// b-direction: holderB's correctly-tagged main_wallet must never
		// appear in this check's findings.
		assert.NotContains(t, f.Description, fmt.Sprintf("holder %d", holderB),
			"a correctly role-tagged classification must not be flagged")
	}
	assert.True(t, flagged, "got: %+v", check.Findings)

	// The same scenario's SolvencyReport is the actual consequence this
	// check exists to make visible: holderA's 100 is invisible to
	// SolvencyReport.Liability (I-37's balance_role filter), which is the
	// silent understatement this check now surfaces as a Finding instead.
	pbStore := postgres.NewPlatformBalanceStore(pgpool)
	solvency, err := pbStore.SolvencyCheck(ctx, currencyUID)
	require.NoError(t, err)
	assert.True(t, solvency.Liability.Equal(decimal.NewFromInt(200)),
		"Liability must reflect ONLY holderB's correctly-tagged 200 -- holderA's 100 stays invisible to SolvencyReport by design (I-37), which is exactly why this reconcile check exists as an independent safety net: got %s", solvency.Liability)
}

// TestFullReconciliation_RoleLessLiability_ExplicitMemoIsNotFlagged is the
// other b-direction non-regression, corrected after Team Lead's finding that
// an earlier version of this fix filtered on normal_side='credit': this
// library's own convention has real liabilities on BOTH sides (main_wallet
// is debit-normal), so normal_side cannot distinguish a mistagged liability
// from a legitimate memo account -- only balance_role can. A non-system,
// user-side classification EXPLICITLY tagged BalanceRoleMemo (the
// fee_expense shape, debit-normal, a real per-user cost account, never a
// liability -- I-37) must never be flagged by this check, regardless of
// normal_side, because "balance_role = ”" (not "debit-normal") is this
// check's only trigger.
func TestFullReconciliation_RoleLessLiability_ExplicitMemoIsNotFlagged(t *testing.T) {
	pgpool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rollup := postgres.NewRollupAdapter(pgpool)
	reconcileAdapter := postgres.NewReconcileAdapter(pgpool)

	currencyUID := postgrestest.SeedCurrency(t, pgpool, "USDT", "Tether M4Memo")
	jtUID := postgrestest.SeedJournalType(t, pgpool, "m4_fee", "M4 Fee")

	// fee_expense shape: debit-normal, non-system, EXPLICITLY tagged memo --
	// a legitimate cost account, never a liability (I-37). Seeded via
	// SeedClassificationWithRole so balance_role='memo' is not empty, unlike
	// the mistagged classification in the DetectsMistaggedClassification
	// test above.
	feeUID := postgrestest.SeedClassificationWithRole(t, pgpool, "fee_expense_m4", "Fee Expense M4", "debit", false, "memo")
	revenueUID := postgrestest.SeedClassification(t, pgpool, "fee_revenue_m4", "Fee Revenue M4", "credit", true)

	holder := int64(9702)
	seedJournal(t, pgpool, jtUID, holder, currencyUID, feeUID, revenueUID, decimal.NewFromInt(5), time.Now(), postgrestest.UniqueKey("m4-fee"))

	eng := core.NewEngine()
	basic := service.NewReconciliationService(rollup, rollup, rollup, rollup, eng)
	full := service.NewFullReconciliationService(basic, reconcileAdapter, service.FullReconciliationConfig{}, eng)

	report, err := full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	check := findCheck(t, report, "role_less_liability")
	assert.True(t, check.Passed, "an explicitly memo-tagged classification must never be flagged: got %+v", check.Findings)
}

// TestFullReconciliation_RoleLessLiability_UntaggedDebitNormalIsFlagged pins
// the exact gap Team Lead's finding closed: a role-less DEBIT-normal
// classification with a nonzero balance -- the main_wallet shape, this
// library's canonical REAL liability -- must be flagged. An earlier version
// of this fix filtered on normal_side='credit' and missed this entirely; a
// classification built by copying main_wallet's shape but forgetting its
// balance_role reproduces precisely that miss.
func TestFullReconciliation_RoleLessLiability_UntaggedDebitNormalIsFlagged(t *testing.T) {
	pgpool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rollup := postgres.NewRollupAdapter(pgpool)
	reconcileAdapter := postgres.NewReconcileAdapter(pgpool)

	currencyUID := postgrestest.SeedCurrency(t, pgpool, "USDT", "Tether M4DebitGap")
	jtUID := postgrestest.SeedJournalType(t, pgpool, "m4_debitgap_deposit", "M4 Debit Gap Deposit")

	// main_wallet shape, copied without its balance_role: debit-normal,
	// non-system, balance_role='' -- seeded via raw SQL (SeedClassification),
	// bypassing ClassificationInput.Validate, to model data that predates
	// the M-4 fix or was written directly (the exact ambiguity this check
	// exists to surface for already-existing data).
	mistaggedUID := postgrestest.SeedClassification(t, pgpool, "copied_wallet_m4", "Copied Wallet M4", "debit", false)
	custodialUID := postgrestest.SeedClassification(t, pgpool, "custodial_m4dg", "Custodial M4DG", "credit", true)

	holder := int64(9703)
	seedJournal(t, pgpool, jtUID, holder, currencyUID, mistaggedUID, custodialUID, decimal.NewFromInt(300), time.Now(), postgrestest.UniqueKey("m4-debitgap"))

	eng := core.NewEngine()
	basic := service.NewReconciliationService(rollup, rollup, rollup, rollup, eng)
	full := service.NewFullReconciliationService(basic, reconcileAdapter, service.FullReconciliationConfig{}, eng)

	report, err := full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	check := findCheck(t, report, "role_less_liability")
	assert.False(t, check.Passed, "a role-less DEBIT-normal classification with a real balance must be flagged -- this is exactly what the credit-only filter missed")

	var flagged bool
	for _, f := range check.Findings {
		if strings.Contains(f.Description, fmt.Sprintf("holder %d", holder)) && strings.Contains(f.Description, "no balance_role") {
			flagged = true
			assert.Contains(t, f.Detail, "300")
			assert.Contains(t, f.Detail, "normal_side=debit")
		}
	}
	assert.True(t, flagged, "got: %+v", check.Findings)

	// Confirms the actual consequence: SolvencyCheck.Liability stays blind
	// to this holder's real 300 balance.
	pbStore := postgres.NewPlatformBalanceStore(pgpool)
	solvency, err := pbStore.SolvencyCheck(ctx, currencyUID)
	require.NoError(t, err)
	assert.True(t, solvency.Liability.IsZero(),
		"Liability must stay blind to the untagged debit-normal balance -- this is the invisible understatement the check now surfaces: got %s", solvency.Liability)
}
