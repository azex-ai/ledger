package service_test

// W3-M4 pins (2026-09-02 adversarial re-review, w3-review/money-path.md M-4).
//
// journals.auth_status is a plain CHECK column (001_baseline §3) with no
// guard trigger, and ledger_app -- the credential this whole threat model
// assumes is leaked -- picks its value on INSERT. Step 3b used to read it as
// a trust signal: `unsigned_tx_mode` meant "legitimate, counted not flagged",
// so the identical forged 1,000,000 journal reported
//
//	auth_status=unsigned_tx_mode     -> DRIFT     ("benign backlog, the next run covers them")
//	auth_status=unsigned_no_attestor -> TAMPERED
//
// The attacker chose which. Worse, the DRIFT was permanent wherever the
// attestation job is not actually running (WithAttestor only wires the
// signer; the batch job is a Worker job), and ledger-cli verify exits 0 on
// DRIFT.

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/anchordev"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

// insertUnverifiableJournalAged inserts a journal claiming authStatus, with no
// signature, effective_at at `age` in the past (negative age = future-dated),
// plus a balanced entry pair. This is what a direct-SQL forgery looks like on
// the wire; the only knob is the auth_status string the attacker picks.
func insertUnverifiableJournalAged(t testing.TB, ctx context.Context, pool *pgxpool.Pool, f attestFixture, idemKey, authStatus string, age time.Duration, holder int64) int64 {
	t.Helper()
	var journalID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit, metadata, actor_id, source, event_id, effective_at, uid, auth_digest, auth_signature, auth_key_id, auth_status)
		VALUES ($1, $2, 1, 1, '{}'::jsonb, 0, 'w3-m4-test', NULL, now() - $3::interval, gen_random_uuid(), ''::bytea, ''::bytea, '', $4)
		RETURNING id
	`, f.journalTypeID, idemKey, age.String(), authStatus).Scan(&journalID))
	tx := beginWithCleanup(t, ctx, pool)
	insertBalancedPairInTx(t, ctx, tx, f, journalID, holder, core.SystemAccountHolder(holder))
	require.NoError(t, tx.Commit(ctx))
	return journalID
}

// TestVerifyLedger_UncoveredTxModeClaimBeyondGraceIsTampered is the attack:
// the exact forgery TestVerifyLedger_FlagsUncoveredUnsignedEntry already
// catches, relabelled `unsigned_tx_mode`, and old enough that any running
// attestation job would have covered it several times over.
func TestVerifyLedger_UncoveredTxModeClaimBeyondGraceIsTampered(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor, verifier, err := ed25519KeyPair(t, "verify-w3m4-key")
	require.NoError(t, err)
	ledgerStore := postgres.NewLedgerStore(pool).WithAuth(attestor)
	attestStore := postgres.NewAttestationStore(pool)
	anchor := anchordev.NewLocalFileAnchorForDevelopment(filepath.Join(t.TempDir(), "anchor.txt"))
	attestSvc := service.NewAttestationService(attestStore, attestor, verifier, anchor, core.NewEngine())

	_, err = ledgerStore.PostJournal(ctx, f.journalInput(4301, postgrestest.UniqueKey("w3m4-legit")))
	require.NoError(t, err)
	_, _, err = attestSvc.RunAttestBatch(ctx, 1000)
	require.NoError(t, err)

	queries := postgres.NewQueryStore(pool)
	baseline := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusVerified, baseline.Status, "baseline reasons: %v", baseline.Reasons)

	insertUnverifiableJournalAged(t, ctx, pool, f, postgrestest.UniqueKey("w3m4-forged"),
		string(core.AuthStatusUnsignedTxMode), time.Hour, 4302)

	report := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusTampered, report.Status,
		"an hour-old uncovered entry whose journal carries no verifiable signature is TAMPERED no matter what auth_status calls itself; report: %+v", report)
	require.EqualValues(t, 2, report.UncoveredEntries)
	require.Contains(t, fmt.Sprint(report.Reasons), "no valid authorization")
}

// TestVerifyLedger_UncoveredTxModeWithinGraceIsDriftWithACount is the benign
// half: a genuinely-just-written tx-mode journal (RunInTx posts these -- there
// is no safe point to call a signer inside a caller's transaction) is a
// backlog the next attestation run closes. It stays DRIFT, but the number of
// entries this run could not speak for has to be in the output.
func TestVerifyLedger_UncoveredTxModeWithinGraceIsDriftWithACount(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor, verifier, err := ed25519KeyPair(t, "verify-w3m4-grace-key")
	require.NoError(t, err)
	ledgerStore := postgres.NewLedgerStore(pool).WithAuth(attestor)
	attestStore := postgres.NewAttestationStore(pool)
	anchor := anchordev.NewLocalFileAnchorForDevelopment(filepath.Join(t.TempDir(), "anchor.txt"))
	attestSvc := service.NewAttestationService(attestStore, attestor, verifier, anchor, core.NewEngine())

	_, err = ledgerStore.PostJournal(ctx, f.journalInput(4311, postgrestest.UniqueKey("w3m4-grace-legit")))
	require.NoError(t, err)
	_, _, err = attestSvc.RunAttestBatch(ctx, 1000)
	require.NoError(t, err)

	insertUnverifiableJournalAged(t, ctx, pool, f, postgrestest.UniqueKey("w3m4-grace-fresh"),
		string(core.AuthStatusUnsignedTxMode), time.Second, 4312)

	queries := postgres.NewQueryStore(pool)
	report := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusDrift, report.Status, "report: %+v", report)
	require.EqualValues(t, 2, report.UncoveredEntries)
	require.EqualValues(t, 1, report.UncoveredUnverifiedJournals,
		"the count of uncovered journals no signature speaks for is the number an operator watches; report: %+v", report)
	require.Contains(t, fmt.Sprint(report.Reasons), "cannot be verified by signature")
}

// TestVerifyLedger_FutureDatedUncoveredEntryIsTamperedImmediately closes the
// obvious way out of the grace window: effective_at is a column the forger
// writes, so a row dated in the future would sit "younger than one attestation
// interval" forever.
func TestVerifyLedger_FutureDatedUncoveredEntryIsTamperedImmediately(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor, verifier, err := ed25519KeyPair(t, "verify-w3m4-future-key")
	require.NoError(t, err)
	ledgerStore := postgres.NewLedgerStore(pool).WithAuth(attestor)
	attestStore := postgres.NewAttestationStore(pool)
	anchor := anchordev.NewLocalFileAnchorForDevelopment(filepath.Join(t.TempDir(), "anchor.txt"))
	attestSvc := service.NewAttestationService(attestStore, attestor, verifier, anchor, core.NewEngine())

	_, err = ledgerStore.PostJournal(ctx, f.journalInput(4321, postgrestest.UniqueKey("w3m4-future-legit")))
	require.NoError(t, err)
	_, _, err = attestSvc.RunAttestBatch(ctx, 1000)
	require.NoError(t, err)

	insertUnverifiableJournalAged(t, ctx, pool, f, postgrestest.UniqueKey("w3m4-future-forged"),
		string(core.AuthStatusUnsignedTxMode), -365*24*time.Hour, 4322)

	queries := postgres.NewQueryStore(pool)
	report := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusTampered, report.Status,
		"a future-dated uncovered entry can never age out of the grace window, so the window must not apply to it; report: %+v", report)
}
