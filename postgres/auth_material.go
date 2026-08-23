package postgres

import (
	"context"
	"fmt"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

// fetchJournalAuthMaterial batch-fetches everything core.VerifyJournalAuth
// needs for every id in journalIDs, in two round trips regardless of how
// many distinct journals are requested (design doc §4.5's batched-fetch
// recommendation, T4): one query for journal metadata
// (ListJournalsForAuthCheck) and one for every one of their entries
// (ListEntriesForJournals) -- deliberately never the entries a caller may
// already have in hand for a DIFFERENT reason (e.g. an attestation batch's
// AttestedEntry slice), because a journal's entries can straddle a batch
// boundary (design doc §8.2) and reusing a partial subset would silently
// reconstruct the wrong JournalInput.
//
// Shared by two callers that both need the identical per-journal answer:
// postgres.AttestationStore.JournalAuthMaterial (attestation build time,
// and service.VerifyLedger's live drift recompute) and
// postgres.VerifiedBalanceStore.verifyJournalsNaively (the withdrawal-time
// fallback for entries with no cached T4 verdict yet -- design doc §4.5
// flags this exact N+1 pattern as the naive path's own avoidable cost,
// distinct from the O(N) verification the semantics require).
//
// A requested id absent from the returned map means no journal row exists
// for it -- should not happen (journal_entries.journal_id is FK-enforced
// against journals.id) but callers must not assume every id round-trips.
func fetchJournalAuthMaterial(ctx context.Context, q *sqlcgen.Queries, dims *dimCache, journalIDs []int64) (map[int64]core.JournalAuthMaterial, error) {
	out := make(map[int64]core.JournalAuthMaterial, len(journalIDs))
	if len(journalIDs) == 0 {
		return out, nil
	}

	journalRows, err := q.ListJournalsForAuthCheck(ctx, journalIDs)
	if err != nil {
		return nil, fmt.Errorf("postgres: journal auth material: list journals: %w", err)
	}
	entryRows, err := q.ListEntriesForJournals(ctx, journalIDs)
	if err != nil {
		return nil, fmt.Errorf("postgres: journal auth material: list entries: %w", err)
	}

	entriesByJournal := make(map[int64][]sqlcgen.ListEntriesForJournalsRow, len(journalRows))
	for _, e := range entryRows {
		entriesByJournal[e.JournalID] = append(entriesByJournal[e.JournalID], e)
	}

	for _, j := range journalRows {
		entryInputs := make([]core.EntryInput, 0, len(entriesByJournal[j.ID]))
		for _, e := range entriesByJournal[j.ID] {
			cur, err := dims.currencyByIDOrErr(ctx, q, e.CurrencyID)
			if err != nil {
				return nil, fmt.Errorf("postgres: journal auth material: journal %d: %w", j.ID, err)
			}
			cls, err := dims.classByIDOrErr(ctx, q, e.ClassificationID)
			if err != nil {
				return nil, fmt.Errorf("postgres: journal auth material: journal %d: %w", j.ID, err)
			}
			entryInputs = append(entryInputs, core.EntryInput{
				AccountHolder:     e.AccountHolder,
				CurrencyUID:       cur.UID,
				ClassificationUID: cls.UID,
				EntryType:         core.EntryType(e.EntryType),
				Amount:            mustNumericToDecimal(e.Amount),
			})
		}

		reversalOfUID := ""
		if j.ReversalOfUid.Valid {
			reversalOfUID = pgToUID(j.ReversalOfUid)
		}

		out[j.ID] = core.JournalAuthMaterial{
			Input: core.JournalInput{
				JournalTypeUID: pgToUID(j.JournalTypeUid),
				IdempotencyKey: j.IdempotencyKey,
				ActorID:        j.ActorID,
				Source:         j.Source,
				ReversalOfUID:  reversalOfUID,
				Entries:        entryInputs,
			},
			EffectiveAt:   j.EffectiveAt,
			AuthDigest:    j.AuthDigest,
			AuthSignature: j.AuthSignature,
			AuthKeyID:     j.AuthKeyID,
		}
	}
	return out, nil
}
