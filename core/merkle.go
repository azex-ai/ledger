// Package core: merkle.go
//
// RFC 6962 (Certificate Transparency) Merkle tree over one attestation
// batch's entries (design doc §9, P7 of the integrity-hardening wave).
// Delivers the two capabilities P6's flat batch_digest cannot:
//
//   - localization (§9.1): given two trees over the same leaf count, find
//     the exact leaf indices where they diverge in O(k log n), k = number
//     of mismatches, instead of "the whole batch is suspect".
//   - inclusion proofs (§9.2): prove one entry is in the tree without
//     revealing any other entry's content, verifiable by a third party
//     with no database access at all.
//
// This file implements RFC 6962's tree-hashing algorithm (MTH) and audit
// path algorithm (PATH) exactly as specified -- not a from-scratch design:
//
//	MTH({})      = SHA-256()                          (empty string)
//	MTH({d(0)})  = SHA-256(0x00 || d(0))               (leaf hash)
//	MTH(D[0:n])  = SHA-256(0x01 || MTH(D[0:k]) || MTH(D[k:n]))   for n > 1
//
// where k is the largest power of two strictly smaller than n. This
// recursive split -- NOT a naive level-by-level pairing -- is what gives
// RFC 6962 trees their specific (unbalanced-but-canonical) shape for
// non-power-of-two leaf counts, and is also what closes CVE-2012-2459: an
// odd leaf at any level is never duplicated to pad the pairing, so a tree
// of n leaves and a tree of n+1 leaves (the last being a duplicate of the
// n-th) never produce the same root.
//
// Verification note (no public internet access in this environment): this
// implementation was checked against an independently-written Python
// transcription of the same RFC 6962 algorithm (golden vectors in
// merkle_test.go), the well-known SHA-256("") empty-string constant, and
// structural/security properties (single-leaf identity, non-duplication of
// odd leaves). It was NOT cross-checked against externally published
// reference-implementation test vectors (e.g. Certificate Transparency's
// own test data) -- that requires fetching them, which this session cannot
// do. Flagged explicitly rather than fabricating "official" hex constants
// from uncertain memory (discipline.md §9: don't hallucinate data you
// cannot verify).
package core

import (
	"bytes"
	"crypto/sha256"
	"fmt"
)

// merkleLeafPrefix / merkleNodePrefix are RFC 6962's own fixed domain
// bytes for leaf and interior-node hashes -- not a separator this package
// chose (unlike authDigestDomainV1 / batchDigestDomain / rootHashDomain):
// the RFC mandates exactly these two values.
const (
	merkleLeafPrefix = byte(0x00)
	merkleNodePrefix = byte(0x01)
)

// EmptyMerkleRoot is MTH({}), RFC 6962 §2.1: the SHA-256 hash of the empty
// string. The Merkle root of a batch with zero entries (design doc §8.1's
// "空批照样出一条" -- an empty batch still gets an attestation row).
var EmptyMerkleRoot = func() []byte {
	sum := sha256.Sum256(nil)
	return sum[:]
}()

func merkleLeafHash(payload []byte) []byte {
	sum := sha256.Sum256(append([]byte{merkleLeafPrefix}, payload...))
	return sum[:]
}

func merkleNodeHash(left, right []byte) []byte {
	buf := make([]byte, 0, 1+len(left)+len(right))
	buf = append(buf, merkleNodePrefix)
	buf = append(buf, left...)
	buf = append(buf, right...)
	sum := sha256.Sum256(buf)
	return sum[:]
}

// largestPowerOfTwoLessThan returns the largest k such that k is a power
// of two and k < n. Requires n > 1 (RFC 6962's MTH/PATH recursion only
// calls this when n > 1).
func largestPowerOfTwoLessThan(n int) int {
	k := 1
	for k*2 < n {
		k *= 2
	}
	return k
}

// merkleNode is one node of a fully-materialized RFC 6962 tree: every
// node's hash is computed once at build time and cached, so
// MerkleTree.LocateMismatches can compare cached hashes (O(1) per node)
// instead of recomputing MTH over a sub-slice at every step of the walk
// (which would make localization O(n) per comparison instead of the
// design doc's required O(log n)).
type merkleNode struct {
	hash        []byte
	left, right *merkleNode // nil for leaves
	leafCount   int
}

// MerkleTree is a fully-materialized RFC 6962 tree over one batch's
// entries, built once via BuildMerkleTree. Immutable after construction.
type MerkleTree struct {
	root   *merkleNode
	leaves []*merkleNode // leaf nodes, in original order, for proof generation
}

// BuildMerkleTree builds an RFC 6962 tree over entries, in the given
// order (the caller's responsibility -- same contract as
// CanonicalBatchDigest: entries must already be in the order that
// defines leaf index 0..n-1, typically ascending EntryID). Each leaf's
// payload is encodeAttestedEntry's output (shared with
// CanonicalBatchDigest -- see that function's doc comment for why: one
// encoding, not two).
func BuildMerkleTree(entries []AttestedEntry) (*MerkleTree, error) {
	if len(entries) == 0 {
		return &MerkleTree{root: &merkleNode{hash: EmptyMerkleRoot, leafCount: 0}}, nil
	}

	leaves := make([]*merkleNode, len(entries))
	for i, e := range entries {
		payload, err := encodeAttestedEntry(e)
		if err != nil {
			return nil, fmt.Errorf("core: build merkle tree: entry[%d] (id=%d): %w", i, e.EntryID, err)
		}
		leaves[i] = &merkleNode{hash: merkleLeafHash(payload), leafCount: 1}
	}

	root := buildMTH(leaves)
	return &MerkleTree{root: root, leaves: leaves}, nil
}

// BuildMerkleTreeFromLeafHashes builds an RFC 6962 tree from already-
// computed leaf hashes, in the given order (index 0..n-1), rather than
// from raw entries. Design doc §9.4's self-contained-localization fix:
// service.VerifyLedger rebuilds a tree from migration 048's persisted
// AttestedLeaf.LeafHash values (not from re-derived entry payloads) to
// check them against the batch's stored, signed MerkleRoot -- if this
// took raw payloads instead, it could not distinguish "the stored leaf
// hash was tampered directly" from "the entry it was derived from
// changed" (both would just look like a payload to re-hash); taking
// hashes directly makes it a check purely on entry_attestations.leaf_hash
// itself.
//
// Returns core.ErrInvalidInput if any leafHashes[i] is not exactly
// sha256.Size (32) bytes -- every element is meant to already be an RFC
// 6962 leaf hash (merkleLeafHash's output), not a payload to hash.
func BuildMerkleTreeFromLeafHashes(leafHashes [][]byte) (*MerkleTree, error) {
	if len(leafHashes) == 0 {
		return &MerkleTree{root: &merkleNode{hash: EmptyMerkleRoot, leafCount: 0}}, nil
	}
	leaves := make([]*merkleNode, len(leafHashes))
	for i, h := range leafHashes {
		if len(h) != sha256.Size {
			return nil, fmt.Errorf("core: build merkle tree from leaf hashes: leafHashes[%d] must be %d bytes, got %d: %w", i, sha256.Size, len(h), ErrInvalidInput)
		}
		leaves[i] = &merkleNode{hash: h, leafCount: 1}
	}
	return &MerkleTree{root: buildMTH(leaves), leaves: leaves}, nil
}

// buildMerkleTreeFromPayloads is BuildMerkleTree without the
// AttestedEntry encoding step -- exported only within the package, for
// merkle_test.go's algorithm-level golden vectors (arbitrary byte
// payloads, so the RFC 6962 tree-hashing algorithm itself is pinned
// independently of core/attestation.go's entry encoding).
func buildMerkleTreeFromPayloads(payloads [][]byte) *MerkleTree {
	if len(payloads) == 0 {
		return &MerkleTree{root: &merkleNode{hash: EmptyMerkleRoot, leafCount: 0}}
	}
	leaves := make([]*merkleNode, len(payloads))
	for i, p := range payloads {
		leaves[i] = &merkleNode{hash: merkleLeafHash(p), leafCount: 1}
	}
	return &MerkleTree{root: buildMTH(leaves), leaves: leaves}
}

// buildMTH recursively builds the cached-hash tree per RFC 6962's MTH
// split (largest power of two k < n). nodes must be non-empty.
func buildMTH(nodes []*merkleNode) *merkleNode {
	n := len(nodes)
	if n == 1 {
		return nodes[0]
	}
	k := largestPowerOfTwoLessThan(n)
	left := buildMTH(nodes[:k])
	right := buildMTH(nodes[k:])
	return &merkleNode{
		hash:      merkleNodeHash(left.hash, right.hash),
		left:      left,
		right:     right,
		leafCount: left.leafCount + right.leafCount,
	}
}

// Root returns the tree's MTH root hash.
func (t *MerkleTree) Root() []byte { return t.root.hash }

// LeafCount returns the number of leaves the tree was built over.
func (t *MerkleTree) LeafCount() int { return t.root.leafCount }

// LeafHash returns the RFC 6962 leaf hash (MTH({d(index)}), i.e.
// SHA-256(0x00 || payload)) at index -- the value a caller passes to
// VerifyInclusion alongside a GenerateInclusionProof result. Returns
// core.ErrInvalidInput for an out-of-range index.
func (t *MerkleTree) LeafHash(index int) ([]byte, error) {
	if index < 0 || index >= len(t.leaves) {
		return nil, fmt.Errorf("core: leaf hash: index %d out of range [0,%d): %w", index, len(t.leaves), ErrInvalidInput)
	}
	return t.leaves[index].hash, nil
}

// LeafHashes returns every leaf's RFC 6962 leaf hash, in the same order
// entries/leafHashes were built from (index 0..LeafCount()-1) --
// service.AttestationService.RunAttestBatch uses this to persist each
// entry's AttestedLeaf.LeafHash alongside the batch's MerkleRoot
// (design doc §9.4), rather than looping LeafHash(i) one at a time.
func (t *MerkleTree) LeafHashes() [][]byte {
	out := make([][]byte, len(t.leaves))
	for i, l := range t.leaves {
		out[i] = l.hash
	}
	return out
}

// InclusionProof is the audit path RFC 6962 §2.1.1's PATH algorithm
// produces for one leaf: the sibling hashes needed to recompute the root,
// ordered from the leaf level up to the root. Never contains any leaf's
// raw payload -- only hashes -- so handing this to a third party proves
// "this leaf hash is in a tree with this root" without revealing any
// other entry's content (design doc §9.2's red line).
type InclusionProof struct {
	LeafIndex int64    `json:"leaf_index"`
	TreeSize  int64    `json:"tree_size"`
	Path      [][]byte `json:"path"`
}

// GenerateInclusionProof returns the audit path for the leaf at index (0
// <= index < t.LeafCount()). Returns core.ErrInvalidInput for an
// out-of-range index or an empty tree (no leaves to prove membership of).
func (t *MerkleTree) GenerateInclusionProof(index int) (*InclusionProof, error) {
	n := t.LeafCount()
	if n == 0 {
		return nil, fmt.Errorf("core: generate inclusion proof: empty tree has no leaves: %w", ErrInvalidInput)
	}
	if index < 0 || index >= n {
		return nil, fmt.Errorf("core: generate inclusion proof: index %d out of range [0,%d): %w", index, n, ErrInvalidInput)
	}

	var path [][]byte
	node := t.root
	i := index
	for node.left != nil { // descend until we hit the leaf
		k := node.left.leafCount
		if i < k {
			path = append([][]byte{node.right.hash}, path...)
			node = node.left
		} else {
			path = append([][]byte{node.left.hash}, path...)
			node = node.right
			i -= k
		}
	}

	return &InclusionProof{LeafIndex: int64(index), TreeSize: int64(n), Path: path}, nil
}

// VerifyInclusion recomputes a root from leafHash + proof and reports
// whether it equals root. Pure function, zero dependencies (no DB, no
// MerkleTree instance needed) -- design doc §9.2: a third party verifies
// with only this function, leafHash, the proof, and a root it trusts
// (e.g. from core.Anchor), never touching the ledger's database.
//
// proof.TreeSize is required (not inferable from len(proof.Path) alone):
// RFC 6962's recursive split makes audit-path length vary by leaf index
// for non-power-of-two tree sizes, and reconstructing which side (left or
// right) each sibling belongs on requires re-deriving the same
// largest-power-of-two splits PATH() used to generate the proof, which in
// turn requires knowing the tree size at each recursion level.
func VerifyInclusion(leafHash []byte, proof InclusionProof, root []byte) bool {
	if proof.TreeSize <= 0 || proof.LeafIndex < 0 || proof.LeafIndex >= proof.TreeSize {
		return false
	}

	// directions is ordered root-to-leaf: directions[0] is the shallowest
	// (root-level) split, directions[last] is the deepest (leaf-adjacent)
	// split. proof.Path is ordered leaf-to-root (RFC 6962 PATH()'s own
	// convention, reproduced by GenerateInclusionProof's prepend-while-
	// descending): Path[0] is the deepest sibling, Path[last] is the
	// root-level sibling. Verifying means walking bottom-up (bumping the
	// leaf hash up to the root), so Path is consumed ascending while
	// directions is consumed from its END -- Path[i] always pairs with
	// directions[len(directions)-1-i].
	directions := computeMerkleDirections(proof.LeafIndex, proof.TreeSize)
	if len(directions) != len(proof.Path) {
		return false // malformed proof: wrong number of siblings for this (index, treeSize)
	}

	current := leafHash
	for i := 0; i < len(proof.Path); i++ {
		sibling := proof.Path[i]
		dir := directions[len(directions)-1-i]
		if dir {
			// the leaf/accumulated-hash was in the LEFT partition at this
			// level -> the sibling is on the right.
			current = merkleNodeHash(current, sibling)
		} else {
			current = merkleNodeHash(sibling, current)
		}
	}
	return bytes.Equal(current, root)
}

// computeMerkleDirections returns, for a tree of treeSize leaves and the
// leaf at index, the sequence of left(true)/right(false) partition
// decisions from the ROOT split down to the leaf -- directions[0] is the
// top-level split, directions[len-1] is the deepest (leaf-adjacent) split.
// This is exactly the recursion PATH()/GenerateInclusionProof walks, kept
// as its own function so verification can replay the same decisions
// without a materialized tree.
func computeMerkleDirections(index, treeSize int64) []bool {
	var dirs []bool
	for treeSize > 1 {
		k := int64(largestPowerOfTwoLessThan(int(treeSize)))
		if index < k {
			dirs = append(dirs, true)
			treeSize = k
		} else {
			dirs = append(dirs, false)
			index -= k
			treeSize -= k
		}
	}
	return dirs
}

// LocateMismatches walks two equally-sized trees from the root down,
// descending only into subtrees whose cached hashes disagree, and returns
// the leaf indices where they diverge -- O(k log n), k = number of
// mismatches (design doc §9.1).
//
// reference is whatever the caller trusts as ground truth for this
// comparison -- a pre-incident snapshot restore, a second independent
// replica, a WAL-based point-in-time recovery, etc. This package does not
// pick a source: unlike batch_digest/merkle_root (which are compared
// against a single signed, stored value), meaningful leaf-level
// localization requires a second FULL tree to diff against, and this
// schema deliberately does not persist one (migration 048's ★ note: only
// the aggregate merkle_root is stored). Supplying that second tree is an
// operational/forensics decision, not something this library can default
// (abstractions.md: defer implementation choices that are not this
// package's to make).
//
// Returns core.ErrInvalidInput if the two trees have different leaf
// counts -- a count mismatch means rows were added or removed, which
// core.CanonicalBatchDigest's entry-count recheck (already part of
// service.VerifyLedger) catches on its own; localization only makes sense
// when both trees describe the same set of leaf positions.
func LocateMismatches(reference, actual *MerkleTree) ([]int, error) {
	if reference.LeafCount() != actual.LeafCount() {
		return nil, fmt.Errorf(
			"core: locate mismatches: tree sizes differ (reference=%d, actual=%d); localization requires equal leaf counts -- an entry-count mismatch is a deletion/insertion, already caught by CanonicalBatchDigest's recount: %w",
			reference.LeafCount(), actual.LeafCount(), ErrInvalidInput,
		)
	}
	if reference.LeafCount() == 0 {
		return nil, nil // both empty, nothing to compare
	}

	var mismatched []int
	var walk func(ref, act *merkleNode, offset int)
	walk = func(ref, act *merkleNode, offset int) {
		if bytes.Equal(ref.hash, act.hash) {
			return // subtrees identical -- prune, do not descend
		}
		if ref.left == nil { // leaf level
			mismatched = append(mismatched, offset)
			return
		}
		walk(ref.left, act.left, offset)
		walk(ref.right, act.right, offset+ref.left.leafCount)
	}
	walk(reference.root, actual.root, 0)
	return mismatched, nil
}
