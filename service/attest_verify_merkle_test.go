package service_test

// P7 pin tests: RFC 6962 Merkle tree over each attestation batch
// (docs/plans/2026-08-21-tamper-evident-ledger-design.md §9, I-29/I-30,
// docs/plans/2026-08-21-integrity-hardening-contracts.md §9).

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/anchordev"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

// TestVerifyLedger_TamperedMerkleRootAlone is the isolation pin for I-29:
// corrupt ONLY the stored merkle_root (leave entries, batch_digest,
// root_hash, and signature all genuinely untouched) and confirm
// VerifyLedger still reports TAMPERED. This is the specific gap migration
// 048's header discloses -- merkle_root is not one of AttestationRootHash's
// signed inputs, so P6's signature/root_hash checks alone would NOT catch
// this edit -- proving the merkle_root recompute-and-compare in
// service.VerifyLedger is not redundant with what P6 already had.
func TestVerifyLedger_TamperedMerkleRootAlone(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor, verifier, err := ed25519KeyPair(t, "verify-merkle-key-1")
	require.NoError(t, err)
	attestStore := postgres.NewAttestationStore(pool)
	anchor := anchordev.NewLocalFileAnchor(filepath.Join(t.TempDir(), "anchor.txt"))
	attestSvc := service.NewAttestationService(attestStore, attestor, anchor, core.NewEngine())

	journalID := insertForgedJournal(t, ctx, pool, f, postgrestest.UniqueKey("verify-merkle-alone"))
	tx := beginWithCleanup(t, ctx, pool)
	insertBalancedPairInTx(t, ctx, tx, f, journalID, 9501, 9502)
	require.NoError(t, tx.Commit(ctx))

	_, seq, err := attestSvc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)
	require.NotZero(t, seq)

	// Simulate the owner-role bypass this wave's whole threat model is
	// built around: disable the append-only guard, edit merkle_root
	// alone, re-enable it. batch_digest/root_hash/signature are left
	// exactly as RunAttestBatch wrote them.
	_, err = pool.Exec(ctx, "ALTER TABLE ledger_attestations DISABLE TRIGGER ledger_attestations_no_update")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "UPDATE ledger_attestations SET merkle_root = decode(repeat('ff', 32), 'hex') WHERE seq = $1", seq)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "ALTER TABLE ledger_attestations ENABLE TRIGGER ledger_attestations_no_update")
	require.NoError(t, err)

	queries := postgres.NewQueryStore(pool)
	report := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusTampered, report.Status)
	require.Len(t, report.Reasons, 1, "batch_digest/root_hash/signature must all still verify -- only the merkle_root check should fire: %v", report.Reasons)
	require.Contains(t, report.Reasons[0], "merkle_root mismatch")
}

// TestVerifyLedger_MerkleRootCheckSkippedForLegacyEmptySentinel pins the
// other half of I-29: an attestation row that predates merkle_root being
// computed (migration 048's `”::bytea` default -- simulated here by
// inserting through AttestationStore directly with MerkleRoot left at its
// Go zero value, bypassing service.AttestationService.RunAttestBatch,
// which always computes a real one) must NOT be treated as a mismatch.
// Symmetric with I-26's own established treatment of an empty AuthKeyID
// ("never signed" is not "forged").
func TestVerifyLedger_MerkleRootCheckSkippedForLegacyEmptySentinel(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	attestor, verifier, err := ed25519KeyPair(t, "verify-merkle-key-2")
	require.NoError(t, err)
	attestStore := postgres.NewAttestationStore(pool)
	anchor := anchordev.NewLocalFileAnchor(filepath.Join(t.TempDir(), "anchor.txt"))

	entries, err := attestStore.UncoveredEntries(ctx, 100)
	require.NoError(t, err)
	require.Empty(t, entries, "test setup: expected an empty ledger")

	batchDigest, err := core.CanonicalBatchDigest(entries)
	require.NoError(t, err)
	rootHash, err := core.AttestationRootHash(1, core.GenesisRoot, batchDigest, int64(len(entries)))
	require.NoError(t, err)
	signature, keyID, err := attestor.Sign(ctx, rootHash)
	require.NoError(t, err)

	// MerkleRoot deliberately omitted -- the pre-P7 shape.
	_, err = attestStore.InsertAttestation(ctx, core.Attestation{
		Seq: 1, EntryCount: int64(len(entries)), BatchDigest: batchDigest,
		PrevRoot: core.GenesisRoot, RootHash: rootHash, Signature: signature, KeyID: keyID,
	}, nil)
	require.NoError(t, err)
	require.NoError(t, anchor.Publish(ctx, 1, rootHash))

	queries := postgres.NewQueryStore(pool)
	report := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusVerified, report.Status, "reasons: %v", report.Reasons)
}

// TestVerifyLedger_LocalizesTamperedEntryWithReference pins I-30's
// operational half (design doc §9.1): given a content mismatch AND a
// caller-supplied trusted reference for that seq, VerifyLedger narrows the
// TAMPERED verdict to the exact entry id that changed, not just the seq.
func TestVerifyLedger_LocalizesTamperedEntryWithReference(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor, verifier, err := ed25519KeyPair(t, "verify-merkle-key-3")
	require.NoError(t, err)
	attestStore := postgres.NewAttestationStore(pool)
	anchor := anchordev.NewLocalFileAnchor(filepath.Join(t.TempDir(), "anchor.txt"))
	attestSvc := service.NewAttestationService(attestStore, attestor, anchor, core.NewEngine())

	journalID := insertForgedJournal(t, ctx, pool, f, postgrestest.UniqueKey("verify-merkle-localize"))
	tx := beginWithCleanup(t, ctx, pool)
	debitID, creditID := insertBalancedPairInTx(t, ctx, tx, f, journalID, 9601, 9602)
	require.NoError(t, tx.Commit(ctx))

	_, seq, err := attestSvc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)

	// Snapshot the entries exactly as attested -- this is the "trusted
	// reference" (design doc §9.1: a pre-incident backup/replica/PITR;
	// here, a read taken before the tamper below).
	reference, err := attestStore.EntriesForAttestation(ctx, seq)
	require.NoError(t, err)
	require.Len(t, reference, 2)

	// Tamper the debit leg's amount. Both the append-only guard and the
	// per-journal balance trigger (044) must be bypassed -- the same
	// owner-role-bypass pattern TestVerifyLedger_TamperedOnDeletedEntry
	// already uses, because an update that unbalances a journal is
	// exactly what 044 exists to reject.
	_, err = pool.Exec(ctx, "ALTER TABLE journal_entries DISABLE TRIGGER journal_entries_no_update")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "ALTER TABLE journal_entries DISABLE TRIGGER trg_check_journal_currency_balance")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "UPDATE journal_entries SET amount = 999 WHERE id = $1", debitID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "ALTER TABLE journal_entries ENABLE TRIGGER trg_check_journal_currency_balance")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "ALTER TABLE journal_entries ENABLE TRIGGER journal_entries_no_update")
	require.NoError(t, err)

	queries := postgres.NewQueryStore(pool)
	cfg := service.VerifyConfig{
		ReferenceEntries: func(s int64) ([]core.AttestedEntry, bool) {
			if s == seq {
				return reference, true
			}
			return nil, false
		},
	}
	report := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, cfg)
	require.Equal(t, service.VerifyStatusTampered, report.Status)
	require.Equal(t, []int64{debitID}, report.MismatchedEntryIDs[seq], "localization must name exactly the tampered entry, not creditID or the whole seq")
	_ = creditID
}

// TestVerifyLedger_NoLocalizationWithoutReference is the counterpart:
// the exact same tamper, but VerifyConfig.ReferenceEntries is left nil
// (the default -- no operator-supplied snapshot available). TAMPERED must
// still fire (localization is an enhancement, not a precondition for
// detection), but MismatchedEntryIDs must stay empty -- working-agreements
// §3: no fabricated entry list when localization was never attempted.
func TestVerifyLedger_NoLocalizationWithoutReference(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor, verifier, err := ed25519KeyPair(t, "verify-merkle-key-4")
	require.NoError(t, err)
	attestStore := postgres.NewAttestationStore(pool)
	anchor := anchordev.NewLocalFileAnchor(filepath.Join(t.TempDir(), "anchor.txt"))
	attestSvc := service.NewAttestationService(attestStore, attestor, anchor, core.NewEngine())

	journalID := insertForgedJournal(t, ctx, pool, f, postgrestest.UniqueKey("verify-merkle-no-ref"))
	tx := beginWithCleanup(t, ctx, pool)
	debitID, _ := insertBalancedPairInTx(t, ctx, tx, f, journalID, 9701, 9702)
	require.NoError(t, tx.Commit(ctx))

	_, seq, err := attestSvc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, "ALTER TABLE journal_entries DISABLE TRIGGER journal_entries_no_update")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "ALTER TABLE journal_entries DISABLE TRIGGER trg_check_journal_currency_balance")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "UPDATE journal_entries SET amount = 999 WHERE id = $1", debitID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "ALTER TABLE journal_entries ENABLE TRIGGER trg_check_journal_currency_balance")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "ALTER TABLE journal_entries ENABLE TRIGGER journal_entries_no_update")
	require.NoError(t, err)

	queries := postgres.NewQueryStore(pool)
	report := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusTampered, report.Status)
	require.Empty(t, report.MismatchedEntryIDs, "no reference was supplied -- must not fabricate a localization result")
	_ = seq
}
