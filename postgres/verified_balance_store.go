package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

var _ core.VerifiedBalanceReader = (*VerifiedBalanceStore)(nil)

// VerifiedBalanceStore implements core.VerifiedBalanceReader (design doc §7,
// contracts §W2-1/§W2-2). This is the naive reference implementation the
// contract deliberately asked for first: verify every distinct journal
// contributing an entry to the dimension individually
// (core.VerifyJournalAuth), then trust CheckpointIntegrityStore's
// entries-only recompute for the number once every one of them checks out.
// A batch-attestation-backed implementation (T4, not started this wave)
// can replace this file's body without changing core.VerifiedBalanceReader
// or this type's exported shape — defining the port before picking an
// implementation is the whole point (contracts §W2-2).
type VerifiedBalanceStore struct {
	// pool is non-nil only in pool mode; nil signals tx mode.
	pool      *pgxpool.Pool
	db        DBTX
	q         *sqlcgen.Queries
	dims      *dimCache
	verifier  core.AuthVerifier
	recompute *CheckpointIntegrityStore
}

// NewVerifiedBalanceStore creates a VerifiedBalanceStore backed by a
// connection pool. verifier is exactly the core.AuthVerifier passed to
// ledger.WithAttestor (may be nil if that option was never called).
//
// A nil verifier is not an error at construction time — mirroring
// (*ledger.Service).AuthVerifier's own "nil means never configured"
// contract, this store does not invent a default. It simply means every
// dimension with at least one contributing journal comes back
// core.ErrUnauthorizedJournal (nothing can be confirmed authorized without
// a public key to check signatures against); a dimension with zero
// contributing journals still returns a defined zero balance (see
// VerifiedBalance's doc comment on that vacuous case, which needs no
// verifier at all).
func NewVerifiedBalanceStore(pool *pgxpool.Pool, verifier core.AuthVerifier) *VerifiedBalanceStore {
	return &VerifiedBalanceStore{
		pool:      pool,
		db:        pool,
		q:         sqlcgen.New(pool),
		dims:      dimCacheFor(pool),
		verifier:  verifier,
		recompute: NewCheckpointIntegrityStore(pool),
	}
}

// WithDB returns a clone of the store bound to an existing transaction.
func (s *VerifiedBalanceStore) WithDB(db DBTX) *VerifiedBalanceStore {
	return &VerifiedBalanceStore{
		dims:      s.dims,
		pool:      nil, // tx mode
		db:        db,
		q:         sqlcgen.New(db),
		verifier:  s.verifier,
		recompute: s.recompute.WithDB(db),
	}
}

// VerifiedBalance implements core.VerifiedBalanceReader. See that
// interface's doc comment for the full contract (UNDEFINED semantics,
// vacuous zero-journal case, mechanism-not-policy scope).
func (s *VerifiedBalanceStore) VerifiedBalance(ctx context.Context, holder int64, currencyUID, classificationUID string) (decimal.Decimal, error) {
	cur, err := s.dims.currencyByUIDOrErr(ctx, s.q, currencyUID)
	if err != nil {
		return decimal.Zero, err
	}
	cls, err := s.dims.classByUIDOrErr(ctx, s.q, classificationUID)
	if err != nil {
		return decimal.Zero, err
	}

	journalIDs, err := s.q.ListContributingJournalIDs(ctx, sqlcgen.ListContributingJournalIDsParams{
		AccountHolder:    holder,
		CurrencyID:       cur.ID,
		ClassificationID: cls.ID,
	})
	if err != nil {
		return decimal.Zero, fmt.Errorf("postgres: verified balance: list contributing journals: %w", err)
	}

	for _, journalID := range journalIDs {
		if s.verifier == nil {
			return decimal.Zero, fmt.Errorf("postgres: verified balance: no AuthVerifier configured (ledger.WithAttestor was never called), so journal cannot be confirmed authorized: %w", core.ErrUnauthorizedJournal)
		}

		row, err := s.q.GetJournal(ctx, journalID)
		if err != nil {
			return decimal.Zero, fmt.Errorf("postgres: verified balance: get journal: %w", err)
		}
		journal, err := journalFromRow(ctx, s.dims, s.q, row)
		if err != nil {
			return decimal.Zero, fmt.Errorf("postgres: verified balance: resolve journal: %w", err)
		}

		entryRows, err := s.q.ListJournalEntries(ctx, journalID)
		if err != nil {
			return decimal.Zero, fmt.Errorf("postgres: verified balance: list entries for journal %s: %w", journal.UID, err)
		}

		entries := make([]core.Entry, len(entryRows))
		for i, e := range entryRows {
			entry, err := entryCore(ctx, s.dims, s.q, e.JournalUid, e.AccountHolder, e.CurrencyID, e.ClassificationID, e.EntryType, e.Amount, e.EffectiveAt, e.CreatedAt)
			if err != nil {
				return decimal.Zero, fmt.Errorf("postgres: verified balance: resolve entry for journal %s: %w", journal.UID, err)
			}
			entries[i] = *entry
		}
		input := core.JournalInputFromRecord(*journal, entries)

		if err := core.VerifyJournalAuth(ctx, s.verifier, input, journal.EffectiveAt, journal.AuthDigest, journal.AuthSignature, journal.AuthKeyID); err != nil {
			return decimal.Zero, fmt.Errorf("postgres: verified balance: journal %s: %w", journal.UID, err)
		}
	}

	// Every contributing journal (if any) passed authorization -- trust the
	// same trusted, checkpoint-independent recompute RecomputeBalance uses.
	return s.recompute.RecomputeBalance(ctx, holder, currencyUID, classificationUID)
}
