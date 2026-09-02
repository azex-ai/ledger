package service_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/anchordev"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

// TestVerifyLedger_FlagsUncoveredUnsignedEntry pins step 3b (2026-09-02
// audit, tamper-evident.md M-2 / C-M2): design doc §8.4 step 3 requires a
// LEFT JOIN for entries no attestation covers, and it was never
// implemented. Every other check walks the chain and re-derives what the
// chain says it covers, so a row the chain never mentioned was invisible to
// all of them.
//
// Scenario: attest all history, THEN insert a forged journal with real
// entries by direct SQL and never attest again.
//
// Pinned symbol: service.VerifyLedger (step 3b) via
// service.AttestationStore.UncoveredEntries + JournalAuthMaterial.
func TestVerifyLedger_FlagsUncoveredUnsignedEntry(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor, verifier, err := ed25519KeyPair(t, "verify-uncovered-key")
	require.NoError(t, err)

	ledgerStore := postgres.NewLedgerStore(pool).WithAuth(attestor)
	attestStore := postgres.NewAttestationStore(pool)
	anchorPath := filepath.Join(t.TempDir(), "anchor.txt")
	anchor := anchordev.NewLocalFileAnchor(anchorPath)
	attestSvc := service.NewAttestationService(attestStore, attestor, verifier, anchor, core.NewEngine())

	_, err = ledgerStore.PostJournal(ctx, f.journalInput(4001, postgrestest.UniqueKey("verify-uncovered-legit")))
	require.NoError(t, err)
	_, _, err = attestSvc.RunAttestBatch(ctx, 1000)
	require.NoError(t, err)

	// Sanity: with everything covered and the anchor published, the ledger
	// verifies. If this is not VERIFIED the rest of the test proves nothing.
	queries := postgres.NewQueryStore(pool)
	baseline := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusVerified, baseline.Status, "baseline reasons: %v", baseline.Reasons)
	require.Zero(t, baseline.UncoveredEntries)

	// The forgery: a journal row plus a balanced pair of entries, inserted
	// straight into the tables. No attestation covers them, and the journal
	// carries auth_status's default ('unsigned_no_attestor') because it
	// never went through PostJournal.
	journalID := insertForgedJournal(t, ctx, pool, f, postgrestest.UniqueKey("verify-uncovered-forged"))
	tx := beginWithCleanup(t, ctx, pool)
	insertBalancedPairInTx(t, ctx, tx, f, journalID, 4002, core.SystemAccountHolder(4002))
	require.NoError(t, tx.Commit(ctx))

	report := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusTampered, report.Status,
		"an unattested, unsigned entry pair must be TAMPERED; report: %+v", report)
	require.EqualValues(t, 2, report.UncoveredEntries)
	require.Contains(t, fmt.Sprint(report.Reasons), "no attestation covers")
}

// TestVerifyLedger_UncoveredButLegitimateEntriesAreDriftNotVerified pins the
// other half of step 3b: entries posted through the real, signing write path
// after the last attestation batch are a benign backlog -- but they are still
// NOT VERIFIED, because this run cannot testify about entries no signed
// attestation covers.
//
// Pinned symbol: service.VerifyLedger (step 3b) / service.VerifyStatusDrift.
func TestVerifyLedger_UncoveredButLegitimateEntriesAreDriftNotVerified(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor, verifier, err := ed25519KeyPair(t, "verify-backlog-key")
	require.NoError(t, err)

	ledgerStore := postgres.NewLedgerStore(pool).WithAuth(attestor)
	attestStore := postgres.NewAttestationStore(pool)
	anchor := anchordev.NewLocalFileAnchor(filepath.Join(t.TempDir(), "anchor.txt"))
	attestSvc := service.NewAttestationService(attestStore, attestor, verifier, anchor, core.NewEngine())

	_, err = ledgerStore.PostJournal(ctx, f.journalInput(4101, postgrestest.UniqueKey("verify-backlog-1")))
	require.NoError(t, err)
	_, _, err = attestSvc.RunAttestBatch(ctx, 1000)
	require.NoError(t, err)

	// Posted AFTER the batch: legitimately signed, legitimately uncovered.
	_, err = ledgerStore.PostJournal(ctx, f.journalInput(4102, postgrestest.UniqueKey("verify-backlog-2")))
	require.NoError(t, err)

	queries := postgres.NewQueryStore(pool)
	report := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusDrift, report.Status, "report: %+v", report)
	require.EqualValues(t, 2, report.UncoveredEntries)
	require.Contains(t, fmt.Sprint(report.Reasons), "next attestation run covers them")
}

// TestVerifyLedger_EmptyAnchorWithNonEmptyChainIsNotRun pins C-M3's first
// half (tamper-evident.md M-3): with a real attestation chain in the DB and
// the anchor file deleted, Head() answers (0, nil, nil) -- exactly what it
// answers for an anchor nothing was ever published to. Reading that as DRIFT
// ("a benign, expected inconsistency", and exit code 0 in ledger-cli) made
// `rm anchor.txt` a silent way to switch every external check off.
//
// Pinned symbol: service.VerifyLedger / service.VerifyStatusNotRun.
func TestVerifyLedger_EmptyAnchorWithNonEmptyChainIsNotRun(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	attestor, verifier, err := ed25519KeyPair(t, "verify-empty-anchor-key")
	require.NoError(t, err)
	attestStore := postgres.NewAttestationStore(pool)
	anchorPath := filepath.Join(t.TempDir(), "anchor.txt")
	anchor := anchordev.NewLocalFileAnchor(anchorPath)
	attestSvc := service.NewAttestationService(attestStore, attestor, nil, anchor, core.NewEngine())

	_, _, err = attestSvc.RunAttestBatch(ctx, 100) // seq 1, published to the anchor
	require.NoError(t, err)
	_, _, err = attestSvc.RunAttestBatch(ctx, 100) // seq 2
	require.NoError(t, err)

	queries := postgres.NewQueryStore(pool)
	baseline := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusVerified, baseline.Status, "baseline reasons: %v", baseline.Reasons)
	require.EqualValues(t, 2, baseline.AnchorSeq)

	// Erase the anchor. This is the whole attack: one file (or, on the R2
	// carrier, one PutObject with the ledger's own token).
	require.NoError(t, os.Remove(anchorPath))

	report := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.NotEqual(t, service.VerifyStatusDrift, report.Status,
		"an erased anchor must not be classified as a benign catch-up; report: %+v", report)
	require.Equal(t, service.VerifyStatusTampered, report.Status,
		"a prior observation recorded seq 2, so an anchor now reporting 0 is a provable regression; report: %+v", report)
	require.Contains(t, fmt.Sprint(report.Reasons), "regressed")
}

// TestVerifyLedger_EmptyAnchorWithNoPriorObservationIsNotRun is the same
// erasure with NO recorded observation to contradict it (the attestation job
// never successfully read this anchor -- e.g. the chain was built by a
// service with no anchor wired, and verification points at a fresh one).
// "Never published" and "erased" really are indistinguishable then, so the
// verdict is NOT_RUN: fail-closed, not DRIFT, and never VERIFIED.
//
// Pinned symbol: service.VerifyLedger / service.VerifyStatusNotRun.
func TestVerifyLedger_EmptyAnchorWithNoPriorObservationIsNotRun(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	attestor, verifier, err := ed25519KeyPair(t, "verify-no-observation-key")
	require.NoError(t, err)
	attestStore := postgres.NewAttestationStore(pool)
	// No anchor on the service: nothing is ever published, and no
	// observation is ever recorded.
	attestSvc := service.NewAttestationService(attestStore, attestor, nil, nil, core.NewEngine())
	_, _, err = attestSvc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)

	seq, err := attestStore.HighestObservedAnchorSeq(ctx)
	require.NoError(t, err)
	require.Zero(t, seq, "precondition: no anchor observation recorded")

	anchor := anchordev.NewLocalFileAnchor(filepath.Join(t.TempDir(), "anchor.txt")) // empty
	queries := postgres.NewQueryStore(pool)
	report := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusNotRun, report.Status, "report: %+v", report)
	require.Contains(t, fmt.Sprint(report.Reasons), "anchor reports empty")
}

// TestVerifyLedger_DriftOnlyWhenAnchorHasPublishedButLags pins what DRIFT is
// narrowed to: the anchor has published at least one seq and is behind by a
// finite number. This is the case that genuinely self-heals on the next
// catch-up run.
//
// Pinned symbol: service.VerifyLedger / service.VerifyStatusDrift.
func TestVerifyLedger_DriftOnlyWhenAnchorHasPublishedButLags(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	attestor, verifier, err := ed25519KeyPair(t, "verify-lagging-anchor-key")
	require.NoError(t, err)
	attestStore := postgres.NewAttestationStore(pool)
	anchorPath := filepath.Join(t.TempDir(), "anchor.txt")
	anchor := anchordev.NewLocalFileAnchor(anchorPath)

	// First batch WITH an anchor: seq 1 gets published.
	withAnchor := service.NewAttestationService(attestStore, attestor, nil, anchor, core.NewEngine())
	_, _, err = withAnchor.RunAttestBatch(ctx, 100)
	require.NoError(t, err)

	// Second batch WITHOUT one: the DB chain reaches seq 2 while the anchor
	// stays at seq 1 -- a real catch-up backlog.
	withoutAnchor := service.NewAttestationService(attestStore, attestor, nil, nil, core.NewEngine())
	_, _, err = withoutAnchor.RunAttestBatch(ctx, 100)
	require.NoError(t, err)

	queries := postgres.NewQueryStore(pool)
	report := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusDrift, report.Status, "report: %+v", report)
	require.EqualValues(t, 1, report.AnchorSeq)
	require.Contains(t, fmt.Sprint(report.Reasons), "behind the DB chain")
}

// TestVerifyLedger_AnchorRollbackToAnOlderSeqIsTampered pins C-M3's second
// half: a rollback to a LOWER but non-zero seq. On the R2 carrier this is a
// single PutObject with the ledger's own credential (the audit's M-4), and
// without a recorded observation to compare against it looked exactly like a
// lagging anchor.
//
// Pinned symbol: service.VerifyLedger /
// service.AttestationStore.HighestObservedAnchorSeq.
func TestVerifyLedger_AnchorRollbackToAnOlderSeqIsTampered(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	attestor, verifier, err := ed25519KeyPair(t, "verify-rollback-key")
	require.NoError(t, err)
	attestStore := postgres.NewAttestationStore(pool)
	anchorPath := filepath.Join(t.TempDir(), "anchor.txt")
	anchor := anchordev.NewLocalFileAnchor(anchorPath)
	attestSvc := service.NewAttestationService(attestStore, attestor, nil, anchor, core.NewEngine())

	_, seq1, err := attestSvc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)
	_, _, err = attestSvc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)
	_, _, err = attestSvc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)

	queries := postgres.NewQueryStore(pool)
	baseline := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusVerified, baseline.Status, "baseline reasons: %v", baseline.Reasons)

	// Roll the carrier back out of band, the way an attacker with write
	// access to the carrier (but not through Publish) would: rewrite the
	// state file to an older seq. LocalFileAnchor's Publish would refuse
	// this; the file itself has no such protection, which is precisely the
	// property the R2 adapter had too (audit M-4).
	rollback, err := attestStore.ListAttestationsFrom(ctx, seq1, 1)
	require.NoError(t, err)
	require.Len(t, rollback, 1)
	require.NoError(t, writeAnchorFile(anchorPath, rollback[0].Seq, rollback[0].RootHash))

	report := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusTampered, report.Status, "report: %+v", report)
	require.EqualValues(t, 1, report.AnchorSeq)
	require.EqualValues(t, 3, report.LastObservedAnchorSeq)
	require.Contains(t, fmt.Sprint(report.Reasons), "regressed")
}

// writeAnchorFile writes anchordev.LocalFileAnchor's on-disk format
// directly, bypassing Publish -- the out-of-band write an attacker with
// filesystem (or, for the R2 carrier, bucket) access has and the Publish
// API's ordering checks cannot see. Kept in the test file, not in
// anchordev: nothing in production should have a "write any seq" helper.
func writeAnchorFile(path string, seq int64, head []byte) error {
	return os.WriteFile(path, []byte(fmt.Sprintf("%d\n%x\n", seq, head)), 0o600)
}
