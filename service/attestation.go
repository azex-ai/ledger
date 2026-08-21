// Package service: attestation.go
//
// AttestationService drives the P6 batch attestation chain (design doc §8,
// docs/plans/2026-08-21-integrity-hardening-contracts.md §4). It closes
// two gaps P5's per-journal signature cannot: a row being DELETED (no
// signature survives on a deleted row to say it should still be there),
// and the batch history being rewritten wholesale.
package service

import (
	"context"
	"fmt"

	"github.com/azex-ai/ledger/core"
)

// AttestationStore is the postgres-backed persistence port
// AttestationService consumes. Implemented by postgres.AttestationStore.
type AttestationStore interface {
	// LatestAttestation returns the highest-seq attestation, or a zero
	// core.Attestation (Seq == 0) if the chain has never been started.
	LatestAttestation(ctx context.Context) (core.Attestation, error)
	// UncoveredEntries returns up to limit entries with no attestation
	// coverage yet, oldest id first. See core/attestation.go's package doc
	// comment for why "coverage" is a queryable fact, not an id-range
	// assumption.
	UncoveredEntries(ctx context.Context, limit int32) ([]core.AttestedEntry, error)
	// InsertAttestation atomically inserts the attestation row and its
	// entry_attestations coverage rows. entryIDs empty is valid -- an
	// empty batch still gets a row (design doc §8.1).
	InsertAttestation(ctx context.Context, input core.Attestation, entryIDs []int64) (core.Attestation, error)
	// EntriesForAttestation re-fetches exactly the entries seq covered, in
	// the same order UncoveredEntries would have produced them in.
	EntriesForAttestation(ctx context.Context, seq int64) ([]core.AttestedEntry, error)
	// ListAttestationsFrom is a paginated ascending chain walk starting at
	// fromSeq (inclusive).
	ListAttestationsFrom(ctx context.Context, fromSeq int64, limit int32) ([]core.Attestation, error)
}

// AttestationService creates new attestations and keeps the external
// anchor caught up.
type AttestationService struct {
	store    AttestationStore
	attestor core.Attestor
	// anchor is nil when no external anchor has been configured --
	// attestation still proceeds (the DB-side hash chain is real and
	// checkable on its own), but nothing outside the ledger's own database
	// can detect a wholesale history rewrite until an anchor is wired in
	// (design doc §8.3 / contracts §7: the anchor carrier is a genuinely
	// unresolved deployment choice; the library ships no production
	// adapter, so tolerating nil here is what lets P6 ship at all before
	// Aaron picks one).
	anchor core.Anchor
	logger core.Logger
}

// NewAttestationService creates an AttestationService. attestor is
// required -- there is no "unsigned attestation" concept in this schema
// (ledger_attestations.signature is NOT NULL with no expand-safe empty
// default, unlike P5's auth columns): RunAttestBatch refuses to run
// without one, rather than silently skipping the whole feature the way
// PostJournal treats a nil P5 Attestor. anchor may be nil (see the
// AttestationService.anchor field doc comment).
func NewAttestationService(store AttestationStore, attestor core.Attestor, anchor core.Anchor, engine *core.Engine) *AttestationService {
	return &AttestationService{
		store:    store,
		attestor: attestor,
		anchor:   anchor,
		logger:   engine.Logger(),
	}
}

// RunAttestBatch creates exactly one new attestation covering up to
// batchSize currently-uncovered entries (zero is valid -- design doc
// §8.1's "空批照样出一条"), then best-effort publishes it to the anchor.
// Returns the number of entries covered and the new attestation's seq.
//
// Signing happens strictly before any DB transaction is opened
// (financial.md: no external calls inside a DB transaction) -- reading
// entries and the latest chain position are plain queries, not part of
// the transaction AttestationStore.InsertAttestation opens for the
// write.
func (s *AttestationService) RunAttestBatch(ctx context.Context, batchSize int32) (attested int, seq int64, err error) {
	if s.attestor == nil {
		return 0, 0, fmt.Errorf("service: attestation: no Attestor configured: %w", core.ErrInvalidInput)
	}

	s.catchUpAnchor(ctx)

	latest, err := s.store.LatestAttestation(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("service: attestation: latest attestation: %w", err)
	}
	nextSeq := latest.Seq + 1
	prevRoot := latest.RootHash
	if latest.Seq == 0 {
		prevRoot = core.GenesisRoot
	}

	entries, err := s.store.UncoveredEntries(ctx, batchSize)
	if err != nil {
		return 0, 0, fmt.Errorf("service: attestation: uncovered entries: %w", err)
	}

	batchDigest, err := core.CanonicalBatchDigest(entries)
	if err != nil {
		return 0, 0, fmt.Errorf("service: attestation: batch digest: %w", err)
	}
	rootHash, err := core.AttestationRootHash(nextSeq, prevRoot, batchDigest, int64(len(entries)))
	if err != nil {
		return 0, 0, fmt.Errorf("service: attestation: root hash: %w", err)
	}

	// P7: RFC 6962 Merkle root over the same entries, for localization
	// (core.LocateMismatches) and inclusion proofs (core.
	// GenerateInclusionProof/core.VerifyInclusion) -- see migration 048's
	// header for why this is NOT one of rootHash's inputs (it is a
	// deliberately separate, additive value, not part of what gets
	// signed). An empty batch still produces a real 32-byte RFC 6962
	// empty-tree root (core.EmptyMerkleRoot), not the migration's
	// never-computed sentinel ('').
	merkleTree, err := core.BuildMerkleTree(entries)
	if err != nil {
		return 0, 0, fmt.Errorf("service: attestation: merkle tree: %w", err)
	}
	merkleRoot := merkleTree.Root()

	signature, keyID, err := s.attestor.Sign(ctx, rootHash)
	if err != nil {
		return 0, 0, fmt.Errorf("service: attestation: attestor sign: %w: %w", err, core.ErrAttestorUnavailable)
	}

	entryIDs := make([]int64, len(entries))
	for i, e := range entries {
		entryIDs[i] = e.EntryID
	}

	result, err := s.store.InsertAttestation(ctx, core.Attestation{
		Seq:         nextSeq,
		EntryCount:  int64(len(entries)),
		BatchDigest: batchDigest,
		MerkleRoot:  merkleRoot,
		PrevRoot:    prevRoot,
		RootHash:    rootHash,
		Signature:   signature,
		KeyID:       keyID,
	}, entryIDs)
	if err != nil {
		return 0, 0, fmt.Errorf("service: attestation: insert: %w", err)
	}

	if s.anchor != nil {
		if err := s.anchor.Publish(ctx, result.Seq, result.RootHash); err != nil {
			// Anchoring is a sidecar to the DB-side chain, not a
			// precondition for it -- design doc §8.3: "锚定失败不阻塞
			// journal 写入" extends to not blocking attestation either.
			// catchUpAnchor retries this on the next run.
			s.logger.Error("service: attestation: anchor publish failed, will retry next run", "seq", result.Seq, "error", err)
		}
	}

	return len(entries), result.Seq, nil
}

// catchUpAnchor republishes every attestation the anchor has not seen yet
// (the gap between core.Anchor.Head and the DB's latest seq), oldest
// first. This IS the "本地重试队列" design doc §8.3 calls for -- the queue
// is the gap itself, not a separate table, so it survives process
// restarts for free (the anchor is external and durable).
func (s *AttestationService) catchUpAnchor(ctx context.Context) {
	if s.anchor == nil {
		return
	}
	anchorSeq, _, err := s.anchor.Head(ctx)
	if err != nil {
		s.logger.Error("service: attestation: anchor head unavailable, skipping catch-up this run", "error", err)
		return
	}

	latest, err := s.store.LatestAttestation(ctx)
	if err != nil {
		s.logger.Error("service: attestation: catch-up: latest attestation failed", "error", err)
		return
	}
	if latest.Seq <= anchorSeq {
		return
	}

	const catchUpPageSize = 1000
	missing, err := s.store.ListAttestationsFrom(ctx, anchorSeq+1, catchUpPageSize)
	if err != nil {
		s.logger.Error("service: attestation: catch-up: list attestations failed", "error", err)
		return
	}
	for _, a := range missing {
		if err := s.anchor.Publish(ctx, a.Seq, a.RootHash); err != nil {
			s.logger.Error("service: attestation: catch-up: publish failed, will retry next run", "seq", a.Seq, "error", err)
			return // the anchor is likely still down; don't hammer it with the rest of the page.
		}
	}
}
