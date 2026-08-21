// Package service: attest_verify.go
//
// VerifyLedger implements `ledger-cli verify`'s five steps (design doc
// §8.4, P6): pull the external anchor's head (never trust the DB alone),
// walk the attestation chain checking seq continuity / prev_root linkage /
// signatures, recompute each batch's digest from live entries, sample
// P5's per-journal signatures, and report one of
// VERIFIED | DRIFT | TAMPERED | NOT_RUN.
package service

import (
	"bytes"
	"context"
	"fmt"

	"github.com/azex-ai/ledger/core"
)

// VerifyStatus is ledger-cli verify's top-level classification.
type VerifyStatus string

const (
	// VerifyStatusVerified means every check that ran passed.
	VerifyStatusVerified VerifyStatus = "VERIFIED"
	// VerifyStatusDrift means a benign, expected inconsistency was found
	// -- currently only "the external anchor is behind the DB's chain"
	// (catch-up pending, not evidence of tampering: nothing else in the
	// chain failed).
	VerifyStatusDrift VerifyStatus = "DRIFT"
	// VerifyStatusTampered means a check found concrete evidence of
	// forgery, deletion, or history rewrite.
	VerifyStatusTampered VerifyStatus = "TAMPERED"
	// VerifyStatusNotRun means a required check could not run at all
	// (missing public key, anchor unreachable, timeout). NOT_RUN is
	// deliberately its own state, never folded into VERIFIED --
	// working-agreements §3 / this wave's P0 precedent
	// (ReconcileReport.Complete): "the check didn't run" and "the check
	// ran and passed" must never be indistinguishable.
	VerifyStatusNotRun VerifyStatus = "NOT_RUN"
)

// VerifyReport is VerifyLedger's result.
type VerifyReport struct {
	Status VerifyStatus `json:"status"`
	// Reasons lists every finding that contributed to Status -- for
	// TAMPERED/DRIFT there is always at least one; for NOT_RUN, the first
	// entry is why the required check could not run.
	Reasons          []string `json:"reasons,omitempty"`
	ChainSeqsChecked int64    `json:"chain_seqs_checked"`
	EntriesRechecked int64    `json:"entries_rechecked"`
	JournalsSampled  int64    `json:"journals_sampled"`
	// MismatchedEntryIDs maps seq -> the specific entry ids
	// core.LocateMismatches found within that batch (design doc §9.1),
	// present only for seqs where a content mismatch was found AND
	// VerifyConfig.ReferenceEntries supplied a trusted reference for that
	// seq. A TAMPERED seq with no entry here means localization was not
	// attempted (no reference available), not that nothing was found --
	// see Reasons for that seq's text.
	MismatchedEntryIDs map[int64][]int64 `json:"mismatched_entry_ids,omitempty"`
}

// VerifyConfig tunes VerifyLedger. Zero value uses sensible defaults via
// DefaultVerifyConfig.
type VerifyConfig struct {
	// JournalSampleSize is how many of the most recent journals step 4
	// samples for a valid P5 signature. default: 20
	JournalSampleSize int32
	// ChainPageSize is the page size for walking the attestation chain in
	// step 2. default: 500
	ChainPageSize int32
	// ReferenceEntries, when non-nil, supplies an externally-trusted set
	// of entries for a given seq (e.g. loaded from a pre-incident backup,
	// a second independent replica, or a WAL-based point-in-time
	// recovery) so a content mismatch at that seq can be localized to
	// specific entry ids (design doc §9.1) instead of only naming the
	// seq. ok=false (or nil ReferenceEntries) means no reference is
	// available -- VerifyLedger then reports the seq as TAMPERED without
	// an entry list, never a fabricated one. Supplying this is an
	// operational/forensics decision this package cannot default (same
	// reasoning as core.LocateMismatches's own doc comment) -- ledger-cli
	// verify's --reference-dir flag is the reference implementation of
	// this hook, not the only possible one.
	ReferenceEntries func(seq int64) (entries []core.AttestedEntry, ok bool)
}

// DefaultVerifyConfig returns VerifyConfig's defaults.
func DefaultVerifyConfig() VerifyConfig {
	return VerifyConfig{JournalSampleSize: 20, ChainPageSize: 500}
}

// VerifyLedger runs the five-step verification (design doc §8.4).
// anchor and verifier are both required for anything beyond NOT_RUN --
// nil either one and VerifyLedger returns NOT_RUN immediately, never a
// partial VERIFIED (fail-closed).
func VerifyLedger(ctx context.Context, store AttestationStore, anchor core.Anchor, verifier core.AuthVerifier, journals core.QueryProvider, cfg VerifyConfig) VerifyReport {
	if cfg.JournalSampleSize <= 0 {
		cfg.JournalSampleSize = DefaultVerifyConfig().JournalSampleSize
	}
	if cfg.ChainPageSize <= 0 {
		cfg.ChainPageSize = DefaultVerifyConfig().ChainPageSize
	}

	// Step 1: pull the anchor's head. Never trust the DB for this value.
	if anchor == nil {
		return VerifyReport{Status: VerifyStatusNotRun, Reasons: []string{"no Anchor configured"}}
	}
	if verifier == nil {
		return VerifyReport{Status: VerifyStatusNotRun, Reasons: []string{"no AuthVerifier configured"}}
	}
	anchorSeq, anchorHead, err := anchor.Head(ctx)
	if err != nil {
		return VerifyReport{Status: VerifyStatusNotRun, Reasons: []string{fmt.Sprintf("anchor head unavailable: %v", err)}}
	}

	report := VerifyReport{}
	var reasons []string
	tampered := func(format string, args ...any) {
		reasons = append(reasons, fmt.Sprintf(format, args...))
	}

	// Step 2 + 3 combined per seq: chain continuity, prev_root linkage,
	// signature, and batch digest recomputation from live entries.
	var (
		expectedSeq int64 = 1
		prevRoot          = core.GenesisRoot
		maxSeqSeen  int64
	)
	for {
		page, err := store.ListAttestationsFrom(ctx, expectedSeq, cfg.ChainPageSize)
		if err != nil {
			return VerifyReport{Status: VerifyStatusNotRun, Reasons: []string{fmt.Sprintf("list attestations from seq %d: %v", expectedSeq, err)}}
		}
		if len(page) == 0 {
			break
		}
		for _, a := range page {
			report.ChainSeqsChecked++
			if a.Seq != expectedSeq {
				tampered("seq gap: expected %d, got %d", expectedSeq, a.Seq)
				// Continue the walk from a.Seq's own successor (set below)
				// rather than aborting -- a single gap should not hide
				// later tampering the rest of the chain might reveal.
			}
			if !bytes.Equal(a.PrevRoot, prevRoot) {
				tampered("prev_root mismatch at seq %d", a.Seq)
			}
			if err := verifier.Verify(ctx, a.RootHash, a.Signature, a.KeyID); err != nil {
				tampered("invalid signature at seq %d: %v", a.Seq, err)
			}

			entries, err := store.EntriesForAttestation(ctx, a.Seq)
			if err != nil {
				return VerifyReport{Status: VerifyStatusNotRun, Reasons: []string{fmt.Sprintf("recheck entries for seq %d: %v", a.Seq, err)}}
			}
			report.EntriesRechecked += int64(len(entries))
			contentMismatch := false
			if int64(len(entries)) != a.EntryCount {
				tampered("seq %d: entry_count=%d but only %d entries still exist (deleted row?)", a.Seq, a.EntryCount, len(entries))
				contentMismatch = true
			}
			recomputedDigest, err := core.CanonicalBatchDigest(entries)
			if err != nil {
				tampered("seq %d: recompute batch digest: %v", a.Seq, err)
				contentMismatch = true
			} else if !bytes.Equal(recomputedDigest, a.BatchDigest) {
				tampered("seq %d: batch_digest mismatch (entry content changed after attestation)", a.Seq)
				contentMismatch = true
			} else {
				recomputedRoot, err := core.AttestationRootHash(a.Seq, prevRoot, recomputedDigest, a.EntryCount)
				if err != nil || !bytes.Equal(recomputedRoot, a.RootHash) {
					tampered("seq %d: root_hash does not match its own stored batch_digest/prev_root", a.Seq)
				}
			}

			// P7: recompute the RFC 6962 Merkle root and compare against
			// the stored value. Empty a.MerkleRoot means this row
			// predates merkle_root being computed (migration 048's
			// never-computed sentinel, distinct from core.EmptyMerkleRoot
			// -- an EMPTY BATCH's real tree root) -- skip, don't treat
			// absence as a mismatch (same "empty = not this check's
			// concern" treatment step 4 already gives an unsigned
			// journal's empty AuthKeyID).
			if len(a.MerkleRoot) > 0 {
				merkleTree, err := core.BuildMerkleTree(entries)
				if err != nil {
					tampered("seq %d: recompute merkle root: %v", a.Seq, err)
					contentMismatch = true
				} else if !bytes.Equal(merkleTree.Root(), a.MerkleRoot) {
					tampered("seq %d: merkle_root mismatch (entry content or order changed after attestation)", a.Seq)
					contentMismatch = true
				}
			}

			// P7 localization (design doc §9.1): a content mismatch only
			// names the seq so far -- narrow it to specific entry ids when
			// the caller supplied a trusted reference for this seq.
			// core.LocateMismatches requires a second FULL tree; this
			// package has no way to produce one on its own (only the
			// aggregate merkle_root is persisted, deliberately -- see
			// core/merkle.go's LocateMismatches doc comment), so silently
			// skip (no fabricated entry list) when no reference is
			// available or its leaf count does not match.
			if contentMismatch && cfg.ReferenceEntries != nil {
				if refEntries, ok := cfg.ReferenceEntries(a.Seq); ok {
					if mismatched := locateMismatchedEntryIDs(refEntries, entries); len(mismatched) > 0 {
						if report.MismatchedEntryIDs == nil {
							report.MismatchedEntryIDs = make(map[int64][]int64)
						}
						report.MismatchedEntryIDs[a.Seq] = mismatched
						tampered("seq %d: localized to entry id(s) %v", a.Seq, mismatched)
					}
				}
			}

			if a.Seq == anchorSeq && !bytes.Equal(a.RootHash, anchorHead) {
				tampered("seq %d: DB root_hash does not match the externally anchored head", a.Seq)
			}

			prevRoot = a.RootHash
			maxSeqSeen = a.Seq
			expectedSeq = a.Seq + 1
		}
		if int32(len(page)) < cfg.ChainPageSize {
			break
		}
	}

	if anchorSeq > maxSeqSeen {
		tampered("anchor knows about seq %d but the DB chain only reaches seq %d", anchorSeq, maxSeqSeen)
	}

	// Step 4: sample the most recent journals' P5 signatures.
	journalList, _, err := journals.ListJournals(ctx, "", cfg.JournalSampleSize)
	if err != nil {
		return VerifyReport{Status: VerifyStatusNotRun, Reasons: []string{fmt.Sprintf("list journals for sampling: %v", err)}}
	}
	for _, j := range journalList {
		if j.AuthKeyID == "" {
			continue // never signed (Attestor not configured at post time) -- not this check's concern, I-26's own scope note covers it
		}
		report.JournalsSampled++
		_, entries, err := journals.GetJournal(ctx, j.UID)
		if err != nil {
			tampered("journal %s: refetch for sampling: %v", j.UID, err)
			continue
		}
		input := core.JournalInput{
			JournalTypeUID: j.JournalTypeUID,
			IdempotencyKey: j.IdempotencyKey,
			ActorID:        j.ActorID,
			Source:         j.Source,
			EventUID:       j.EventUID,
			ReversalOfUID:  j.ReversalOfUID,
		}
		for _, e := range entries {
			input.Entries = append(input.Entries, core.EntryInput{
				AccountHolder: e.AccountHolder, CurrencyUID: e.CurrencyUID,
				ClassificationUID: e.ClassificationUID, EntryType: e.EntryType, Amount: e.Amount,
			})
		}
		if err := core.VerifyJournalAuth(ctx, verifier, input, j.EffectiveAt, j.AuthDigest, j.AuthSignature, j.AuthKeyID); err != nil {
			tampered("journal %s: signature verification failed: %v", j.UID, err)
		}
	}

	switch {
	case len(reasons) > 0:
		report.Status = VerifyStatusTampered
		report.Reasons = reasons
	case anchorSeq < maxSeqSeen:
		report.Status = VerifyStatusDrift
		report.Reasons = []string{fmt.Sprintf("anchor is behind the DB chain by %d attestation(s) (catch-up pending)", maxSeqSeen-anchorSeq)}
	default:
		report.Status = VerifyStatusVerified
	}
	return report
}

// locateMismatchedEntryIDs builds an RFC 6962 tree over each of reference
// and actual (both must already be in the same canonical order --
// ascending EntryID, the order every AttestationStore read path in this
// package produces) and returns the actual-side entry ids
// core.LocateMismatches finds diverging. Returns nil (not an error) when
// the two sets have different leaf counts -- localization requires equal
// leaf counts (core.LocateMismatches's own contract); an entry-count
// mismatch is a different finding (already reported by this file's
// "entry_count=%d but only %d entries still exist" check) that a leaf-count
// mismatch here would not add anything to.
func locateMismatchedEntryIDs(reference, actual []core.AttestedEntry) []int64 {
	if len(reference) != len(actual) {
		return nil
	}
	refTree, err := core.BuildMerkleTree(reference)
	if err != nil {
		return nil
	}
	actualTree, err := core.BuildMerkleTree(actual)
	if err != nil {
		return nil
	}
	indices, err := core.LocateMismatches(refTree, actualTree)
	if err != nil || len(indices) == 0 {
		return nil
	}
	ids := make([]int64, len(indices))
	for i, idx := range indices {
		ids[i] = actual[idx].EntryID
	}
	return ids
}
