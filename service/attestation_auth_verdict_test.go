package service_test

// T4 pin tests: attestation-batched authorization verdicts
// (docs/plans/2026-08-21-tamper-evident-ledger-design.md §8 extended,
// docs/plans/2026-08-21-integrity-hardening-contracts.md §W3-B,
// docs/INVARIANTS.md I-33). These exercise the WRITE side (RunAttestBatch
// computing and caching verdicts, root hash v3) and the periodic-verify
// drift check; postgres/attested_auth_pin_test.go exercises the fast READ
// side (postgres.VerifiedBalanceStore trusting a cached verdict).

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/anchordev"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

// TestAttestationService_ComputesAndCachesAuthorizedVerdict pins the
// headline write-side behavior: with a core.AuthVerifier configured (T4
// enabled), RunAttestBatch verifies a genuinely-signed journal's entries
// and persists core.JournalAuthVerdictAuthorized on entry_attestations,
// and the resulting ledger_attestations row is v3 (non-empty
// auth_verdict_digest, root_hash signed under AttestationRootHashV3).
func TestAttestationService_ComputesAndCachesAuthorizedVerdict(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor, verifier, err := ed25519KeyPair(t, "auth-verdict-key-1")
	require.NoError(t, err)
	ledgerStore := postgres.NewLedgerStore(pool).WithAuth(attestor)
	attestStore := postgres.NewAttestationStore(pool)
	attestSvc := service.NewAttestationService(attestStore, attestor, verifier, nil, core.NewEngine())

	journal, err := ledgerStore.PostJournal(ctx, f.journalInput(9901, postgrestest.UniqueKey("auth-verdict-signed")))
	require.NoError(t, err)

	attested, seq, err := attestSvc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, 2, attested)

	var journalID int64
	require.NoError(t, pool.QueryRow(ctx, "SELECT id FROM journals WHERE uid=$1", journal.UID).Scan(&journalID))
	var verdicts []string
	rows, err := pool.Query(ctx, "SELECT auth_verdict FROM entry_attestations ea JOIN journal_entries je ON je.id=ea.entry_id WHERE je.journal_id=$1", journalID)
	require.NoError(t, err)
	for rows.Next() {
		var v string
		require.NoError(t, rows.Scan(&v))
		verdicts = append(verdicts, v)
	}
	rows.Close()
	require.Len(t, verdicts, 2)
	for _, v := range verdicts {
		require.Equal(t, string(core.JournalAuthVerdictAuthorized), v)
	}

	var authVerdictDigest []byte
	require.NoError(t, pool.QueryRow(ctx, "SELECT auth_verdict_digest FROM ledger_attestations WHERE seq=$1", seq).Scan(&authVerdictDigest))
	require.NotEmpty(t, authVerdictDigest, "an AttestationService with a configured AuthVerifier must produce a v3 (non-empty auth_verdict_digest) row")
}

// TestAttestationService_CachesUnauthorizedVerdictForForgedJournal is the
// negative counterpart: a forged (unsigned) journal's entries still get
// P6 coverage (DELETE-detection is orthogonal to P5 authorization -- see
// migration 054's header), but their cached verdict is
// core.JournalAuthVerdictUnauthorized, not silently dropped or upgraded.
func TestAttestationService_CachesUnauthorizedVerdictForForgedJournal(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor, verifier, err := ed25519KeyPair(t, "auth-verdict-key-2")
	require.NoError(t, err)
	attestStore := postgres.NewAttestationStore(pool)
	attestSvc := service.NewAttestationService(attestStore, attestor, verifier, nil, core.NewEngine())

	journalID := insertForgedJournal(t, ctx, pool, f, postgrestest.UniqueKey("auth-verdict-forged"))
	tx := beginWithCleanup(t, ctx, pool)
	debitID, creditID := insertBalancedPairInTx(t, ctx, tx, f, journalID, 9911, 9912)
	require.NoError(t, tx.Commit(ctx))

	attested, _, err := attestSvc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, 2, attested)

	for _, entryID := range []int64{debitID, creditID} {
		var verdict string
		require.NoError(t, pool.QueryRow(ctx, "SELECT auth_verdict FROM entry_attestations WHERE entry_id=$1", entryID).Scan(&verdict))
		require.Equal(t, string(core.JournalAuthVerdictUnauthorized), verdict, "a forged, unsigned journal must be cached as unauthorized, not silently coverage-only")

		var count int
		require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM entry_attestations WHERE entry_id=$1", entryID).Scan(&count))
		require.Equal(t, 1, count, "P6 coverage (DELETE-detection) must still happen for an unauthorized journal -- it is an orthogonal check")
	}
}

// TestAttestationService_NoVerifierConfiguredStaysV2 pins T4's opt-in
// contract: an AttestationService with no core.AuthVerifier configured
// produces the exact v2 shape it would have before T4 existed -- no
// verdicts computed, empty auth_verdict_digest, root_hash signed under
// AttestationRootHashV2 (not V3).
func TestAttestationService_NoVerifierConfiguredStaysV2(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor := newTestAttestor(t, "auth-verdict-key-3")
	attestStore := postgres.NewAttestationStore(pool)
	attestSvc := service.NewAttestationService(attestStore, attestor, nil, nil, core.NewEngine())

	journalID := insertForgedJournal(t, ctx, pool, f, postgrestest.UniqueKey("auth-verdict-no-t4"))
	tx := beginWithCleanup(t, ctx, pool)
	debitID, _ := insertBalancedPairInTx(t, ctx, tx, f, journalID, 9921, 9922)
	require.NoError(t, tx.Commit(ctx))

	_, seq, err := attestSvc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)

	var authVerdictDigest []byte
	require.NoError(t, pool.QueryRow(ctx, "SELECT auth_verdict_digest FROM ledger_attestations WHERE seq=$1", seq).Scan(&authVerdictDigest))
	require.Empty(t, authVerdictDigest, "no AuthVerifier configured -- this row must stay v2, not gratuitously become v3")

	var verdict string
	require.NoError(t, pool.QueryRow(ctx, "SELECT auth_verdict FROM entry_attestations WHERE entry_id=$1", debitID).Scan(&verdict))
	require.Equal(t, string(core.JournalAuthVerdictUnknown), verdict)
}

// TestVerifyLedger_DetectsAuthVerdictDrift is the write-side companion to
// postgres/attested_auth_pin_test.go's read-side pin: it proves
// AuthVerdictDigest is a REAL tamper-evidence mechanism, not a value that
// is written once and never re-checked. A journal cached as Authorized at
// attestation time, whose stored auth_signature is later corrupted
// (an owner-role bypass of the no-arbitrary-update trigger -- this wave's
// standing threat model), must surface as TAMPERED the next time
// service.VerifyLedger's periodic full check runs -- the exact drift
// core.VerifiedBalanceStore's fast, cache-trusting path (correctly) does
// NOT re-derive on every call.
func TestVerifyLedger_DetectsAuthVerdictDrift(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor, verifier, err := ed25519KeyPair(t, "auth-verdict-key-4")
	require.NoError(t, err)
	ledgerStore := postgres.NewLedgerStore(pool).WithAuth(attestor)
	attestStore := postgres.NewAttestationStore(pool)
	anchor := anchordev.NewLocalFileAnchor(filepath.Join(t.TempDir(), "anchor.txt"))
	attestSvc := service.NewAttestationService(attestStore, attestor, verifier, anchor, core.NewEngine())

	journal, err := ledgerStore.PostJournal(ctx, f.journalInput(9931, postgrestest.UniqueKey("auth-verdict-drift")))
	require.NoError(t, err)

	_, seq, err := attestSvc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)
	require.NotZero(t, seq)

	// Simulate the owner-role bypass this wave's whole threat model is
	// built around: disable the append-only guard, corrupt the journal's
	// stored signature (leaving auth_digest/auth_key_id/auth_status and
	// every entry untouched -- content the batch_digest/merkle_root checks
	// alone would NOT catch), re-enable it.
	_, err = pool.Exec(ctx, "ALTER TABLE journals DISABLE TRIGGER journals_no_arbitrary_update")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "UPDATE journals SET auth_signature = decode(repeat('ff', 32), 'hex') WHERE uid = $1", journal.UID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "ALTER TABLE journals ENABLE TRIGGER journals_no_arbitrary_update")
	require.NoError(t, err)

	queries := postgres.NewQueryStore(pool)
	report := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusTampered, report.Status, "reasons: %v", report.Reasons)
	require.Contains(t, report.Reasons, fmt.Sprintf("seq %d: auth_verdict_digest mismatch (a cached journal authorization verdict no longer matches a live recheck)", seq),
		"auth_verdict_digest drift must be one of the reported findings: %v", report.Reasons)
}
