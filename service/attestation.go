// Package service: attestation.go
//
// AttestationService drives the P6 batch attestation chain (design doc §8,
// docs/plans/2026-08-21-integrity-hardening-contracts.md §4). It closes
// two gaps P5's per-journal signature cannot: a row being DELETED (no
// signature survives on a deleted row to say it should still be there),
// and the batch history being rewritten wholesale.
//
// T4 (design doc §8 extended, contracts §W3-B) extends the same batch job
// with a third amortized check: for every distinct journal contributing an
// entry to the batch, RunAttestBatch runs core.VerifyJournalAuth ONCE and
// binds the pass/fail verdict into the batch's own signed content
// (core.AuthVerdictDigest -> core.AttestationRootHashV3). A later
// postgres.VerifiedBalanceStore call for an already-attested entry then
// trusts the cached verdict instead of re-deriving it -- the naive path
// measured in .local/bench-verify-2026-08-23.md pays a DB round trip plus
// a fresh crypto verify for every contributing journal, every call; T4
// pays that cost once per journal, here, and every later read of an
// attested entry is free. T4 is additive and opt-in (verifier may be nil):
// an AttestationService with no core.AuthVerifier configured behaves
// exactly as it did before T4 existed (v2 attestations, no verdicts).
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
	// empty batch still gets a row (design doc §8.1). leafHashes and
	// verdicts, if non-nil, must be the same length as entryIDs (design doc
	// §9.4 -- entryIDs[i]'s stored core.AttestedLeaf.LeafHash; T4, design
	// doc §8 extended -- entryIDs[i]'s cached core.JournalAuthVerdict).
	InsertAttestation(ctx context.Context, input core.Attestation, entryIDs []int64, leafHashes [][]byte, verdicts []core.JournalAuthVerdict) (core.Attestation, error)
	// EntriesForAttestation re-fetches exactly the entries seq covered, in
	// the same order UncoveredEntries would have produced them in.
	EntriesForAttestation(ctx context.Context, seq int64) ([]core.AttestedEntry, error)
	// LeafHashesForAttestation returns the stored core.AttestedLeaf rows
	// seq covered, in the same entry_id order EntriesForAttestation uses
	// (design doc §9.4's self-contained localization).
	LeafHashesForAttestation(ctx context.Context, seq int64) ([]core.AttestedLeaf, error)
	// ListAttestationsFrom is a paginated ascending chain walk starting at
	// fromSeq (inclusive).
	ListAttestationsFrom(ctx context.Context, fromSeq int64, limit int32) ([]core.Attestation, error)
	// RecordAnchorObservation appends one row remembering what
	// core.Anchor.Head just reported (migration 018). Append-only: this is
	// the memory service.VerifyLedger compares a live head against, so that
	// "the anchor went backwards" is provable rather than
	// indistinguishable from "the anchor was never published to"
	// (tamper-evident.md M-3). seq == 0 is a legal, meaningful observation.
	RecordAnchorObservation(ctx context.Context, seq int64, head []byte) error
	// HighestObservedAnchorSeq returns the highest seq this deployment has
	// ever recorded seeing the anchor report, or 0 if no observation has
	// been recorded yet.
	HighestObservedAnchorSeq(ctx context.Context) (int64, error)
	// JournalAuthMaterial batch-fetches everything core.VerifyJournalAuth
	// needs to reconstruct and verify each of journalIDs, in as few round
	// trips as the store can manage (design doc §4.5's batched-fetch
	// recommendation, T4). A requested id absent from the result map means
	// no journal row exists for it (should not happen -- journal_entries.
	// journal_id is FK-enforced -- but callers must not assume every id
	// round-trips).
	JournalAuthMaterial(ctx context.Context, journalIDs []int64) (map[int64]core.JournalAuthMaterial, error)
}

// AttestationMetrics is the observability port AttestationService needs
// (2026-09-02 audit, tamper-evident.md M-9 / I-M8). Before it existed, a
// persistently failing anchor publish produced exactly one signal -- an
// ERROR log line -- and nothing at all in metrics, so "the ledger's history
// has had no external witness for three days" was not an alertable fact.
//
// Defined here, at the consumer, rather than as a widening of core.Metrics:
// core.Metrics is implemented by consumers, and this package cannot add
// methods to it without breaking them (golang.md: interfaces belong to the
// consumer, 1-3 methods). core.Metrics is EXPECTED to satisfy this
// interface -- NewAttestationService checks whether the configured
// implementation does, and says so when it does not.
//
// FOLLOW-UP once core.Metrics carries all three methods (D-ops's half of
// this seam, agreed 2026-09-02): the capability check in
// NewAttestationService becomes unreachable by construction -- every
// core.Metrics value would satisfy this port -- and should be replaced by a
// plain `metrics: engine.Metrics()` assignment, deleting both the warning
// and its test. The check exists so that this half can be merged and
// verified on its own; a branch that only compiles once someone else's
// branch lands is not independently reviewable.
//
// See docs/RUNBOOK.md for the alert thresholds these three feed.
type AttestationMetrics interface {
	// AttestationBatchResult reports whether one RunAttestBatch pass
	// completed. ok=false means the DB-side hash chain did not advance this
	// run.
	AttestationBatchResult(ok bool)
	// AnchorPublishResult reports whether publishing one attestation to the
	// external anchor succeeded. Repeated failures mean the ledger's own
	// database is the only remaining witness to its own history.
	AnchorPublishResult(ok bool)
	// AnchorLagSeqs reports how many attestations the anchor is behind the
	// DB chain (0 = caught up). Emitted on every run, success or failure, so
	// a stuck anchor shows up as a rising number rather than as an absence
	// of events.
	AnchorLagSeqs(lag int64)
}

// nopAttestationMetrics is the fallback when the configured core.Metrics
// does not satisfy AttestationMetrics. It is deliberately paired with a
// startup Warn in NewAttestationService: silently discarding these three
// would recreate the blind spot this port exists to close.
type nopAttestationMetrics struct{}

func (nopAttestationMetrics) AttestationBatchResult(bool) {}
func (nopAttestationMetrics) AnchorPublishResult(bool)    {}
func (nopAttestationMetrics) AnchorLagSeqs(int64)         {}

// AttestationService creates new attestations and keeps the external
// anchor caught up.
type AttestationService struct {
	store    AttestationStore
	attestor core.Attestor
	// verifier is nil when T4 is not enabled for this deployment -- every
	// attestation this service builds stays v2 (core.AttestationRootHashV2),
	// with no auth verdicts computed, exactly as before T4 existed. T4 is
	// additive/opt-in (contracts §W3-B): unlike attestor, RunAttestBatch
	// does not refuse to run without it.
	verifier core.AuthVerifier
	// anchor is nil when no external anchor has been configured --
	// attestation still proceeds (the DB-side hash chain is real and
	// checkable on its own), but nothing outside the ledger's own database
	// can detect a wholesale history rewrite until an anchor is wired in
	// (design doc §8.3 / contracts §7: the anchor carrier is a genuinely
	// unresolved deployment choice; the library ships no production
	// adapter, so tolerating nil here is what lets P6 ship at all before
	// Aaron picks one).
	anchor  core.Anchor
	logger  core.Logger
	metrics AttestationMetrics
}

// NewAttestationService creates an AttestationService. attestor is
// required -- there is no "unsigned attestation" concept in this schema
// (ledger_attestations.signature is NOT NULL with no expand-safe empty
// default, unlike P5's auth columns): RunAttestBatch refuses to run
// without one, rather than silently skipping the whole feature the way
// PostJournal treats a nil P5 Attestor. verifier is T4's opt-in switch
// (may be nil -- see the AttestationService.verifier field doc comment).
// anchor may be nil (see the AttestationService.anchor field doc comment).
func NewAttestationService(store AttestationStore, attestor core.Attestor, verifier core.AuthVerifier, anchor core.Anchor, engine *core.Engine) *AttestationService {
	logger := engine.Logger()

	// The configured core.Metrics is asked whether it can carry the three
	// attestation/anchor observations (see AttestationMetrics). An
	// implementation that predates them keeps working -- but the loss of
	// those three signals is logged rather than absorbed, because "no
	// anchor_publish_total time series exists" and "publishing is fine" look
	// identical on a dashboard (working-agreements.md §3).
	var metrics AttestationMetrics = nopAttestationMetrics{}
	if am, ok := engine.Metrics().(AttestationMetrics); ok {
		metrics = am
	} else {
		logger.Warn("service: attestation: the configured core.Metrics does not implement service.AttestationMetrics -- " +
			"attestation/anchor metrics (batch result, publish result, anchor lag) will not be emitted; " +
			"anchor failures will only appear in logs")
	}

	return &AttestationService{
		store:    store,
		attestor: attestor,
		verifier: verifier,
		anchor:   anchor,
		logger:   logger,
		metrics:  metrics,
	}
}

// SetMetrics overrides the metrics sink NewAttestationService resolved from
// the engine -- for a consumer whose core.Metrics implementation cannot be
// widened, or a test that needs to observe the emissions.
func (s *AttestationService) SetMetrics(m AttestationMetrics) {
	if m == nil {
		return
	}
	s.metrics = m
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

	// Reported on EVERY exit path below, including the early error returns
	// (M-9): a run that failed before it could sign is exactly as much a
	// "the chain did not advance" event as one that failed to publish, and a
	// counter that only increments on success cannot distinguish "nothing to
	// do" from "broken for a week".
	defer func() { s.metrics.AttestationBatchResult(err == nil) }()

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

	// P7: RFC 6962 Merkle root over the same entries, for localization
	// (core.LocateMismatches) and inclusion proofs (core.
	// GenerateInclusionProof/core.VerifyInclusion). Every attestation
	// created from migration 048 onward binds merkleRoot into rootHash
	// itself (AttestationRootHashV2, design doc §9.4) -- unlike the first
	// cut of this feature, merkle_root is NOT a separate, unsigned value
	// here: an inclusion proof's root is only trustworthy to a third
	// party if core.Attestor's signature (and, once published,
	// core.Anchor's head) actually covers it. An empty batch still
	// produces a real 32-byte RFC 6962 empty-tree root
	// (core.EmptyMerkleRoot), not the migration's never-computed
	// sentinel ('').
	merkleTree, err := core.BuildMerkleTree(entries)
	if err != nil {
		return 0, 0, fmt.Errorf("service: attestation: merkle tree: %w", err)
	}
	merkleRoot := merkleTree.Root()

	// T4 (design doc §8 extended, contracts §W3-B): verify each distinct
	// journal contributing to this batch exactly once, and bind the
	// per-entry verdict into the batch's own signed content. verifier==nil
	// means T4 is not enabled for this deployment -- verdicts stays all
	// core.JournalAuthVerdictUnknown and authVerdictDigest stays nil, so
	// this attestation is built and signed exactly as it would have been
	// before T4 existed (v2, unchanged).
	verdicts := make([]core.JournalAuthVerdict, len(entries))
	var authVerdictDigest []byte
	if s.verifier != nil {
		verdicts, err = s.computeAuthVerdicts(ctx, entries)
		if err != nil {
			return 0, 0, fmt.Errorf("service: attestation: auth verdicts: %w", err)
		}
		authVerdictDigest, err = core.AuthVerdictDigest(entries, verdicts)
		if err != nil {
			return 0, 0, fmt.Errorf("service: attestation: auth verdict digest: %w", err)
		}
	}

	var rootHash []byte
	if authVerdictDigest != nil {
		rootHash, err = core.AttestationRootHashV3(nextSeq, prevRoot, batchDigest, merkleRoot, authVerdictDigest, int64(len(entries)))
	} else {
		rootHash, err = core.AttestationRootHashV2(nextSeq, prevRoot, batchDigest, merkleRoot, int64(len(entries)))
	}
	if err != nil {
		return 0, 0, fmt.Errorf("service: attestation: root hash: %w", err)
	}

	signature, keyID, err := s.attestor.Sign(ctx, rootHash)
	if err != nil {
		return 0, 0, fmt.Errorf("service: attestation: attestor sign: %w: %w", err, core.ErrAttestorUnavailable)
	}

	entryIDs := make([]int64, len(entries))
	for i, e := range entries {
		entryIDs[i] = e.EntryID
	}
	// design doc §9.4: persist each entry's own leaf hash alongside the
	// batch's merkleRoot, so a later TAMPERED verdict can localize to
	// specific entry ids without requiring an operator-supplied external
	// reference (core.BuildMerkleTreeFromLeafHashes / core.LocateMismatches
	// in service.VerifyLedger). Same order as entryIDs -- both derive from
	// entries via the same index i.
	leafHashes := merkleTree.LeafHashes()

	result, err := s.store.InsertAttestation(ctx, core.Attestation{
		Seq:               nextSeq,
		EntryCount:        int64(len(entries)),
		BatchDigest:       batchDigest,
		MerkleRoot:        merkleRoot,
		AuthVerdictDigest: authVerdictDigest,
		PrevRoot:          prevRoot,
		RootHash:          rootHash,
		Signature:         signature,
		KeyID:             keyID,
	}, entryIDs, leafHashes, verdicts)
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
			s.metrics.AnchorPublishResult(false)
		} else if err := s.store.RecordAnchorObservation(ctx, result.Seq, result.RootHash); err != nil {
			// A successful Publish is knowledge that the anchor holds this
			// seq -- as much an observation as a Head() read, and recorded
			// for the same reason (migration 018): without it, the memory
			// would always lag one batch behind, and an anchor erased right
			// after its first publish would have nothing to contradict it.
			s.logger.Error("service: attestation: recording the anchor observation failed", "seq", result.Seq, "error", err)
			s.metrics.AnchorPublishResult(true)
		} else {
			s.metrics.AnchorPublishResult(true)
		}
	}

	return len(entries), result.Seq, nil
}

// computeAuthVerdicts runs core.VerifyJournalAuth once per DISTINCT journal
// referenced by entries (design doc §8 extended / contracts §W3-B: "worker
// 每批验一次每笔 journal 的授权") and returns a verdict per entry, same
// order/length as entries. A journal referenced by several entries in this
// batch (a multi-entry journal, or several entries from the same 2-entry
// journal landing in the same batch) is checked exactly once, not once per
// entry -- the same amortization principle T4 exists to extend all the way
// to VerifiedBalance's callers.
func (s *AttestationService) computeAuthVerdicts(ctx context.Context, entries []core.AttestedEntry) ([]core.JournalAuthVerdict, error) {
	distinctIDs := make([]int64, 0, len(entries))
	seen := make(map[int64]struct{}, len(entries))
	for _, e := range entries {
		if _, ok := seen[e.JournalID]; !ok {
			seen[e.JournalID] = struct{}{}
			distinctIDs = append(distinctIDs, e.JournalID)
		}
	}

	verdictByJournal, err := verdictsForJournals(ctx, s.store, s.verifier, distinctIDs, s.logger)
	if err != nil {
		return nil, err
	}

	verdicts := make([]core.JournalAuthVerdict, len(entries))
	for i, e := range entries {
		verdicts[i] = verdictByJournal[e.JournalID]
	}
	return verdicts, nil
}

// verdictsForJournals batch-fetches auth material for journalIDs
// (design doc §4.5's batched-fetch recommendation) and runs
// core.VerifyJournalAuth once per id -- shared by
// AttestationService.computeAuthVerdicts (build time, T4) and
// service.VerifyLedger (live drift recompute) -- both need the identical
// per-journal answer.
func verdictsForJournals(ctx context.Context, store AttestationStore, verifier core.AuthVerifier, journalIDs []int64, logger core.Logger) (map[int64]core.JournalAuthVerdict, error) {
	out := make(map[int64]core.JournalAuthVerdict, len(journalIDs))
	if len(journalIDs) == 0 {
		return out, nil
	}

	materials, err := store.JournalAuthMaterial(ctx, journalIDs)
	if err != nil {
		return nil, fmt.Errorf("service: attestation: journal auth material: %w", err)
	}

	for _, id := range journalIDs {
		material, ok := materials[id]
		if !ok {
			// Should not happen -- journal_entries.journal_id is FK-enforced
			// against journals.id -- but an unverifiable journal must never
			// be silently trusted (working-agreements §3, fail-closed).
			out[id] = core.JournalAuthVerdictUnauthorized
			if logger != nil {
				logger.Error("service: attestation: journal auth material missing", "journal_id", id)
			}
			continue
		}
		if err := core.VerifyJournalAuth(ctx, verifier, material.Input, material.EffectiveAt, material.AuthDigest, material.AuthSignature, material.AuthKeyID); err != nil {
			out[id] = core.JournalAuthVerdictUnauthorized
		} else {
			out[id] = core.JournalAuthVerdictAuthorized
		}
	}
	return out, nil
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
	anchorSeq, anchorHead, err := s.anchor.Head(ctx)
	if err != nil {
		s.logger.Error("service: attestation: anchor head unavailable, skipping catch-up this run", "error", err)
		return
	}

	// Remember what the anchor said, before doing anything about it. This is
	// the only writer of anchor_observations (migration 018): it turns a
	// later, LOWER head into evidence of a rollback instead of an
	// indistinguishable "behind, catch-up pending" (tamper-evident.md M-3).
	// Recorded on every successful read, including seq 0 -- "reachable and
	// empty" is exactly the observation a later erasure has to contradict.
	//
	// A failure to record is logged, not fatal: the anchor catch-up below is
	// what keeps the external evidence current, and skipping it because the
	// bookkeeping insert failed would trade a real defence for a
	// bookkeeping one.
	if err := s.store.RecordAnchorObservation(ctx, anchorSeq, anchorHead); err != nil {
		s.logger.Error("service: attestation: recording the anchor observation failed", "seq", anchorSeq, "error", err)
	}

	latest, err := s.store.LatestAttestation(ctx)
	if err != nil {
		s.logger.Error("service: attestation: catch-up: latest attestation failed", "error", err)
		return
	}

	// Emitted on every run, caught up or not (M-9): a gauge that only
	// appears when something is wrong is indistinguishable from a scrape
	// that failed. 0 is the healthy, expected value.
	lag := latest.Seq - anchorSeq
	if lag < 0 {
		// The anchor claims more than the DB has. VerifyLedger reports that
		// as TAMPERED; here it is not a negative "lag".
		lag = 0
	}
	s.metrics.AnchorLagSeqs(lag)

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
			s.metrics.AnchorPublishResult(false)
			return // the anchor is likely still down; don't hammer it with the rest of the page.
		}
		s.metrics.AnchorPublishResult(true)
		if err := s.store.RecordAnchorObservation(ctx, a.Seq, a.RootHash); err != nil {
			s.logger.Error("service: attestation: recording the anchor observation failed", "seq", a.Seq, "error", err)
		}
	}
}
