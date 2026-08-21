package service_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/anchordev"
	"github.com/azex-ai/ledger/authdev"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

func TestVerifyLedger_VerifiedOnAHealthyChain(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	seedAttestor, verifier, err := ed25519KeyPair(t, "verify-key-1")
	require.NoError(t, err)

	ledgerStore := postgres.NewLedgerStore(pool).WithAuth(seedAttestor)
	attestStore := postgres.NewAttestationStore(pool)
	anchor := anchordev.NewLocalFileAnchor(filepath.Join(t.TempDir(), "anchor.txt"))
	attestSvc := service.NewAttestationService(attestStore, seedAttestor, anchor, core.NewEngine())

	// Post a couple of real, signed journals through the normal write
	// path (not the forged-SQL helper) so step 4's sampling has something
	// genuinely signed to check.
	input1 := f.journalInput(1001, postgrestest.UniqueKey("verify-healthy-1"))
	_, err = ledgerStore.PostJournal(ctx, input1)
	require.NoError(t, err)
	input2 := f.journalInput(1002, postgrestest.UniqueKey("verify-healthy-2"))
	_, err = ledgerStore.PostJournal(ctx, input2)
	require.NoError(t, err)

	_, _, err = attestSvc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)

	queries := postgres.NewQueryStore(pool)
	report := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusVerified, report.Status, "reasons: %v", report.Reasons)
}

func TestVerifyLedger_NotRunWithoutAnchor(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	_, verifier, err := ed25519KeyPair(t, "verify-key-2")
	require.NoError(t, err)
	attestStore := postgres.NewAttestationStore(pool)
	queries := postgres.NewQueryStore(pool)

	report := service.VerifyLedger(ctx, attestStore, nil, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusNotRun, report.Status)
}

func TestVerifyLedger_NotRunWithoutVerifier(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	anchor := anchordev.NewLocalFileAnchor(filepath.Join(t.TempDir(), "anchor.txt"))
	attestStore := postgres.NewAttestationStore(pool)
	queries := postgres.NewQueryStore(pool)

	report := service.VerifyLedger(ctx, attestStore, anchor, nil, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusNotRun, report.Status)
}

// TestVerifyLedger_NotRunWhenAnchorHeadErrors pins the fail-closed red
// line: an unreachable anchor must produce NOT_RUN, never a folded-in
// VERIFIED.
func TestVerifyLedger_NotRunWhenAnchorHeadErrors(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	_, verifier, err := ed25519KeyPair(t, "verify-key-3")
	require.NoError(t, err)
	attestStore := postgres.NewAttestationStore(pool)
	queries := postgres.NewQueryStore(pool)

	report := service.VerifyLedger(ctx, attestStore, alwaysErrorAnchor{}, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusNotRun, report.Status)
}

type alwaysErrorAnchor struct{}

func (alwaysErrorAnchor) Publish(ctx context.Context, seq int64, head []byte) error {
	return errAlwaysFails
}
func (alwaysErrorAnchor) Head(ctx context.Context) (int64, []byte, error) {
	return 0, nil, errAlwaysFails
}

var errAlwaysFails = &staticError{"alwaysErrorAnchor: simulated failure"}

type staticError struct{ msg string }

func (e *staticError) Error() string { return e.msg }

// TestVerifyLedger_TamperedOnBrokenChainLink pins the chain-continuity
// check: directly corrupting a stored prev_root (simulating an owner-role
// UPDATE that bypassed the no-UPDATE trigger, exactly this wave's threat
// model) must surface as TAMPERED, not VERIFIED.
func TestVerifyLedger_TamperedOnBrokenChainLink(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	attestor, verifier, err := ed25519KeyPair(t, "verify-key-4")
	require.NoError(t, err)
	attestStore := postgres.NewAttestationStore(pool)
	anchor := anchordev.NewLocalFileAnchor(filepath.Join(t.TempDir(), "anchor.txt"))
	attestSvc := service.NewAttestationService(attestStore, attestor, anchor, core.NewEngine())

	_, _, err = attestSvc.RunAttestBatch(ctx, 100) // seq 1
	require.NoError(t, err)
	_, seq2, err := attestSvc.RunAttestBatch(ctx, 100) // seq 2
	require.NoError(t, err)

	// Simulate a superuser/owner-role bypass of the no-UPDATE trigger
	// (this wave's whole threat model) by disabling it for one statement.
	_, err = pool.Exec(ctx, "ALTER TABLE ledger_attestations DISABLE TRIGGER ledger_attestations_no_update")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "UPDATE ledger_attestations SET prev_root = decode(repeat('ff', 32), 'hex') WHERE seq = $1", seq2)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "ALTER TABLE ledger_attestations ENABLE TRIGGER ledger_attestations_no_update")
	require.NoError(t, err)

	queries := postgres.NewQueryStore(pool)
	report := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusTampered, report.Status)
	require.NotEmpty(t, report.Reasons)
}

// TestVerifyLedger_TamperedOnDeletedEntry pins the "row deleted" scenario
// P5 cannot detect on its own (design doc §8's whole reason to exist):
// deleting an attested entry must surface as TAMPERED via the
// entry_count/recount mismatch.
func TestVerifyLedger_TamperedOnDeletedEntry(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor, verifier, err := ed25519KeyPair(t, "verify-key-5")
	require.NoError(t, err)
	attestStore := postgres.NewAttestationStore(pool)
	anchor := anchordev.NewLocalFileAnchor(filepath.Join(t.TempDir(), "anchor.txt"))
	attestSvc := service.NewAttestationService(attestStore, attestor, anchor, core.NewEngine())

	// A balanced pair (P3's migration 044 deferred constraint trigger now
	// enforces per-journal, per-currency balance at commit time even for
	// direct-SQL inserts) -- deleting one leg afterward is what actually
	// simulates "a row disappeared", not an unbalanced insert.
	journalID := insertForgedJournal(t, ctx, pool, f, postgrestest.UniqueKey("verify-delete"))
	tx := beginWithCleanup(t, ctx, pool)
	entryID, _ := insertBalancedPairInTx(t, ctx, tx, f, journalID, 9401, core.SystemAccountHolder(9401))
	require.NoError(t, tx.Commit(ctx))

	_, seq, err := attestSvc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)
	require.NotZero(t, seq)

	// Simulate the owner-role deletion this wave's threat model is built
	// around: disable both guards (append-only AND the balance check --
	// deleting one leg of a balanced journal is, by definition, exactly
	// what P3's trigger exists to reject), delete the row, re-enable both.
	_, err = pool.Exec(ctx, "ALTER TABLE journal_entries DISABLE TRIGGER journal_entries_no_delete")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "ALTER TABLE journal_entries DISABLE TRIGGER trg_check_journal_currency_balance")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "DELETE FROM journal_entries WHERE id = $1", entryID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "ALTER TABLE journal_entries ENABLE TRIGGER trg_check_journal_currency_balance")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "ALTER TABLE journal_entries ENABLE TRIGGER journal_entries_no_delete")
	require.NoError(t, err)

	queries := postgres.NewQueryStore(pool)
	report := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusTampered, report.Status)
	require.NotEmpty(t, report.Reasons)
}

// TestVerifyLedger_DriftWhenAnchorIsBehind pins the DRIFT classification:
// the DB chain has advanced past what the anchor has recorded, but
// nothing else is inconsistent -- a benign, catch-up-pending state, not
// tampering.
func TestVerifyLedger_DriftWhenAnchorIsBehind(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	attestor, verifier, err := ed25519KeyPair(t, "verify-key-6")
	require.NoError(t, err)
	attestStore := postgres.NewAttestationStore(pool)
	// No anchor wired into AttestationService -- RunAttestBatch below
	// never publishes, so the anchor (used only for verify) stays empty
	// while the DB chain advances.
	attestSvc := service.NewAttestationService(attestStore, attestor, nil, core.NewEngine())

	_, _, err = attestSvc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)

	anchor := anchordev.NewLocalFileAnchor(filepath.Join(t.TempDir(), "anchor.txt")) // empty
	queries := postgres.NewQueryStore(pool)
	report := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusDrift, report.Status)
}

// ed25519KeyPair returns a fresh Attestor/AuthVerifier pair over a
// randomly generated seed -- test-only key material, matching
// newTestAttestor's precedent in attestation_test.go (which discards the
// verifier half; these tests need both).
func ed25519KeyPair(t testing.TB, keyID string) (core.Attestor, core.AuthVerifier, error) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	return authdev.NewLocalAttestor(priv.Seed(), keyID)
}
