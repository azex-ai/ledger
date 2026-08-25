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
// A cached attestation-time verdict (entry_attestations.auth_verdict, T4's
// migration 054) is honoured in one direction only:
//   - core.JournalAuthVerdictUnauthorized -> the whole balance is UNDEFINED
//     immediately (I-32's fail-closed rule). That answer can only make the
//     result stricter, so trusting it is safe.
//   - anything else, Authorized included -> a live core.VerifyJournalAuth.
//
// T4 originally skipped the live check on a cached Authorized, and that was
// the flaw. The verdict answers "was this journal authorized when it was
// attested"; it does not answer "are its entries still what was authorized".
// core.CanonicalJournalDigest covers every entry's Amount, so a live check
// recomputes the digest from current content and fails once an amount has been
// edited -- and the fast path skipped precisely that check. The batch's signed
// content protects the verdict from alteration; it does not protect the
// entries the verdict was about.
//
// entry_attestations.leaf_hash does protect those, and comparing it is what
// the asynchronous VerifyLedger sweep does. This is the synchronous gate a
// withdrawal passes through, so it cannot defer to a sweep.
//
// The cost is therefore the pre-T4 cost: a `journals JOIN journal_entries`
// round trip plus one core.VerifyJournalAuth per contributing journal,
// batched. That saving is given up on purpose. Worth recording: this gate was
// the only caller of VerifiedBalance, so T4's optimization only ever served
// the single path that cannot afford it.
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

	// A cached verdict is honoured in one direction only.
	//
	// Unauthorized short-circuits the whole dimension, because that answer can
	// only ever make the result stricter -- a journal that failed verification
	// at attestation time cannot become authorized later.
	//
	// Authorized does NOT skip the live check, which is what T4 originally did
	// and where it was wrong. The cached verdict answers "was this journal
	// authorized when it was attested". It does not answer "are its entries
	// still what was authorized". core.CanonicalJournalDigest covers every
	// entry's Amount, so a live core.VerifyJournalAuth recomputes the digest
	// from current content and fails when an amount has been edited since --
	// and skipping it on the strength of the cached verdict skipped the only
	// check in this path that notices. The batch's signed content protects the
	// verdict from being altered; it does not protect the entries the verdict
	// was about. entry_attestations.leaf_hash does, and that is what the
	// asynchronous VerifyLedger sweep compares -- but this is the synchronous
	// gate a withdrawal passes through, and it cannot wait for a sweep.
	//
	// The cost is the pre-T4 cost: one journals-to-journal_entries round trip
	// and one signature verification per contributing journal, batched. T4's
	// saving is given up here deliberately. It is worth noting that this gate
	// was the only caller of VerifiedBalance, so the optimization only ever
	// served the one path that cannot afford it.
	needsLiveCheck := make(map[int64]struct{})
	for _, r := range rows {
		switch core.JournalAuthVerdict(r.Verdict) {
		case core.JournalAuthVerdictUnauthorized:
			return decimal.Zero, fmt.Errorf("postgres: verified balance: journal id %d: cached attestation verdict is unauthorized: %w", r.JournalID, core.ErrUnauthorizedJournal)
		default:
			// Authorized and Unknown alike: verify live.
			needsLiveCheck[r.JournalID] = struct{}{}
		}
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

	// Every contributing journal (if any) just passed a live check against its
	// current content -- trust the same checkpoint-independent recompute
	// RecomputeBalance uses.
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
