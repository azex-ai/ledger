package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

// AttestationStore implements service.AttestationStore (P6, design doc
// §8). Read paths (LatestAttestation, UncoveredEntries,
// EntriesForAttestation, ListAttestationsFrom) run outside any explicit
// transaction -- they are plain queries, and the caller
// (service.AttestationService) must complete signing (an external call)
// before ever opening the transaction InsertAttestation uses
// (financial.md: no external calls inside a DB transaction).
type AttestationStore struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
	dims *dimCache
}

// NewAttestationStore creates a new AttestationStore backed by a
// connection pool. InsertAttestation opens its own transaction.
func NewAttestationStore(pool *pgxpool.Pool) *AttestationStore {
	return &AttestationStore{pool: pool, q: sqlcgen.New(pool), dims: dimCacheFor(pool)}
}

// LatestAttestation returns the highest-seq attestation, or a zero
// core.Attestation (Seq == 0) if the chain has never been started -- the
// caller treats that as the genesis case (core.GenesisRoot as prev_root).
func (s *AttestationStore) LatestAttestation(ctx context.Context) (core.Attestation, error) {
	row, err := s.q.GetLatestLedgerAttestation(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Attestation{}, nil
	}
	if err != nil {
		return core.Attestation{}, fmt.Errorf("postgres: latest attestation: %w", err)
	}
	return attestationFromRow(row), nil
}

// UncoveredEntries returns up to limit entries with no entry_attestations
// row yet, oldest id first.
func (s *AttestationStore) UncoveredEntries(ctx context.Context, limit int32) ([]core.AttestedEntry, error) {
	rows, err := s.q.ListUncoveredEntries(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: uncovered entries: %w", err)
	}
	out := make([]core.AttestedEntry, len(rows))
	for i, r := range rows {
		out[i] = core.AttestedEntry{
			EntryID:          r.ID.Int64,
			JournalID:        r.JournalID,
			AccountHolder:    r.AccountHolder,
			CurrencyID:       r.CurrencyID,
			ClassificationID: r.ClassificationID,
			EntryType:        core.EntryType(r.EntryType),
			Amount:           mustNumericToDecimal(r.Amount),
			EffectiveAt:      r.EffectiveAt,
		}
	}
	return out, nil
}

// EntriesForAttestation re-fetches exactly the entries seq covered, in the
// same order UncoveredEntries would have produced them in.
func (s *AttestationStore) EntriesForAttestation(ctx context.Context, seq int64) ([]core.AttestedEntry, error) {
	rows, err := s.q.ListEntriesForAttestation(ctx, seq)
	if err != nil {
		return nil, fmt.Errorf("postgres: entries for attestation: %w", err)
	}
	out := make([]core.AttestedEntry, len(rows))
	for i, r := range rows {
		out[i] = core.AttestedEntry{
			EntryID:          r.ID.Int64,
			JournalID:        r.JournalID,
			AccountHolder:    r.AccountHolder,
			CurrencyID:       r.CurrencyID,
			ClassificationID: r.ClassificationID,
			EntryType:        core.EntryType(r.EntryType),
			Amount:           mustNumericToDecimal(r.Amount),
			EffectiveAt:      r.EffectiveAt,
		}
	}
	return out, nil
}

// ListAttestationsFrom is a paginated ascending chain walk starting at
// fromSeq (inclusive), for ledger-cli verify's seq-continuity /
// prev_root-linkage check.
func (s *AttestationStore) ListAttestationsFrom(ctx context.Context, fromSeq int64, limit int32) ([]core.Attestation, error) {
	rows, err := s.q.ListLedgerAttestationsFrom(ctx, sqlcgen.ListLedgerAttestationsFromParams{
		FromSeq: fromSeq, PageLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: list attestations: %w", err)
	}
	out := make([]core.Attestation, len(rows))
	for i, r := range rows {
		out[i] = attestationFromRow(r)
	}
	return out, nil
}

// InsertAttestation atomically inserts the attestation row (input; Seq
// must already be resolved by the caller -- see
// service.AttestationService -- input.UID is ignored, a fresh uid is
// always minted here, mirroring PostJournal's newUID() at insert time)
// and its entry_attestations coverage rows for entryIDs, in one
// transaction. entryIDs empty is valid: an empty batch still gets an
// attestation row (design doc §8.1), it just skips the entry_attestations
// insert.
//
// leafHashes, if non-nil, MUST be the same length as entryIDs -- it is
// entryIDs[i]'s stored core.AttestedLeaf.LeafHash (design doc §9.4).
// verdicts, if non-nil, MUST also be the same length -- it is entryIDs[i]'s
// cached core.JournalAuthVerdict (T4, design doc §8 extended). nil or
// wrong-length either array is normalized to a same-length slice of empty
// (”) placeholders rather than passed through mismatched: the underlying
// SQL query zips the arrays by position via WITH ORDINALITY (see
// InsertEntryAttestations's comment) -- an actual length mismatch would
// silently truncate to the shortest array's length via the INNER JOINs,
// which for a nil/empty array would mean an INSERT of entry_ids that
// silently inserts ZERO rows instead of entryIDs' full count. Normalizing
// in Go, before the query ever runs, makes that failure mode
// structurally unreachable rather than a runtime footgun callers must
// remember to avoid.
func (s *AttestationStore) InsertAttestation(ctx context.Context, input core.Attestation, entryIDs []int64, leafHashes [][]byte, verdicts []core.JournalAuthVerdict) (core.Attestation, error) {
	if len(leafHashes) != len(entryIDs) {
		leafHashes = make([][]byte, len(entryIDs))
		for i := range leafHashes {
			leafHashes[i] = []byte{}
		}
	}
	if len(verdicts) != len(entryIDs) {
		verdicts = make([]core.JournalAuthVerdict, len(entryIDs))
	}
	verdictStrings := make([]string, len(verdicts))
	for i, v := range verdicts {
		verdictStrings[i] = string(v)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return core.Attestation{}, fmt.Errorf("postgres: insert attestation: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)

	row, err := qtx.InsertLedgerAttestation(ctx, sqlcgen.InsertLedgerAttestationParams{
		Uid:               newUID(),
		Seq:               input.Seq,
		EntryCount:        input.EntryCount,
		BatchDigest:       bytesOrEmpty(input.BatchDigest),
		MerkleRoot:        bytesOrEmpty(input.MerkleRoot),
		AuthVerdictDigest: bytesOrEmpty(input.AuthVerdictDigest),
		PrevRoot:          bytesOrEmpty(input.PrevRoot),
		RootHash:          bytesOrEmpty(input.RootHash),
		Signature:         bytesOrEmpty(input.Signature),
		KeyID:             input.KeyID,
	})
	if err != nil {
		return core.Attestation{}, wrapStoreError("postgres: insert attestation: insert ledger_attestations", err)
	}

	if len(entryIDs) > 0 {
		if err := qtx.InsertEntryAttestations(ctx, sqlcgen.InsertEntryAttestationsParams{
			Seq: input.Seq, EntryIds: entryIDs, LeafHashes: leafHashes, AuthVerdicts: verdictStrings,
		}); err != nil {
			return core.Attestation{}, wrapStoreError("postgres: insert attestation: insert entry_attestations", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return core.Attestation{}, fmt.Errorf("postgres: insert attestation: commit: %w", err)
	}
	return attestationFromRow(row), nil
}

// LeafHashesForAttestation returns the stored core.AttestedLeaf rows a
// given seq covered, ascending by entry_id -- the same order
// EntriesForAttestation returns live entries in, so index i in both
// result sets refers to the same entry (design doc §9.4).
func (s *AttestationStore) LeafHashesForAttestation(ctx context.Context, seq int64) ([]core.AttestedLeaf, error) {
	rows, err := s.q.ListLeafHashesForAttestation(ctx, seq)
	if err != nil {
		return nil, fmt.Errorf("postgres: leaf hashes for attestation: %w", err)
	}
	out := make([]core.AttestedLeaf, len(rows))
	for i, r := range rows {
		out[i] = core.AttestedLeaf{EntryID: r.EntryID, LeafHash: r.LeafHash}
	}
	return out, nil
}

func attestationFromRow(row sqlcgen.LedgerAttestation) core.Attestation {
	return core.Attestation{
		UID:               pgToUID(row.Uid),
		Seq:               row.Seq,
		EntryCount:        row.EntryCount,
		BatchDigest:       row.BatchDigest,
		MerkleRoot:        row.MerkleRoot,
		AuthVerdictDigest: row.AuthVerdictDigest,
		PrevRoot:          row.PrevRoot,
		RootHash:          row.RootHash,
		Signature:         row.Signature,
		KeyID:             row.KeyID,
		CreatedAt:         row.CreatedAt,
	}
}

// JournalAuthMaterial implements service.AttestationStore.JournalAuthMaterial
// (T4, design doc §4.5's batched-fetch recommendation) by delegating to
// fetchJournalAuthMaterial (postgres/auth_material.go) -- the same batched
// fetch postgres.VerifiedBalanceStore.verifyJournalsNaively uses for its
// own, different caller (the withdrawal-time naive fallback for entries
// with no cached T4 verdict yet).
func (s *AttestationStore) JournalAuthMaterial(ctx context.Context, journalIDs []int64) (map[int64]core.JournalAuthMaterial, error) {
	return fetchJournalAuthMaterial(ctx, s.q, s.dims, journalIDs)
}
