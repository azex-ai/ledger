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
			if int64(len(entries)) != a.EntryCount {
				tampered("seq %d: entry_count=%d but only %d entries still exist (deleted row?)", a.Seq, a.EntryCount, len(entries))
			}
			recomputedDigest, err := core.CanonicalBatchDigest(entries)
			if err != nil {
				tampered("seq %d: recompute batch digest: %v", a.Seq, err)
			} else if !bytes.Equal(recomputedDigest, a.BatchDigest) {
				tampered("seq %d: batch_digest mismatch (entry content changed after attestation)", a.Seq)
			} else {
				recomputedRoot, err := core.AttestationRootHash(a.Seq, prevRoot, recomputedDigest, a.EntryCount)
				if err != nil || !bytes.Equal(recomputedRoot, a.RootHash) {
					tampered("seq %d: root_hash does not match its own stored batch_digest/prev_root", a.Seq)
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
