// Package core: attestation.go
//
// Batch attestation chain (docs/plans/2026-08-21-tamper-evident-ledger-design.md
// §8, P6 of the integrity-hardening wave). Where P5's per-journal signature
// (core/auth.go) proves "this row was authorized when it was written", the
// attestation chain proves two things P5 cannot: that no covered row has
// since been DELETED, and that the batch history itself has not been
// rewritten. Every function here is pure -- no DB access, no signing call
// -- for the same reason CanonicalJournalDigest is pure: it is the caller's
// job (service.AttestationService) to keep the Attestor.Sign call this
// feeds into strictly outside a DB transaction (financial.md).
//
// Unlike P5's canonical digest, which MUST be uid-space (computed before
// the row it describes exists, so the transaction that inserts it and the
// external signing call can never overlap), the batch digest here is
// computed from rows that are already committed and durable by the time
// the attestation job reads them. There is no ordering hazard to avoid, so
// this file uses internal storage ids (entry_id, journal_id, currency_id,
// classification_id) directly -- reproducing a batch digest always
// requires a live database connection anyway (core.VerifyJournalAuth's
// downstream consumers read the DB to recompute; so does this one), so
// uid-space's main advantage (recomputability without touching internal
// ids) buys nothing extra here.
package core

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// Attestation is a persisted batch attestation row (design doc §8.1,
// migration 047). UID is the only identifier exposed anywhere the way
// other core types are (api-contract §3); Seq is the chain position and
// is also externally meaningful (it is what an core.Anchor publishes
// against), so it is not hidden the way internal storage ids normally
// are.
type Attestation struct {
	UID         string `json:"uid"`
	Seq         int64  `json:"seq"`
	EntryCount  int64  `json:"entry_count"`
	BatchDigest []byte `json:"batch_digest"`
	// MerkleRoot is the RFC 6962 tree root over the same batch's entries
	// (P7, migration 048) -- see that migration's header for the "new
	// column, not a rename of BatchDigest" reasoning. Non-empty means this
	// row is v2 (AttestationRootHashV2, separator 0x11): RootHash binds
	// MerkleRoot, so Signature covers it too -- design doc §9.4's fix for
	// the gap the first cut of this field shipped with (a merkle_root not
	// attested by anything outside the database is not a root a third
	// party should trust for an inclusion proof). Empty means this row
	// predates merkle_root being computed and is v1 (AttestationRootHash,
	// separator 0x03, unchanged) -- callers must treat that as "not
	// available", not as a mismatch.
	MerkleRoot []byte    `json:"merkle_root"`
	PrevRoot   []byte    `json:"prev_root"`
	RootHash   []byte    `json:"root_hash"`
	Signature  []byte    `json:"signature"`
	KeyID      string    `json:"key_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// GenesisRootHashLen is the fixed width, in bytes, of every root_hash /
// prev_root / batch_digest value in the attestation chain (a SHA-256
// output). GenesisRoot (seq 1's prev_root) is this many zero bytes.
const GenesisRootHashLen = sha256.Size

// GenesisRoot is the prev_root value for the first attestation (seq 1) --
// design doc §8.1: "创世为 32 字节 0".
var GenesisRoot = make([]byte, GenesisRootHashLen)

// batchDigestDomain / rootHashDomain / rootHashDomainV2 domain-separate this
// file's hashes from each other and from core/auth.go's authDigestDomain
// (allocation table: contracts §2.6). A breaking encoding change to any of
// them MUST introduce a new separator, never reuse an existing one.
//
//   - batchDigestDomain (0x02) / rootHashDomain (0x03): P6, unchanged since
//     migration 047. rootHashDomain is v1's root-hash formula -- every
//     attestation row created before migration 048 wired merkle_root in
//     was signed under it, and verification must keep accepting those
//     rows exactly as signed (deployment.md: an already-signed value
//     cannot be silently re-derived).
//   - rootHashDomainV2 (0x11): P7, design doc §9.4. AttestationRootHashV2
//     binds MerkleRoot into the signed hash -- the fix for the gap the
//     first cut of migration 048 shipped with (a merkle_root not attested
//     by anything outside the database is not a root a third party
//     verifying an inclusion proof should trust). Every attestation
//     migration 048 onward is created under v2; v1 rows are never
//     retroactively upgraded (same reasoning as batch_digest not being
//     renamed -- see 048's header).
const (
	batchDigestDomain = byte(0x02)
	rootHashDomain    = byte(0x03)
	rootHashDomainV2  = byte(0x11)
)

// AttestedEntry is the subset of a journal_entries row CanonicalBatchDigest
// hashes. Internal storage ids (EntryID/JournalID/CurrencyID/
// ClassificationID), not uids -- see the package doc comment for why that
// is fine here (unlike CanonicalJournalDigest).
type AttestedEntry struct {
	EntryID          int64
	JournalID        int64
	AccountHolder    int64
	CurrencyID       int64
	ClassificationID int64
	EntryType        EntryType
	Amount           decimal.Decimal
	EffectiveAt      time.Time
}

// CanonicalBatchDigest computes the deterministic, domain-separated
// SHA-256 digest of a batch of entries. entries MUST already be sorted by
// EntryID ascending (the attestation job's own read query guarantees
// this; this function does not re-sort, unlike CanonicalJournalDigest,
// because -- unlike a caller-supplied journal posting -- there is no
// untrusted ordering to normalize: the batch's membership and order are
// entirely this package's own choice).
//
// An empty entries slice is valid and produces a well-defined digest --
// design doc §8.1: "空批照样出一条" (an empty batch must still be
// attestable, so "the job ran and found nothing" is distinguishable from
// "the job never ran").
//
// Byte layout:
//
//	SHA-256(
//	  0x02                                  -- domain separator
//	  BE64(len(entries))
//	  for each entry:
//	    BE64(entry.EntryID)
//	    BE64(entry.JournalID)
//	    BE64(entry.AccountHolder)
//	    BE64(entry.CurrencyID)
//	    BE64(entry.ClassificationID)
//	    LP(string(entry.EntryType))
//	    EncodeAmount(entry.Amount)          -- 16 bytes, see EncodeAmount
//	    LP(entry.EffectiveAt.UTC().Format(RFC3339Nano))
//	)
func CanonicalBatchDigest(entries []AttestedEntry) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte(batchDigestDomain)
	writeBE64(&buf, uint64(len(entries)))

	for i, e := range entries {
		payload, err := encodeAttestedEntry(e)
		if err != nil {
			return nil, fmt.Errorf("core: canonical batch digest: entry[%d] (id=%d): %w", i, e.EntryID, err)
		}
		buf.Write(payload)
	}

	sum := sha256.Sum256(buf.Bytes())
	return sum[:], nil
}

// encodeAttestedEntry encodes a single entry's fields, in the exact order
// CanonicalBatchDigest lays them out inline for each entry. Extracted so
// core/merkle.go's leaf hashing (P7) reuses this byte-for-byte instead of
// inventing a second encoding of the same fields (design doc §9.3: "叶子的
// payload 复用 P5 的 EncodeAmount 与字段顺序纪律，不要另起一套编码").
func encodeAttestedEntry(e AttestedEntry) ([]byte, error) {
	var buf bytes.Buffer
	writeBE64(&buf, uint64(e.EntryID))
	writeBE64(&buf, uint64(e.JournalID))
	writeBE64(&buf, uint64(e.AccountHolder))
	writeBE64(&buf, uint64(e.CurrencyID))
	writeBE64(&buf, uint64(e.ClassificationID))
	writeLenPrefixed(&buf, string(e.EntryType))
	amtBytes, err := EncodeAmount(e.Amount)
	if err != nil {
		return nil, err
	}
	buf.Write(amtBytes)
	writeLenPrefixed(&buf, e.EffectiveAt.UTC().Format(time.RFC3339Nano))
	return buf.Bytes(), nil
}

// AttestationRootHash computes the hash-chain link for attestation seq,
// binding it to the previous attestation's root (prevRoot -- GenesisRoot
// for seq 1) and this batch's own digest. This is the value
// core.Attestor.Sign signs; rewriting any earlier batch's content changes
// its root_hash, which changes every subsequent prevRoot, which changes
// every root_hash after it -- so an external anchor only ever needs to
// remember the LATEST root_hash to make history rewrites anywhere in the
// chain detectable (design doc §8.3).
//
// Byte layout:
//
//	SHA-256(0x03 || BE64(seq) || prevRoot (32 bytes) || batchDigest (32 bytes) || BE64(entryCount))
//
// Returns core.ErrInvalidInput if prevRoot or batchDigest is not exactly
// GenesisRootHashLen (32) bytes -- both are meant to be SHA-256 outputs
// (GenesisRoot or a prior AttestationRootHash / CanonicalBatchDigest
// result); a different length means a caller passed the wrong value.
func AttestationRootHash(seq int64, prevRoot, batchDigest []byte, entryCount int64) ([]byte, error) {
	if len(prevRoot) != GenesisRootHashLen {
		return nil, fmt.Errorf("core: attestation root hash: prevRoot must be %d bytes, got %d: %w", GenesisRootHashLen, len(prevRoot), ErrInvalidInput)
	}
	if len(batchDigest) != GenesisRootHashLen {
		return nil, fmt.Errorf("core: attestation root hash: batchDigest must be %d bytes, got %d: %w", GenesisRootHashLen, len(batchDigest), ErrInvalidInput)
	}

	var buf bytes.Buffer
	buf.WriteByte(rootHashDomain)
	writeBE64(&buf, uint64(seq))
	buf.Write(prevRoot)
	buf.Write(batchDigest)
	writeBE64(&buf, uint64(entryCount))

	sum := sha256.Sum256(buf.Bytes())
	return sum[:], nil
}

// AttestationRootHashV2 is AttestationRootHash with merkleRoot bound into
// the signed hash (design doc §9.4, P7). Every attestation created from
// migration 048 onward uses this -- see rootHashDomainV2's doc comment for
// why v1 (AttestationRootHash) still exists and is not retroactively
// upgraded.
//
// Byte layout:
//
//	SHA-256(0x11 || BE64(seq) || prevRoot (32 bytes) || batchDigest (32 bytes) || merkleRoot (32 bytes) || BE64(entryCount))
//
// Returns core.ErrInvalidInput if prevRoot, batchDigest, or merkleRoot is
// not exactly GenesisRootHashLen (32) bytes.
func AttestationRootHashV2(seq int64, prevRoot, batchDigest, merkleRoot []byte, entryCount int64) ([]byte, error) {
	if len(prevRoot) != GenesisRootHashLen {
		return nil, fmt.Errorf("core: attestation root hash v2: prevRoot must be %d bytes, got %d: %w", GenesisRootHashLen, len(prevRoot), ErrInvalidInput)
	}
	if len(batchDigest) != GenesisRootHashLen {
		return nil, fmt.Errorf("core: attestation root hash v2: batchDigest must be %d bytes, got %d: %w", GenesisRootHashLen, len(batchDigest), ErrInvalidInput)
	}
	if len(merkleRoot) != GenesisRootHashLen {
		return nil, fmt.Errorf("core: attestation root hash v2: merkleRoot must be %d bytes, got %d: %w", GenesisRootHashLen, len(merkleRoot), ErrInvalidInput)
	}

	var buf bytes.Buffer
	buf.WriteByte(rootHashDomainV2)
	writeBE64(&buf, uint64(seq))
	buf.Write(prevRoot)
	buf.Write(batchDigest)
	buf.Write(merkleRoot)
	writeBE64(&buf, uint64(entryCount))

	sum := sha256.Sum256(buf.Bytes())
	return sum[:], nil
}

// AttestedLeaf is one persisted entry_attestations row's leaf hash (design
// doc §9.4's self-contained-localization fix, migration 048): the exact
// RFC 6962 leaf hash (core.merkleLeafHash(encodeAttestedEntry(entry))) that
// went into computing the batch's MerkleRoot at attestation time, stored
// alongside the entry_id it covers so it survives independently of
// re-deriving it from journal_entries. Its own tamper-evidence comes from
// MerkleRoot: rebuilding a tree from every AttestedLeaf in a batch and
// comparing the result against the batch's stored, signed MerkleRoot
// detects any edit to a stored leaf hash, exactly the same way editing a
// journal_entries row is detected by recomputing from the live row --
// except this check does not need to touch journal_entries at all, which
// is what makes on-call localization self-contained (no operator-supplied
// external reference required).
type AttestedLeaf struct {
	EntryID  int64
	LeafHash []byte
}
