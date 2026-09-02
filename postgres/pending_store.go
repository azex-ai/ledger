package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

// Compile-time interface assertions.
var (
	_ core.PendingBalanceWriter  = (*PendingStore)(nil)
	_ core.PendingTimeoutSweeper = (*PendingStore)(nil)
)

// PendingStore implements PendingBalanceWriter and PendingTimeoutSweeper.
//
// It operates on top of LedgerStore.PostJournal (which handles advisory
// locking, idempotency, and rollup-queue enqueueing) using the well-known
// deposit_pending / deposit_confirm_pending / deposit_release_pending template
// journal types. AddPending/ConfirmPending/CancelPending resolve the
// classification codes each call needs via resolveClassificationIDs, never
// caching them on the store; ExpirePendingOlderThan does the same. See
// NewPendingStore's doc comment for why nothing is resolved (or can fail) at
// construction time -- installing the pending bundle
// (presets.InstallPendingBundle) is required before any of these methods
// succeed, but that surfaces as a core.ErrNotFound from the first call that
// needs a classification the bundle would have created, not from
// construction.
//
// Pool-mode vs tx-mode mirror LedgerStore semantics:
//   - pool mode: each public method starts its own transaction.
//   - tx mode  : obtained via WithDB; callers own the transaction lifecycle.
type PendingStore struct {
	pool       *pgxpool.Pool
	db         DBTX
	q          *sqlcgen.Queries
	ledger     *LedgerStore
	classStore *ClassificationStore
}

// NewPendingStore constructs a PendingStore. It performs no I/O and cannot
// fail -- classification IDs are resolved per call (resolveClassificationIDs),
// never cached on the store, so there is nothing to resolve or cache here.
// Per-call resolution is also what keeps the store race-free across the
// goroutines it is shared between (caching at construction time would freeze
// in whatever ids happened to exist yet, and never notice a later install).
func NewPendingStore(pool *pgxpool.Pool, ledger *LedgerStore, classStore *ClassificationStore) *PendingStore {
	return &PendingStore{
		pool:       pool,
		db:         pool,
		q:          sqlcgen.New(pool),
		ledger:     ledger,
		classStore: classStore,
	}
}

// WithDB returns a clone bound to an existing transaction or DBTX.  The caller
// owns tx lifecycle.
func (s *PendingStore) WithDB(db DBTX, ledger *LedgerStore, classStore *ClassificationStore) *PendingStore {
	return &PendingStore{
		pool:       nil,
		db:         db,
		q:          sqlcgen.New(db),
		ledger:     ledger,
		classStore: classStore,
	}
}

// AddPending moves funds into the pending classification (two-phase step 1).
// Entry pattern: DR suspense (system), CR pending (user).
// Idempotent on IdempotencyKey.
func (s *PendingStore) AddPending(ctx context.Context, in core.AddPendingInput) (*core.Journal, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}

	clsIDs, err := s.resolveClassificationIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("pending: add: %w", err)
	}

	systemHolder := core.SystemAccountHolder(in.AccountHolder)

	input := core.JournalInput{
		IdempotencyKey: in.IdempotencyKey,
		ActorID:        in.ActorID,
		Source:         in.Source,
		Metadata:       in.Metadata,
		Entries: []core.EntryInput{
			// DR suspense (system) — funds held by platform
			{
				AccountHolder:     systemHolder,
				CurrencyUID:       in.CurrencyUID,
				ClassificationUID: clsIDs.suspense,
				EntryType:         core.EntryTypeDebit,
				Amount:            in.Amount,
			},
			// CR pending (user) — in-flight deposit credited to user pending
			{
				AccountHolder:     in.AccountHolder,
				CurrencyUID:       in.CurrencyUID,
				ClassificationUID: clsIDs.pending,
				EntryType:         core.EntryTypeCredit,
				Amount:            in.Amount,
			},
		},
	}

	// Resolve the journal type uid for "deposit_pending"
	jt, err := s.q.GetJournalTypeByCode(ctx, "deposit_pending")
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("pending: add: journal type 'deposit_pending' not found — install pending bundle first: %w", core.ErrNotFound)
		}
		return nil, fmt.Errorf("pending: add: resolve journal type: %w", err)
	}
	input.JournalTypeUID = pgToUID(jt.Uid)

	return s.ledger.PostJournal(ctx, input)
}

// ConfirmPending settles a pending deposit (two-phase step 2 — success path).
// Entry pattern (4 lines):
//
//	DR pending   (user)   — clears user's pending balance
//	DR main_wallet (user) — credits user's spendable balance
//	CR suspense  (system) — clears platform suspense
//	CR custodial (system) — records platform custody gain
//
// Idempotent on IdempotencyKey.
// Returns ErrInsufficientBalance if the pending balance is less than Amount.
func (s *PendingStore) ConfirmPending(ctx context.Context, in core.ConfirmPendingInput) (*core.Journal, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}

	clsIDs, err := s.resolveClassificationIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("pending: confirm: %w", err)
	}

	jt, err := s.q.GetJournalTypeByCode(ctx, "deposit_confirm_pending")
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("pending: confirm: journal type 'deposit_confirm_pending' not found: %w", core.ErrNotFound)
		}
		return nil, fmt.Errorf("pending: confirm: resolve journal type: %w", err)
	}

	input := s.buildConfirmPendingJournalInput(in, clsIDs)
	input.JournalTypeUID = pgToUID(jt.Uid)

	// Idempotency check first — avoid acquiring a balance lock if already posted.
	existing, err := s.q.GetJournalByIdempotencyKey(ctx, in.IdempotencyKey)
	if err == nil {
		return s.ledger.ensureJournalMatchesInput(ctx, s.q, existing, input)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("pending: confirm: idempotency check: %w", err)
	}

	return s.checkPendingBalanceAndPost(ctx, "pending: confirm", in.AccountHolder, in.CurrencyUID, clsIDs.pending, in.Amount, input)
}

// CancelPending reverses a pending deposit (two-phase step 2 — cancel path).
// Posts a compensating journal: DR pending (user), CR suspense (system).
// The original AddPending journal is never mutated (append-only principle).
// Returns ErrInsufficientBalance if the pending balance is already zero.
// Idempotent on IdempotencyKey.
func (s *PendingStore) CancelPending(ctx context.Context, in core.CancelPendingInput) (*core.Journal, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}

	clsIDs, err := s.resolveClassificationIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("pending: cancel: %w", err)
	}

	jt, err := s.q.GetJournalTypeByCode(ctx, "deposit_release_pending")
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("pending: cancel: journal type 'deposit_release_pending' not found: %w", core.ErrNotFound)
		}
		return nil, fmt.Errorf("pending: cancel: resolve journal type: %w", err)
	}

	input := s.buildCancelPendingJournalInput(in, clsIDs)
	input.JournalTypeUID = pgToUID(jt.Uid)

	// Idempotency check first — avoid acquiring a balance lock if already posted.
	existing, err := s.q.GetJournalByIdempotencyKey(ctx, in.IdempotencyKey)
	if err == nil {
		return s.ledger.ensureJournalMatchesInput(ctx, s.q, existing, input)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("pending: cancel: idempotency check: %w", err)
	}

	return s.checkPendingBalanceAndPost(ctx, "pending: cancel", in.AccountHolder, in.CurrencyUID, clsIDs.pending, in.Amount, input)
}

// checkPendingBalanceAndPost serializes the (holder, currency_id) balance with
// an advisory lock, reads the pending balance under the lock, rejects if
// insufficient, and posts the journal — all in one transaction so two
// concurrent confirms or cancels cannot both pass the balance check (TOCTOU).
//
// In pool mode this method begins and commits its own transaction. In tx mode
// the caller's transaction is used; the caller owns commit/rollback.
//
// Signing: in pool mode this method owns its own transaction the same way
// LedgerStore.PostJournal's pool-mode branch does, so it follows the same
// rule (financial.md: no external call inside an open transaction) —
// Authorize runs strictly before Begin, and the resulting AuthorizedJournal
// is posted via PostAuthorized once inside the transaction, instead of
// calling PostJournal on a WithDB-bound LedgerStore (which would always
// produce AuthStatusUnsignedTxMode, indistinguishable from "no Attestor
// configured", regardless of pool mode — see PostJournal's own doc comment).
// Before this, ConfirmPending/CancelPending journals were unsigned even when
// this Service was constructed WithAttestor, because this method's own
// transaction made the inner LedgerStore look tx-bound by the time
// PostJournal ran. In tx mode (this PendingStore obtained via WithDB, i.e.
// composed inside ledger.Service.RunInTx) there remains no safe point to
// call the Attestor, so the journal is posted via the plain PostJournal path
// and keeps AuthStatusUnsignedTxMode, exactly like every other write
// composed that way (see ledger.go's RunInTx doc comment).
func (s *PendingStore) checkPendingBalanceAndPost(
	ctx context.Context,
	errPrefix string,
	holder int64,
	currencyUID, pendingClsUID string,
	required decimal.Decimal,
	input core.JournalInput,
) (*core.Journal, error) {
	run := func(qtx *sqlcgen.Queries, ledger *LedgerStore, authorized *core.AuthorizedJournal) (*core.Journal, error) {
		cur, err := ledger.dims.currencyByUIDOrErr(ctx, qtx, currencyUID)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", errPrefix, err)
		}
		// Pre-lock the COMPLETE set of (holder, currency_id) pairs this
		// journal will touch, in the same global order postJournalWithQueries
		// derives (concurrency.md 2026-09-02 Major). Locking only the user's
		// own pair here used to invert the order for the rest of the
		// transaction: PostJournal below takes the system counterpart -holder
		// FIRST (it sorts ascending and SystemAccountHolder(h) = -h is
		// negative), so this path held H and asked for -H while every other
		// write path in the repository holds -H and asks for H. Two entirely
		// ordinary calls -- an AddPending and a ConfirmPending on the same
		// user -- were then enough to close an ABBA cycle on the deposit
		// money-path (SQLSTATE 40P01, retryable but a real failure + latency
		// spike, no malicious input needed).
		//
		// Advisory xact locks are re-entrant, so PostJournal re-taking these
		// same locks a few lines down is a no-op. The user's pair stays in the
		// set, so the TOCTOU protection the pending-balance gate below relies
		// on is unchanged -- asserted rather than assumed, right after.
		resolved, err := ledger.resolveEntries(ctx, qtx, input.Entries)
		if err != nil {
			return nil, fmt.Errorf("%s: resolve entries for lock order: %w", errPrefix, err)
		}
		pairs := balancePairsFromEntries(resolved)
		gatePair := balancePair{holder: holder, currencyID: cur.ID}
		if !slices.Contains(pairs, gatePair) {
			return nil, fmt.Errorf(
				"%s: internal: journal entries do not touch (holder %d, currency %s), so the pending-balance check below would read an unlocked balance: %w",
				errPrefix, holder, currencyUID, core.ErrInvalidInput,
			)
		}
		// Idempotency BEFORE balance, the same order postJournalWithQueries
		// and Reserve use. Taking it here rather than leaving it to
		// PostJournal below (where it would land after these balance locks)
		// is the second half of the same lock-order rule: a concurrent
		// single-journal retry of this key would otherwise hold idem:K while
		// waiting for a balance lock held here. Re-entrant, so PostJournal
		// re-taking it costs nothing.
		if err := acquireIdempotencyLock(ctx, qtx, input.IdempotencyKey); err != nil {
			return nil, fmt.Errorf("%s: %w", errPrefix, err)
		}
		if err := acquireBalanceLocks(ctx, qtx, pairs); err != nil {
			return nil, fmt.Errorf("%s: %w", errPrefix, err)
		}
		// Idempotency recheck UNDER the balance lock (I-3): the caller's
		// pre-check ran before the lock, so a retry racing its original
		// request can pass the pre-check, then arrive here after the original
		// committed and consumed the pending balance. Without this recheck it
		// would hit the balance gate below and get ErrInsufficientBalance
		// instead of the idempotent original journal — a contract violation
		// that misleads callers into treating a completed confirm as failed.
		if existing, err := qtx.GetJournalByIdempotencyKey(ctx, input.IdempotencyKey); err == nil {
			return ledger.ensureJournalMatchesInput(ctx, qtx, existing, input)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%s: idempotency recheck: %w", errPrefix, err)
		}
		bal, err := ledger.GetBalance(ctx, holder, currencyUID, pendingClsUID)
		if err != nil {
			return nil, fmt.Errorf("%s: get pending balance: %w", errPrefix, err)
		}
		if bal.LessThan(required) {
			return nil, fmt.Errorf(
				"%s: insufficient pending balance: available=%s required=%s: %w",
				errPrefix, bal, required, core.ErrInsufficientBalance,
			)
		}
		if authorized != nil {
			return ledger.PostAuthorized(ctx, *authorized)
		}
		return ledger.PostJournal(ctx, input)
	}

	if s.pool == nil {
		// Tx mode: caller owns tx; queries and ledger are already bound to
		// it, and the Attestor cannot be called from inside an
		// already-open transaction. No authorized journal to hand in.
		return run(s.q, s.ledger, nil)
	}

	// Pool mode: sign strictly before opening the transaction below (same
	// placement rule PostJournal's own pool-mode branch follows). Authorize
	// itself re-checks idempotency before signing (skips signing -- and any
	// Attestor call -- if this key was already posted), so a retried
	// ConfirmPending/CancelPending does not needlessly re-sign.
	authorized, err := s.ledger.Authorize(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("%s: authorize: %w", errPrefix, err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: begin: %w", errPrefix, err)
	}
	defer tx.Rollback(ctx)

	j, err := run(s.q.WithTx(tx), s.ledger.WithDB(tx), &authorized)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("%s: commit: %w", errPrefix, err)
	}
	return j, nil
}

func (s *PendingStore) buildConfirmPendingJournalInput(in core.ConfirmPendingInput, clsIDs pendingClassIDs) core.JournalInput {
	systemHolder := core.SystemAccountHolder(in.AccountHolder)
	return core.JournalInput{
		IdempotencyKey: in.IdempotencyKey,
		ActorID:        in.ActorID,
		Source:         in.Source,
		Metadata:       in.Metadata,
		Entries: []core.EntryInput{
			{
				AccountHolder:     in.AccountHolder,
				CurrencyUID:       in.CurrencyUID,
				ClassificationUID: clsIDs.pending,
				EntryType:         core.EntryTypeDebit,
				Amount:            in.Amount,
			},
			{
				AccountHolder:     in.AccountHolder,
				CurrencyUID:       in.CurrencyUID,
				ClassificationUID: clsIDs.mainWallet,
				EntryType:         core.EntryTypeDebit,
				Amount:            in.Amount,
			},
			{
				AccountHolder:     systemHolder,
				CurrencyUID:       in.CurrencyUID,
				ClassificationUID: clsIDs.suspense,
				EntryType:         core.EntryTypeCredit,
				Amount:            in.Amount,
			},
			{
				AccountHolder:     systemHolder,
				CurrencyUID:       in.CurrencyUID,
				ClassificationUID: clsIDs.custodial,
				EntryType:         core.EntryTypeCredit,
				Amount:            in.Amount,
			},
		},
	}
}

func (s *PendingStore) buildCancelPendingJournalInput(in core.CancelPendingInput, clsIDs pendingClassIDs) core.JournalInput {
	reason := in.Reason
	if reason == "" {
		reason = "cancelled"
	}

	meta := make(map[string]string, len(in.Metadata)+1)
	for k, v := range in.Metadata {
		meta[k] = v
	}
	meta["reason"] = reason

	systemHolder := core.SystemAccountHolder(in.AccountHolder)
	return core.JournalInput{
		IdempotencyKey: in.IdempotencyKey,
		ActorID:        in.ActorID,
		Source:         in.Source,
		Metadata:       meta,
		Entries: []core.EntryInput{
			{
				AccountHolder:     in.AccountHolder,
				CurrencyUID:       in.CurrencyUID,
				ClassificationUID: clsIDs.pending,
				EntryType:         core.EntryTypeDebit,
				Amount:            in.Amount,
			},
			{
				AccountHolder:     systemHolder,
				CurrencyUID:       in.CurrencyUID,
				ClassificationUID: clsIDs.suspense,
				EntryType:         core.EntryTypeCredit,
				Amount:            in.Amount,
			},
		},
	}
}

// ExpirePendingOlderThan finds all user accounts with a pending balance
// originating from journals created more than [threshold] ago and cancels
// them by posting compensating journals.
//
// At most 1 000 accounts are processed per call.  The caller (cron / worker)
// is responsible for calling this repeatedly until 0 is returned.
//
// Returns the count of accounts expired, and any partial error (errors from
// individual cancellations are aggregated, not fatal — the sweeper is
// idempotent on retry).
func (s *PendingStore) ExpirePendingOlderThan(ctx context.Context, threshold time.Duration) (int, error) {
	clsIDs, err := s.resolveClassificationIDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("pending: expire: %w", err)
	}

	cutoff := time.Now().UTC().Add(-threshold)

	pendingCls, err := s.ledger.dims.classByUIDOrErr(ctx, s.q, clsIDs.pending)
	if err != nil {
		return 0, fmt.Errorf("pending: expire: %w", err)
	}
	rows, err := s.q.ListPendingJournalsOlderThan(ctx, sqlcgen.ListPendingJournalsOlderThanParams{
		PendingClassificationID: pendingCls.ID,
		Cutoff:                  cutoff,
	})
	if err != nil {
		return 0, fmt.Errorf("pending: expire: list stale journals: %w", err)
	}

	var cancelled int
	var errs []error

	for _, row := range rows {
		amount := mustNumericToDecimal(row.Amount)

		// Check actual pending balance — skip if already drained (confirmed/cancelled).
		rowCur, err := s.ledger.dims.currencyByIDOrErr(ctx, s.q, row.CurrencyID)
		if err != nil {
			errs = append(errs, fmt.Errorf("holder=%d resolve currency: %w", row.AccountHolder, err))
			continue
		}
		bal, err := s.ledger.GetBalance(ctx, row.AccountHolder, rowCur.UID, clsIDs.pending)
		if err != nil {
			errs = append(errs, fmt.Errorf("holder=%d currency=%s get balance: %w", row.AccountHolder, rowCur.UID, err))
			continue
		}
		if !bal.IsPositive() {
			continue // already settled
		}
		// Cap to actual balance (partial confirmations may have happened).
		if bal.LessThan(amount) {
			amount = bal
		}

		_, err = s.CancelPending(ctx, core.CancelPendingInput{
			AccountHolder:  row.AccountHolder,
			CurrencyUID:    rowCur.UID,
			Amount:         amount,
			Reason:         "expired",
			IdempotencyKey: fmt.Sprintf("pending:expire:journal=%d", row.JournalID),
			Source:         "pending_timeout_sweeper",
		})
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"pending: expire: cancel journal_id=%d holder=%d currency=%s: %w",
				row.JournalID, row.AccountHolder, rowCur.UID, err,
			))
			continue
		}
		cancelled++
	}

	if len(errs) > 0 {
		// errors.Join, not %v: this is PendingTimeoutSweeper's ONLY return
		// surface, and a consumer's worker decides retry-vs-alert on it with
		// errors.Is / core.IsRetryable. %v flattens every wrapped sentinel to
		// text, so a transient DB failure and a frozen account came back
		// indistinguishable (consumer-surface.md 2026-09-02 m-13). Join keeps
		// errors.Is reaching through to every sub-error at once.
		return cancelled, fmt.Errorf("pending: expire: %d errors: %w", len(errs), errors.Join(errs...))
	}
	return cancelled, nil
}

// pendingClassIDs holds the resolved classification IDs needed for entry construction.
type pendingClassIDs struct {
	pending    string
	suspense   string
	mainWallet string
	custodial  string
}

// resolveClassificationIDs loads all four required classification IDs.
// Results are cached on the store after first resolution.
func (s *PendingStore) resolveClassificationIDs(ctx context.Context) (pendingClassIDs, error) {
	resolve := func(code string) (string, error) {
		cls, err := s.classStore.GetByCode(ctx, code)
		if err != nil {
			if errors.Is(err, core.ErrNotFound) {
				return "", fmt.Errorf("classification %q not found — install pending bundle first: %w", code, core.ErrNotFound)
			}
			return "", fmt.Errorf("get classification %q: %w", code, err)
		}
		return cls.UID, nil
	}

	pendingID, err := resolve("pending")
	if err != nil {
		return pendingClassIDs{}, err
	}
	suspenseID, err := resolve("suspense")
	if err != nil {
		return pendingClassIDs{}, err
	}
	mainWalletID, err := resolve("main_wallet")
	if err != nil {
		return pendingClassIDs{}, err
	}
	custodialID, err := resolve("custodial")
	if err != nil {
		return pendingClassIDs{}, err
	}

	return pendingClassIDs{
		pending:    pendingID,
		suspense:   suspenseID,
		mainWallet: mainWalletID,
		custodial:  custodialID,
	}, nil
}
