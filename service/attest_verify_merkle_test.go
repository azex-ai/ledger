package service_test

// P7 pin tests: RFC 6962 Merkle tree over each attestation batch
// (docs/plans/2026-08-21-tamper-evident-ledger-design.md §9/§9.4, I-29/I-30,
// docs/plans/2026-08-21-integrity-hardening-contracts.md §9).

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

// insertAttestationWithoutLeafHashes mirrors service.AttestationService.
// RunAttestBatch (v2 root hash, a real merkle_root over whatever entries
// are currently uncovered) but calls AttestationStore.InsertAttestation
// directly with leafHashes=nil, producing entry_attestations rows with
// leaf_hash left at its empty-string default. This simulates an
// attestation created before the leaf_hash column was ever wired up on
// the write path -- a real reachable state (migration 048 could ship
// merkle_root and leaf_hash in two separate releases; this repo ships them
// together, but self-contained localization's own availability check
// must not assume that), used by the tests below that need "self-contained
// localization is unavailable for this seq" without also faking v1
// (empty merkle_root).
func insertAttestationWithoutLeafHashes(t testing.TB, ctx context.Context, attestStore *postgres.AttestationStore, attestor core.Attestor) (seq int64, entries []core.AttestedEntry) {
	t.Helper()

	latest, err := attestStore.LatestAttestation(ctx)
	require.NoError(t, err)
	nextSeq := latest.Seq + 1
	prevRoot := latest.RootHash
	if latest.Seq == 0 {
		prevRoot = core.GenesisRoot
	}

	entries, err = attestStore.UncoveredEntries(ctx, 1000)
	require.NoError(t, err)

	batchDigest, err := core.CanonicalBatchDigest(entries)
	require.NoError(t, err)
	merkleTree, err := core.BuildMerkleTree(entries)
	require.NoError(t, err)
	merkleRoot := merkleTree.Root()
	rootHash, err := core.AttestationRootHashV2(nextSeq, prevRoot, batchDigest, merkleRoot, int64(len(entries)))
	require.NoError(t, err)
	signature, keyID, err := attestor.Sign(ctx, rootHash)
	require.NoError(t, err)

	entryIDs := make([]int64, len(entries))
	for i, e := range entries {
		entryIDs[i] = e.EntryID
	}

	result, err := attestStore.InsertAttestation(ctx, core.Attestation{
		Seq: nextSeq, EntryCount: int64(len(entries)), BatchDigest: batchDigest,
		MerkleRoot: merkleRoot, PrevRoot: prevRoot, RootHash: rootHash,
		Signature: signature, KeyID: keyID,
	}, entryIDs, nil, nil) // leafHashes/verdicts nil -> stored as empty placeholders, not the real hashes/verdicts
	require.NoError(t, err)
	return result.Seq, entries
}

// TestVerifyLedger_TamperedMerkleRootAlone pins the headline fix design
// doc §9.4(1) required: because AttestationRootHashV2 binds merkle_root
// into the signed root_hash, corrupting merkle_root alone (leaving
// journal_entries, entry_attestations.leaf_hash, batch_digest,
// signature, and root_hash's own bytes all genuinely untouched) is
// caught by the root_hash self-consistency check -- the exact gap the
// first cut of migration 048 shipped with (merkle_root not attested by
// anything outside the database). Other checks legitimately fire too
// (live entries and stored leaf hashes both still describe the ORIGINAL
// merkle_root, so both now disagree with the corrupted stored value) --
// that overlap is expected defense-in-depth, not something this test
// narrows away.
func TestVerifyLedger_TamperedMerkleRootAlone(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor, verifier, err := ed25519KeyPair(t, "verify-merkle-key-1")
	require.NoError(t, err)
	attestStore := postgres.NewAttestationStore(pool)
	anchor := anchordev.NewLocalFileAnchor(filepath.Join(t.TempDir(), "anchor.txt"))
	attestSvc := service.NewAttestationService(attestStore, attestor, nil, anchor, core.NewEngine())

	journalID := insertForgedJournal(t, ctx, pool, f, postgrestest.UniqueKey("verify-merkle-alone"))
	tx := beginWithCleanup(t, ctx, pool)
	insertBalancedPairInTx(t, ctx, tx, f, journalID, 9501, 9502)
	require.NoError(t, tx.Commit(ctx))

	_, seq, err := attestSvc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)
	require.NotZero(t, seq)

	// Simulate the owner-role bypass this wave's whole threat model is
	// built around: disable the append-only guard, edit merkle_root
	// alone, re-enable it. batch_digest/root_hash/signature/leaf_hash are
	// left exactly as RunAttestBatch wrote them.
	_, err = pool.Exec(ctx, "ALTER TABLE ledger_attestations DISABLE TRIGGER ledger_attestations_no_update")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "UPDATE ledger_attestations SET merkle_root = decode(repeat('ff', 32), 'hex') WHERE seq = $1", seq)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "ALTER TABLE ledger_attestations ENABLE TRIGGER ledger_attestations_no_update")
	require.NoError(t, err)

	queries := postgres.NewQueryStore(pool)
	report := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusTampered, report.Status)
	require.NotEmpty(t, report.Reasons)

	require.Contains(t, report.Reasons, fmt.Sprintf("seq %d: root_hash does not match its own stored fields", seq),
		"root_hash self-consistency (v2, merkle_root bound in) must be one of the findings -- this is the specific capability §9.4(1) added: %v", report.Reasons)
}

// TestVerifyLedger_TamperedLeafHashAlone is the isolation pin for the
// SECOND, independent check design doc §9.4(2) added: corrupt ONLY one
// entry_attestations.leaf_hash row (leave journal_entries, merkle_root,
// batch_digest, root_hash, and signature all genuinely untouched) and
// confirm VerifyLedger still reports TAMPERED. Neither the "live entries
// vs merkle_root" check nor the root_hash self-consistency check can see
// this on their own -- both only look at fields this tamper leaves alone
// -- so this must be exactly the new stored-leaf-hash-vs-merkle_root
// check's finding, and exactly one finding.
func TestVerifyLedger_TamperedLeafHashAlone(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor, verifier, err := ed25519KeyPair(t, "verify-merkle-key-5")
	require.NoError(t, err)
	attestStore := postgres.NewAttestationStore(pool)
	anchor := anchordev.NewLocalFileAnchor(filepath.Join(t.TempDir(), "anchor.txt"))
	attestSvc := service.NewAttestationService(attestStore, attestor, nil, anchor, core.NewEngine())

	journalID := insertForgedJournal(t, ctx, pool, f, postgrestest.UniqueKey("verify-leaf-alone"))
	tx := beginWithCleanup(t, ctx, pool)
	debitID, _ := insertBalancedPairInTx(t, ctx, tx, f, journalID, 9801, 9802)
	require.NoError(t, tx.Commit(ctx))

	_, seq, err := attestSvc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)
	require.NotZero(t, seq)

	// entry_attestations carries the same ledger_block_mutation() guard
	// as ledger_attestations (047) -- disable it, corrupt one row's
	// leaf_hash, re-enable it.
	_, err = pool.Exec(ctx, "ALTER TABLE entry_attestations DISABLE TRIGGER entry_attestations_no_update")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "UPDATE entry_attestations SET leaf_hash = decode(repeat('ee', 32), 'hex') WHERE entry_id = $1", debitID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "ALTER TABLE entry_attestations ENABLE TRIGGER entry_attestations_no_update")
	require.NoError(t, err)

	queries := postgres.NewQueryStore(pool)
	report := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusTampered, report.Status)
	require.Len(t, report.Reasons, 1, "journal_entries/merkle_root/root_hash/signature must all still verify -- only the stored-leaf-hash check should fire: %v", report.Reasons)
	require.Contains(t, report.Reasons[0], "leaf_hash inconsistent with attested merkle_root")
}

// TestVerifyLedger_MerkleRootCheckSkippedForLegacyEmptySentinel pins v1's
// half of I-29: an attestation row that predates merkle_root being
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

	// MerkleRoot deliberately omitted -- the pre-P7/v1 shape.
	_, err = attestStore.InsertAttestation(ctx, core.Attestation{
		Seq: 1, EntryCount: int64(len(entries)), BatchDigest: batchDigest,
		PrevRoot: core.GenesisRoot, RootHash: rootHash, Signature: signature, KeyID: keyID,
	}, nil, nil, nil)
	require.NoError(t, err)
	require.NoError(t, anchor.Publish(ctx, 1, rootHash))

	queries := postgres.NewQueryStore(pool)
	report := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusVerified, report.Status, "reasons: %v", report.Reasons)
}

// TestVerifyLedger_LocalizesTamperedEntry_SelfContainedNoReferenceNeeded
// is design doc §9.4(2)'s headline capability: with NO
// VerifyConfig.ReferenceEntries configured at all, localization still
// narrows a TAMPERED verdict to the exact entry id -- migration 048's
// stored entry_attestations.leaf_hash values (confirmed internally
// consistent with the batch's own signed merkle_root) are enough on
// their own. This is what makes localization usable by on-call at the
// moment tampering is discovered, when no operator-supplied snapshot is
// likely to exist yet.
func TestVerifyLedger_LocalizesTamperedEntry_SelfContainedNoReferenceNeeded(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor, verifier, err := ed25519KeyPair(t, "verify-merkle-key-3")
	require.NoError(t, err)
	attestStore := postgres.NewAttestationStore(pool)
	anchor := anchordev.NewLocalFileAnchor(filepath.Join(t.TempDir(), "anchor.txt"))
	attestSvc := service.NewAttestationService(attestStore, attestor, nil, anchor, core.NewEngine())

	journalID := insertForgedJournal(t, ctx, pool, f, postgrestest.UniqueKey("verify-merkle-localize"))
	tx := beginWithCleanup(t, ctx, pool)
	debitID, creditID := insertBalancedPairInTx(t, ctx, tx, f, journalID, 9601, 9602)
	require.NoError(t, tx.Commit(ctx))

	_, seq, err := attestSvc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)

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
	// Deliberately zero VerifyConfig -- no ReferenceEntries.
	report := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusTampered, report.Status)
	require.Equal(t, []int64{debitID}, report.MismatchedEntryIDs[seq], "self-contained localization must name exactly the tampered entry, not creditID or the whole seq, with zero operator input")
	_ = creditID
}

// TestVerifyLedger_LocalizesTamperedEntryWithReference pins the FALLBACK
// path (design doc §9.4(2) explicitly keeps it): when self-contained
// localization is unavailable for a seq (here, an attestation created
// without leaf_hash wiring -- insertAttestationWithoutLeafHashes),
// VerifyConfig.ReferenceEntries can still localize a content mismatch.
func TestVerifyLedger_LocalizesTamperedEntryWithReference(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor, verifier, err := ed25519KeyPair(t, "verify-merkle-key-4")
	require.NoError(t, err)
	attestStore := postgres.NewAttestationStore(pool)
	anchor := anchordev.NewLocalFileAnchor(filepath.Join(t.TempDir(), "anchor.txt"))

	journalID := insertForgedJournal(t, ctx, pool, f, postgrestest.UniqueKey("verify-merkle-ref-fallback"))
	tx := beginWithCleanup(t, ctx, pool)
	debitID, creditID := insertBalancedPairInTx(t, ctx, tx, f, journalID, 9611, 9612)
	require.NoError(t, tx.Commit(ctx))

	seq, _ := insertAttestationWithoutLeafHashes(t, ctx, attestStore, attestor)
	require.NoError(t, anchor.Publish(ctx, seq, mustRootHash(t, ctx, attestStore, seq)))

	// Snapshot the entries exactly as attested -- the "trusted reference"
	// (design doc §9.1: a pre-incident backup/replica/PITR; here, a read
	// taken before the tamper below, standing in for one).
	reference, err := attestStore.EntriesForAttestation(ctx, seq)
	require.NoError(t, err)
	require.Len(t, reference, 2)

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
	require.Equal(t, []int64{debitID}, report.MismatchedEntryIDs[seq], "the fallback path must name exactly the tampered entry")
	_ = creditID
}

// TestVerifyLedger_NoLocalizationWithoutReference is the genuine
// "nothing available" counterpart: self-contained localization is
// unavailable (insertAttestationWithoutLeafHashes) AND
// VerifyConfig.ReferenceEntries is left nil. TAMPERED must still fire
// (localization is an enhancement, not a precondition for detection),
// but MismatchedEntryIDs must stay empty -- working-agreements §3: no
// fabricated entry list when localization was never attempted.
func TestVerifyLedger_NoLocalizationWithoutReference(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor, verifier, err := ed25519KeyPair(t, "verify-merkle-key-6")
	require.NoError(t, err)
	attestStore := postgres.NewAttestationStore(pool)
	anchor := anchordev.NewLocalFileAnchor(filepath.Join(t.TempDir(), "anchor.txt"))

	journalID := insertForgedJournal(t, ctx, pool, f, postgrestest.UniqueKey("verify-merkle-no-ref"))
	tx := beginWithCleanup(t, ctx, pool)
	debitID, _ := insertBalancedPairInTx(t, ctx, tx, f, journalID, 9701, 9702)
	require.NoError(t, tx.Commit(ctx))

	seq, _ := insertAttestationWithoutLeafHashes(t, ctx, attestStore, attestor)
	require.NoError(t, anchor.Publish(ctx, seq, mustRootHash(t, ctx, attestStore, seq)))

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
	require.Empty(t, report.MismatchedEntryIDs, "no reference was supplied and self-contained data was unavailable -- must not fabricate a localization result")
}

// mustRootHash reads back seq's stored root_hash -- the anchor's Head
// must reflect whatever RunAttestBatch/insertAttestationWithoutLeafHashes
// actually wrote, or step 1's anchor-vs-DB check would itself flag a
// (spurious, test-harness-only) mismatch unrelated to what each test
// means to exercise.
func mustRootHash(t testing.TB, ctx context.Context, attestStore *postgres.AttestationStore, seq int64) []byte {
	t.Helper()
	rows, err := attestStore.ListAttestationsFrom(ctx, seq, 1)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	return rows[0].RootHash
}
