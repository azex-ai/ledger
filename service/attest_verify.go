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
	// JournalsUnsignedTxMode counts sampled journals posted inside a
	// caller's transaction, where no safe point to sign exists. Reported
	// rather than flagged -- legitimate, but a consumer relying on
	// verification should know how much of the ledger it cannot speak for.
	JournalsUnsignedTxMode int64 `json:"journals_unsigned_tx_mode"`
	// MismatchedEntryIDs maps seq -> the specific entry ids
	// core.LocateMismatches found within that batch (design doc §9.1/§9.4),
	// present only for seqs where a content mismatch was found and could be
	// localized -- preferring the self-contained path (migration 048's
	// entry_attestations.leaf_hash, no operator input required) and
	// falling back to VerifyConfig.ReferenceEntries when self-contained
	// data is unavailable (e.g. a row that predates the leaf_hash column).
	// A TAMPERED seq with no entry here means localization was not
	// attempted or found nothing usable, not that nothing was found --
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
	// ReferenceEntries is the FALLBACK localization path, used only when
	// the self-contained one (migration 048's entry_attestations.leaf_hash,
	// tried first) is unavailable for a seq -- e.g. a row that predates the
	// leaf_hash column. When non-nil, it supplies an externally-trusted set
	// of entries for a given seq (e.g. loaded from a pre-incident backup, a
	// second independent replica, or a WAL-based point-in-time recovery) so
	// a content mismatch at that seq can still be localized to specific
	// entry ids (design doc §9.1) instead of only naming the seq. ok=false
	// (or nil ReferenceEntries) means no reference is available -- and
	// self-contained localization was also unavailable -- so VerifyLedger
	// reports the seq as TAMPERED without an entry list, never a
	// fabricated one. ledger-cli verify's --reference-dir flag is the
	// reference implementation of this hook, not the only possible one.
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

			if recomputedDigest, err := core.CanonicalBatchDigest(entries); err != nil {
				tampered("seq %d: recompute batch digest: %v", a.Seq, err)
				contentMismatch = true
			} else if !bytes.Equal(recomputedDigest, a.BatchDigest) {
				tampered("seq %d: batch_digest mismatch (entry content changed after attestation)", a.Seq)
				contentMismatch = true
			}

			// isV2: empty a.MerkleRoot means this row predates merkle_root
			// being computed (migration 048's never-computed sentinel,
			// distinct from core.EmptyMerkleRoot -- an EMPTY BATCH's real
			// tree root) and kept its original v1 root_hash semantics
			// forever (design doc §9.4 -- see migration 048's header for
			// why a v1 row is never retroactively upgraded).
			// isV3 (T4, design doc §8 extended): empty a.AuthVerdictDigest
			// means either this row predates migration 054, or the
			// AttestationService that built it had no AuthVerifier
			// configured -- either way it kept its v1/v2 root_hash
			// semantics forever, same reasoning as isV2. A v3 row is
			// always ALSO v2 (RunAttestBatch only computes
			// AuthVerdictDigest when it also has a real MerkleRoot -- P7
			// shipped unconditionally before T4 existed), so isV3 implies
			// isV2, never the reverse.
			isV2 := len(a.MerkleRoot) > 0
			isV3 := len(a.AuthVerdictDigest) > 0
			// storedLeafEntryIDs/storedLeafHashes are populated only once
			// the stored entry_attestations.leaf_hash values have been
			// confirmed self-consistent with a.MerkleRoot below -- an
			// unconfirmed set must never be used for localization, or a
			// tampered leaf_hash could mislead an on-call responder about
			// which entry actually changed.
			var storedLeafEntryIDs []int64
			var storedLeafHashes [][]byte

			if isV2 {
				if merkleTree, err := core.BuildMerkleTree(entries); err != nil {
					tampered("seq %d: recompute merkle root: %v", a.Seq, err)
					contentMismatch = true
				} else if !bytes.Equal(merkleTree.Root(), a.MerkleRoot) {
					tampered("seq %d: merkle_root mismatch (entry content or order changed after attestation)", a.Seq)
					contentMismatch = true
				}

				// design doc §9.4: entry_attestations.leaf_hash's own
				// tamper-evidence -- a DIFFERENT failure mode than the
				// check above. That one catches live journal_entries
				// content diverging from the signed merkle_root; this one
				// catches a stored leaf_hash edited directly (entries and
				// merkle_root both left alone), which the live-entries
				// recompute above cannot see at all.
				if storedLeaves, err := store.LeafHashesForAttestation(ctx, a.Seq); err != nil {
					tampered("seq %d: fetch stored leaf hashes: %v", a.Seq, err)
				} else if len(storedLeaves) > 0 {
					hashes := make([][]byte, len(storedLeaves))
					allPresent := true
					for i, l := range storedLeaves {
						if len(l.LeafHash) == 0 {
							allPresent = false
							break
						}
						hashes[i] = l.LeafHash
					}
					if allPresent {
						if storedTree, err := core.BuildMerkleTreeFromLeafHashes(hashes); err != nil {
							tampered("seq %d: rebuild tree from stored leaf hashes: %v", a.Seq, err)
						} else if !bytes.Equal(storedTree.Root(), a.MerkleRoot) {
							tampered("seq %d: entry_attestations.leaf_hash inconsistent with attested merkle_root (leaf-level tamper)", a.Seq)
							contentMismatch = true
						} else {
							// Confirmed self-consistent -- safe to use for
							// self-contained localization below.
							storedLeafHashes = hashes
							storedLeafEntryIDs = make([]int64, len(storedLeaves))
							for i, l := range storedLeaves {
								storedLeafEntryIDs[i] = l.EntryID
							}
						}
					}
				}
			}

			// T4 (design doc §8 extended): recompute AuthVerdictDigest from a
			// LIVE core.VerifyJournalAuth pass over this batch's distinct
			// journals (the same amortized check RunAttestBatch ran at
			// attestation time) and compare it against the stored, signed
			// value. This is the drift check that lets the fast,
			// withdrawal-time path (postgres.VerifiedBalanceStore) trust a
			// cached entry_attestations.auth_verdict without redoing this
			// work on every call: a mismatch here (a journal's stored auth
			// columns edited without a valid re-sign, or a fabricated
			// entry_attestations row claiming a verdict that was never
			// actually computed) is exactly the class of tamper P6's
			// periodic full verify exists to catch, same as the batch_digest
			// and merkle_root checks above.
			if isV3 {
				distinctJournalIDs := make([]int64, 0, len(entries))
				seenJournal := make(map[int64]struct{}, len(entries))
				for _, e := range entries {
					if _, ok := seenJournal[e.JournalID]; !ok {
						seenJournal[e.JournalID] = struct{}{}
						distinctJournalIDs = append(distinctJournalIDs, e.JournalID)
					}
				}
				if verdictByJournal, err := verdictsForJournals(ctx, store, verifier, distinctJournalIDs, nil); err != nil {
					tampered("seq %d: recompute auth verdicts: %v", a.Seq, err)
				} else {
					liveVerdicts := make([]core.JournalAuthVerdict, len(entries))
					for i, e := range entries {
						liveVerdicts[i] = verdictByJournal[e.JournalID]
					}
					if recomputedVerdictDigest, err := core.AuthVerdictDigest(entries, liveVerdicts); err != nil {
						tampered("seq %d: recompute auth verdict digest: %v", a.Seq, err)
					} else if !bytes.Equal(recomputedVerdictDigest, a.AuthVerdictDigest) {
						tampered("seq %d: auth_verdict_digest mismatch (a cached journal authorization verdict no longer matches a live recheck)", a.Seq)
					}

					// A negative verdict is a finding in its own right, not
					// merely something to keep consistent.
					//
					// The check above only asks whether the verdicts still
					// say what they said when this batch was signed. A forged
					// journal answers "unauthorized" both times, so the digest
					// matched and verification reported VERIFIED with a forged
					// credit sitting in the ledger -- while
					// JournalAuthVerdictUnauthorized's own definition is "a
					// forged or tampered journal was live in the ledger at
					// attestation time". The evidence was recorded, signed and
					// anchored, and then never read.
					//
					// Consistency is not health. Anything an operator would
					// act on has to be reported, not just kept stable.
					unauthorized := make([]int64, 0)
					seenBad := make(map[int64]struct{})
					for _, id := range distinctJournalIDs {
						if verdictByJournal[id] != core.JournalAuthVerdictUnauthorized {
							continue
						}
						if _, dup := seenBad[id]; dup {
							continue
						}
						seenBad[id] = struct{}{}
						unauthorized = append(unauthorized, id)
					}
					if len(unauthorized) > 0 {
						tampered("seq %d: %d attested journal(s) carry an UNAUTHORIZED authorization verdict "+
							"(internal journal ids %v) -- a journal live in this batch has no valid signature",
							a.Seq, len(unauthorized), unauthorized)
					}
				}
			}

			// P7 localization (design doc §9.1/§9.4): a content mismatch
			// only names the seq so far. Prefer the self-contained path
			// (stored, verified-consistent leaf hashes -- no operator
			// input needed); fall back to an operator-supplied external
			// reference only when self-contained data was unavailable
			// (e.g. a row that predates the leaf_hash column). Silently
			// skip (no fabricated entry list) if neither path produces a
			// usable localization.
			if contentMismatch {
				var localized []int64
				if len(storedLeafHashes) > 0 && len(storedLeafHashes) == len(entries) {
					localized = localizeUsingStoredLeafHashes(storedLeafEntryIDs, storedLeafHashes, entries)
				}
				if len(localized) == 0 && cfg.ReferenceEntries != nil {
					if refEntries, ok := cfg.ReferenceEntries(a.Seq); ok {
						localized = locateMismatchedEntryIDs(refEntries, entries)
					}
				}
				if len(localized) > 0 {
					if report.MismatchedEntryIDs == nil {
						report.MismatchedEntryIDs = make(map[int64][]int64)
					}
					report.MismatchedEntryIDs[a.Seq] = localized
					tampered("seq %d: localized to entry id(s) %v", a.Seq, localized)
				}
			}

			// root_hash self-consistency: recompute from the STORED fields
			// (a.BatchDigest / a.MerkleRoot -- NOT the live-entries
			// recompute above, a separate concern) under whichever
			// formula this row's version uses, and confirm it still
			// equals the stored, signed root_hash. Deliberately
			// unconditional -- NOT gated on whether those stored fields
			// also matched live data above: this is what catches a.
			// MerkleRoot (or a.BatchDigest) being edited to a value that
			// no longer matches live entries EITHER, root_hash's own
			// bytes staying whatever they were signed for. Gating this on
			// "stored fields already verified against live" would make
			// exactly that edit invisible to this check (it would just
			// silently re-derive a root_hash from the tampered value and
			// never notice root_hash itself no longer agrees) --
			// confirmed by temporarily reintroducing that gate and
			// observing TestVerifyLedger_TamperedMerkleRootAlone lose its
			// root_hash finding.
			{
				var recomputedRootHash []byte
				var err error
				switch {
				case isV3:
					recomputedRootHash, err = core.AttestationRootHashV3(a.Seq, prevRoot, a.BatchDigest, a.MerkleRoot, a.AuthVerdictDigest, a.EntryCount)
				case isV2:
					recomputedRootHash, err = core.AttestationRootHashV2(a.Seq, prevRoot, a.BatchDigest, a.MerkleRoot, a.EntryCount)
				default:
					recomputedRootHash, err = core.AttestationRootHash(a.Seq, prevRoot, a.BatchDigest, a.EntryCount)
				}
				if err != nil || !bytes.Equal(recomputedRootHash, a.RootHash) {
					tampered("seq %d: root_hash does not match its own stored fields", a.Seq)
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
	// Journals in the sample that were never signed. Silence here was the
	// original behaviour and it was wrong: a forged row inserted by direct
	// SQL takes auth_status's column default, so it is exactly the shape
	// this branch used to skip -- verification could report VERIFIED with a
	// forged credit live in the ledger.
	//
	// unsigned_tx_mode is genuinely legitimate: those journals were posted
	// inside a caller's transaction, where there is no safe point left to
	// call out to a signer. They are counted, not flagged.
	//
	// unsigned_no_attestor is not legitimate here. VerifyLedger has already
	// returned NOT_RUN if no verifier is configured, so reaching this line
	// means signing is wired -- and a journal claiming no attestor existed
	// is either a forgery or predates the key. Both need an operator to look;
	// neither should read as verified.
	var unsignedTxMode, unsignedNoAttestor int64
	for _, j := range journalList {
		if j.AuthStatus != core.AuthStatusSigned {
			switch j.AuthStatus {
			case core.AuthStatusUnsignedTxMode:
				unsignedTxMode++
			default:
				unsignedNoAttestor++
			}
			continue
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
	if unsignedNoAttestor > 0 {
		tampered("%d of the %d most recent journal(s) carry no signature (auth_status=%s) while signing is configured -- "+
			"either forged, or posted before the key was wired; an operator has to tell which",
			unsignedNoAttestor, len(journalList), core.AuthStatusUnsignedNoAttestor)
	}
	report.JournalsUnsignedTxMode = unsignedTxMode

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

// localizeUsingStoredLeafHashes is the self-contained localization path
// (design doc §9.4): storedHashes are a batch's persisted
// entry_attestations.leaf_hash values (already confirmed, by the caller,
// to rebuild to the batch's own stored merkle_root -- this function does
// not re-verify that), storedEntryIDs is the parallel entry_id for each
// (same order), and live is the same seq's current journal_entries
// content. Returns the actual-side entry ids core.LocateMismatches finds
// diverging, or nil if the two sets are not the same size (a count
// mismatch is a different, already-reported finding) or nothing diverges
// at the leaf level (e.g. only the row's own root_hash/signature was
// tampered, which the caller's other checks already catch).
func localizeUsingStoredLeafHashes(storedEntryIDs []int64, storedHashes [][]byte, live []core.AttestedEntry) []int64 {
	if len(storedHashes) != len(live) {
		return nil
	}
	refTree, err := core.BuildMerkleTreeFromLeafHashes(storedHashes)
	if err != nil {
		return nil
	}
	actualTree, err := core.BuildMerkleTree(live)
	if err != nil {
		return nil
	}
	indices, err := core.LocateMismatches(refTree, actualTree)
	if err != nil || len(indices) == 0 {
		return nil
	}
	ids := make([]int64, len(indices))
	for i, idx := range indices {
		ids[i] = storedEntryIDs[idx]
	}
	return ids
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
