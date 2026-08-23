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
// contracts §W2-1/§W2-2). T4 (design doc §8 extended, contracts §W3-B)
// replaced this file's naive-only body with an attestation-backed fast
// path, WITHOUT changing core.VerifiedBalanceReader or this type's
// exported shape — defining the port before picking an implementation was
// the whole point (contracts §W2-2), and it paid off exactly as intended.
//
// For each entry contributing to the dimension, VerifiedBalance first asks
// whether an attestation batch already cached a core.JournalAuthVerdict for
// its journal (entry_attestations.auth_verdict, T4's migration 054):
//   - core.JournalAuthVerdictAuthorized -> trusted, zero extra round trips
//     or crypto calls for that journal.
//   - core.JournalAuthVerdictUnauthorized -> the whole balance is UNDEFINED
//     immediately (I-32's fail-closed rule); no need to even look at the
//     rest of the dimension's journals.
//   - core.JournalAuthVerdictUnknown (uncovered tail, or an
//     entry_attestations row that predates migration 054) -> fall back to
//     the ORIGINAL naive per-journal check (verifyJournalsNaively below) --
//     exactly the behavior this file had before T4 existed, bounded to
//     however many journals have not yet been swept into an attestation
//     batch (the attestation interval).
//
// This is why T4 is a real algorithmic win, not just a cache: the naive
// path (.local/bench-verify-2026-08-23.md) pays a `journals JOIN
// journal_entries` round trip plus a fresh core.VerifyJournalAuth call for
// EVERY contributing journal, EVERY call. Once a journal's entries are
// attested, every later VerifiedBalance call for that dimension pays
// nothing extra for it -- the crypto/DB work happened once, in
// service.AttestationService.RunAttestBatch, and its result is cached in
// content an external anchor (P6/I-28) already protects.
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

	rows, err := s.q.ListContributingEntryVerdicts(ctx, sqlcgen.ListContributingEntryVerdictsParams{
		AccountHolder:    holder,
		CurrencyID:       cur.ID,
		ClassificationID: cls.ID,
	})
	if err != nil {
		return decimal.Zero, fmt.Errorf("postgres: verified balance: list contributing entries: %w", err)
	}

	// T4 (design doc §8 extended): partition contributing journals by
	// their cached verdict. A journal with a mix of Authorized and Unknown
	// rows (its entries straddled an attestation batch boundary, design
	// doc §8.2) ends up in BOTH sets below -- authorizedByCachedVerdict
	// wins (needsLiveCheck is pruned against it after this loop), since one
	// Authorized row already proves the naive live check would reach the
	// same answer for the whole journal; there is no need to spend the
	// round trip this optimization exists to avoid on it.
	authorizedByCachedVerdict := make(map[int64]struct{})
	needsLiveCheck := make(map[int64]struct{})
	for _, r := range rows {
		switch core.JournalAuthVerdict(r.Verdict) {
		case core.JournalAuthVerdictAuthorized:
			authorizedByCachedVerdict[r.JournalID] = struct{}{}
		case core.JournalAuthVerdictUnauthorized:
			return decimal.Zero, fmt.Errorf("postgres: verified balance: journal id %d: cached attestation verdict is unauthorized: %w", r.JournalID, core.ErrUnauthorizedJournal)
		default: // core.JournalAuthVerdictUnknown: uncovered tail, or predates migration 054.
			needsLiveCheck[r.JournalID] = struct{}{}
		}
	}
	for journalID := range authorizedByCachedVerdict {
		delete(needsLiveCheck, journalID)
	}

	if len(needsLiveCheck) > 0 {
		if s.verifier == nil {
			return decimal.Zero, fmt.Errorf("postgres: verified balance: no AuthVerifier configured (ledger.WithAttestor was never called), so journal cannot be confirmed authorized: %w", core.ErrUnauthorizedJournal)
		}
		journalIDs := make([]int64, 0, len(needsLiveCheck))
		for journalID := range needsLiveCheck {
			journalIDs = append(journalIDs, journalID)
		}
		if err := s.verifyJournalsNaively(ctx, journalIDs); err != nil {
			return decimal.Zero, err
		}
	}

	// Every contributing journal (if any) is either backed by a trusted
	// cached verdict or just passed a live check -- trust the same
	// trusted, checkpoint-independent recompute RecomputeBalance uses.
	return s.recompute.RecomputeBalance(ctx, holder, currencyUID, classificationUID)
}

// verifyJournalsNaively is the naive, pre-T4 per-journal check, now itself
// batched (design doc §4.5's own flagged optimization): fetchJournalAuthMaterial
// fetches every one of journalIDs' metadata and entries in two round trips
// total (not one round trip per journal, the pre-batching shape this
// function had before), then core.VerifyJournalAuth runs in-process per
// journal against already-fetched data. Only reached for journals whose
// contributing entries have no Authorized cached verdict (the uncovered
// tail, or entries predating migration 054) -- bounded by the attestation
// interval, not by the account's total history.
func (s *VerifiedBalanceStore) verifyJournalsNaively(ctx context.Context, journalIDs []int64) error {
	materials, err := fetchJournalAuthMaterial(ctx, s.q, s.dims, journalIDs)
	if err != nil {
		return fmt.Errorf("postgres: verified balance: %w", err)
	}

	for _, journalID := range journalIDs {
		material, ok := materials[journalID]
		if !ok {
			// Should not happen -- journal_entries.journal_id is FK-enforced
			// against journals.id -- but an unverifiable journal must never
			// be silently trusted (working-agreements §3, fail-closed).
			return fmt.Errorf("postgres: verified balance: journal id %d: no journal row found: %w", journalID, core.ErrUnauthorizedJournal)
		}
		if err := core.VerifyJournalAuth(ctx, s.verifier, material.Input, material.EffectiveAt, material.AuthDigest, material.AuthSignature, material.AuthKeyID); err != nil {
			return fmt.Errorf("postgres: verified balance: journal id %d: %w", journalID, err)
		}
	}
	return nil
}
