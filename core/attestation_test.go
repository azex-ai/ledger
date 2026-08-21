package core

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// Golden vectors for CanonicalBatchDigest / AttestationRootHash.
//
// Every hex value below was computed independently in Python, replicating
// the byte layout documented on CanonicalBatchDigest / AttestationRootHash
// -- not by calling this package. See core/auth_test.go's golden vectors
// for the sibling P5 mechanism this mirrors.
// ---------------------------------------------------------------------------

func TestCanonicalBatchDigest_EmptyBatchGoldenVector(t *testing.T) {
	const want = "4322fd2bc0a137d1375b37b3b2e2b4715b3d3dd7ca9682438d4fea0f8437fad3"
	got, err := CanonicalBatchDigest(nil)
	if err != nil {
		t.Fatalf("CanonicalBatchDigest(nil): unexpected error: %v", err)
	}
	if hex.EncodeToString(got) != want {
		t.Errorf("CanonicalBatchDigest(nil) = %s, want %s", hex.EncodeToString(got), want)
	}
}

func TestAttestationRootHash_GenesisGoldenVector(t *testing.T) {
	emptyDigest, err := CanonicalBatchDigest(nil)
	if err != nil {
		t.Fatalf("CanonicalBatchDigest(nil): %v", err)
	}
	const want = "bd61f82d035c6fb50d8b9b5f5bd1913162b9af7d559c89d1756da5f8d1674252"
	got, err := AttestationRootHash(1, GenesisRoot, emptyDigest, 0)
	if err != nil {
		t.Fatalf("AttestationRootHash: unexpected error: %v", err)
	}
	if hex.EncodeToString(got) != want {
		t.Errorf("AttestationRootHash(seq=1, genesis, empty digest, 0) = %s, want %s", hex.EncodeToString(got), want)
	}
}

func twoEntryBatch() []AttestedEntry {
	effectiveAt := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	return []AttestedEntry{
		{EntryID: 100, JournalID: 50, AccountHolder: 1001, CurrencyID: 1, ClassificationID: 2, EntryType: EntryTypeDebit, Amount: decimal.RequireFromString("100.5"), EffectiveAt: effectiveAt},
		{EntryID: 101, JournalID: 50, AccountHolder: 2001, CurrencyID: 1, ClassificationID: 3, EntryType: EntryTypeCredit, Amount: decimal.RequireFromString("100.5"), EffectiveAt: effectiveAt},
	}
}

func TestCanonicalBatchDigest_TwoEntryGoldenVector(t *testing.T) {
	const want = "f927af858687ce2c340b9270e4a966653ecfaba75d0ae3122cfff082b96f2e82"
	got, err := CanonicalBatchDigest(twoEntryBatch())
	if err != nil {
		t.Fatalf("CanonicalBatchDigest: unexpected error: %v", err)
	}
	if hex.EncodeToString(got) != want {
		t.Errorf("CanonicalBatchDigest(2 entries) = %s, want %s", hex.EncodeToString(got), want)
	}
}

func TestAttestationRootHash_ChainedGoldenVector(t *testing.T) {
	emptyDigest, err := CanonicalBatchDigest(nil)
	if err != nil {
		t.Fatalf("CanonicalBatchDigest(nil): %v", err)
	}
	rh1, err := AttestationRootHash(1, GenesisRoot, emptyDigest, 0)
	if err != nil {
		t.Fatalf("AttestationRootHash(seq=1): %v", err)
	}

	d2, err := CanonicalBatchDigest(twoEntryBatch())
	if err != nil {
		t.Fatalf("CanonicalBatchDigest: %v", err)
	}
	const want = "f0dd51f2a0a666cf2a234189b585e4fde75d33fe0d1380359ad3449929b5d768"
	rh2, err := AttestationRootHash(2, rh1, d2, 2)
	if err != nil {
		t.Fatalf("AttestationRootHash(seq=2): %v", err)
	}
	if hex.EncodeToString(rh2) != want {
		t.Errorf("AttestationRootHash(seq=2, chained) = %s, want %s", hex.EncodeToString(rh2), want)
	}
}

func TestCanonicalBatchDigest_NegativeHolderTinyAmountGoldenVector(t *testing.T) {
	const want = "d5107c69c368fc5b4c95a7bc3b30a133415f14efdfe5409d2bb29f595710569e"
	entries := []AttestedEntry{
		{
			EntryID: 200, JournalID: 99, AccountHolder: -9001, CurrencyID: 5, ClassificationID: 6,
			EntryType: EntryTypeDebit, Amount: decimal.RequireFromString("0.000000000000000001"),
			EffectiveAt: time.Date(2026, 8, 21, 15, 30, 0, 0, time.UTC),
		},
	}
	got, err := CanonicalBatchDigest(entries)
	if err != nil {
		t.Fatalf("CanonicalBatchDigest: unexpected error: %v", err)
	}
	if hex.EncodeToString(got) != want {
		t.Errorf("CanonicalBatchDigest = %s, want %s", hex.EncodeToString(got), want)
	}
}

// ---------------------------------------------------------------------------
// Structural properties.
// ---------------------------------------------------------------------------

func TestCanonicalBatchDigest_DeterministicAcrossCalls(t *testing.T) {
	entries := twoEntryBatch()
	d1, err := CanonicalBatchDigest(entries)
	if err != nil {
		t.Fatalf("CanonicalBatchDigest: %v", err)
	}
	d2, err := CanonicalBatchDigest(entries)
	if err != nil {
		t.Fatalf("CanonicalBatchDigest: %v", err)
	}
	if hex.EncodeToString(d1) != hex.EncodeToString(d2) {
		t.Error("same entries produced different digests")
	}
}

func TestCanonicalBatchDigest_DifferentEntriesDifferentDigest(t *testing.T) {
	a, err := CanonicalBatchDigest(twoEntryBatch())
	if err != nil {
		t.Fatalf("CanonicalBatchDigest: %v", err)
	}
	modified := twoEntryBatch()
	modified[0].Amount = modified[0].Amount.Add(decimal.NewFromInt(1))
	b, err := CanonicalBatchDigest(modified)
	if err != nil {
		t.Fatalf("CanonicalBatchDigest: %v", err)
	}
	if hex.EncodeToString(a) == hex.EncodeToString(b) {
		t.Error("changing an entry's amount did not change the batch digest")
	}
}

func TestCanonicalBatchDigest_EntryOrderMatters(t *testing.T) {
	// Unlike CanonicalJournalDigest, CanonicalBatchDigest does NOT sort --
	// the caller (the attestation job's own fetch order) owns ordering.
	// Pin that this function does not silently normalize order, so a
	// future change to that contract is a deliberate, reviewed decision.
	entries := twoEntryBatch()
	a, err := CanonicalBatchDigest(entries)
	if err != nil {
		t.Fatalf("CanonicalBatchDigest: %v", err)
	}
	reversed := []AttestedEntry{entries[1], entries[0]}
	b, err := CanonicalBatchDigest(reversed)
	if err != nil {
		t.Fatalf("CanonicalBatchDigest: %v", err)
	}
	if hex.EncodeToString(a) == hex.EncodeToString(b) {
		t.Error("CanonicalBatchDigest must be order-sensitive (it does not sort like CanonicalJournalDigest)")
	}
}

func TestAttestationRootHash_RejectsWrongLengthPrevRoot(t *testing.T) {
	digest, err := CanonicalBatchDigest(nil)
	if err != nil {
		t.Fatalf("CanonicalBatchDigest: %v", err)
	}
	if _, err := AttestationRootHash(1, []byte("too-short"), digest, 0); err == nil {
		t.Error("expected error for wrong-length prevRoot, got nil")
	}
}

func TestAttestationRootHash_RejectsWrongLengthBatchDigest(t *testing.T) {
	if _, err := AttestationRootHash(1, GenesisRoot, []byte("too-short"), 0); err == nil {
		t.Error("expected error for wrong-length batchDigest, got nil")
	}
}

func TestAttestationRootHash_DifferentSeqDifferentHash(t *testing.T) {
	digest, err := CanonicalBatchDigest(nil)
	if err != nil {
		t.Fatalf("CanonicalBatchDigest: %v", err)
	}
	a, err := AttestationRootHash(1, GenesisRoot, digest, 0)
	if err != nil {
		t.Fatalf("AttestationRootHash: %v", err)
	}
	b, err := AttestationRootHash(2, GenesisRoot, digest, 0)
	if err != nil {
		t.Fatalf("AttestationRootHash: %v", err)
	}
	if hex.EncodeToString(a) == hex.EncodeToString(b) {
		t.Error("different seq produced the same root hash")
	}
}

// ---------------------------------------------------------------------------
// Golden vectors for AttestationRootHashV2 (design doc §9.4, P7). Computed
// independently in Python (same buffer-layout replication as the v1
// vectors above), using the ALREADY-PINNED v1 empty-batch digest
// ("4322fd2b...") as a cross-check that the Python encoder for this
// session matches the one that produced the pre-existing v1 pins above --
// not just self-consistent with this file's own Go implementation.
// ---------------------------------------------------------------------------

func TestAttestationRootHashV2_GenesisGoldenVector(t *testing.T) {
	emptyDigest, err := CanonicalBatchDigest(nil)
	if err != nil {
		t.Fatalf("CanonicalBatchDigest(nil): %v", err)
	}
	const want = "bdb2acc07e7e373f7b0039c83441ab041324341f319b75d7be6a55d3c863ade5"
	got, err := AttestationRootHashV2(1, GenesisRoot, emptyDigest, EmptyMerkleRoot, 0)
	if err != nil {
		t.Fatalf("AttestationRootHashV2: unexpected error: %v", err)
	}
	if hex.EncodeToString(got) != want {
		t.Errorf("AttestationRootHashV2(seq=1, genesis, empty digest, empty merkle root, 0) = %s, want %s", hex.EncodeToString(got), want)
	}
}

func TestAttestationRootHashV2_ChainedGoldenVector(t *testing.T) {
	rh1, err := AttestationRootHashV2(1, GenesisRoot, mustDigest(t, nil), EmptyMerkleRoot, 0)
	if err != nil {
		t.Fatalf("AttestationRootHashV2(seq=1): %v", err)
	}

	arbitraryMerkleRoot, err := hex.DecodeString("2d6e943e85ac09dd6af182bf9fc9041abe70609149a3d2d55717e09e37507e6d")
	if err != nil {
		t.Fatalf("decode test merkle root: %v", err)
	}
	const want = "90aa6e40fd9bb0760bb122d6111f4c782796ec45152e7a2ab53becb8122d9273"
	rh2, err := AttestationRootHashV2(2, rh1, mustDigest(t, twoEntryBatch()), arbitraryMerkleRoot, 2)
	if err != nil {
		t.Fatalf("AttestationRootHashV2(seq=2): %v", err)
	}
	if hex.EncodeToString(rh2) != want {
		t.Errorf("AttestationRootHashV2(seq=2, chained) = %s, want %s", hex.EncodeToString(rh2), want)
	}
}

func mustDigest(t *testing.T, entries []AttestedEntry) []byte {
	t.Helper()
	d, err := CanonicalBatchDigest(entries)
	if err != nil {
		t.Fatalf("CanonicalBatchDigest: %v", err)
	}
	return d
}

func TestAttestationRootHashV2_DiffersFromV1ForTheSameInputs(t *testing.T) {
	// The whole point of a separate domain separator (0x11 vs 0x03): the
	// same seq/prevRoot/batchDigest/entryCount must never collide between
	// v1 and what v2 would produce if merkleRoot happened to equal
	// GenesisRoot's all-zero bytes (an adversarial edge case worth pinning
	// explicitly, not just trusting the domain byte in the abstract).
	digest := mustDigest(t, nil)
	v1, err := AttestationRootHash(1, GenesisRoot, digest, 0)
	if err != nil {
		t.Fatalf("AttestationRootHash: %v", err)
	}
	zeroMerkleRoot := make([]byte, GenesisRootHashLen)
	v2, err := AttestationRootHashV2(1, GenesisRoot, digest, zeroMerkleRoot, 0)
	if err != nil {
		t.Fatalf("AttestationRootHashV2: %v", err)
	}
	if hex.EncodeToString(v1) == hex.EncodeToString(v2) {
		t.Error("v1 and v2 produced the same root hash for the same seq/prevRoot/batchDigest/entryCount -- domain separation is broken")
	}
}

func TestAttestationRootHashV2_DifferentMerkleRootDifferentHash(t *testing.T) {
	digest := mustDigest(t, nil)
	a, err := AttestationRootHashV2(1, GenesisRoot, digest, EmptyMerkleRoot, 0)
	if err != nil {
		t.Fatalf("AttestationRootHashV2: %v", err)
	}
	otherRoot := make([]byte, GenesisRootHashLen)
	otherRoot[0] = 0xff
	b, err := AttestationRootHashV2(1, GenesisRoot, digest, otherRoot, 0)
	if err != nil {
		t.Fatalf("AttestationRootHashV2: %v", err)
	}
	if hex.EncodeToString(a) == hex.EncodeToString(b) {
		t.Error("changing merkleRoot did not change AttestationRootHashV2's output -- it is not actually bound into the hash")
	}
}

func TestAttestationRootHashV2_RejectsWrongLengthMerkleRoot(t *testing.T) {
	digest := mustDigest(t, nil)
	if _, err := AttestationRootHashV2(1, GenesisRoot, digest, []byte("too-short"), 0); err == nil {
		t.Error("expected error for wrong-length merkleRoot, got nil")
	}
}

func TestGenesisRoot_IsThirtyTwoZeroBytes(t *testing.T) {
	if len(GenesisRoot) != GenesisRootHashLen {
		t.Fatalf("len(GenesisRoot) = %d, want %d", len(GenesisRoot), GenesisRootHashLen)
	}
	for i, b := range GenesisRoot {
		if b != 0 {
			t.Fatalf("GenesisRoot[%d] = %d, want 0", i, b)
		}
	}
}
