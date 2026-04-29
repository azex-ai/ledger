package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

// Compile-time interface assertions.
var (
	_ core.PendingBalanceWriter = (*PendingStore)(nil)
	_ core.PendingTimeoutSweeper = (*PendingStore)(nil)
)

// PendingStore implements PendingBalanceWriter and PendingTimeoutSweeper.
//
// It operates on top of LedgerStore.PostJournal (which handles advisory
// locking, idempotency, and rollup-queue enqueueing) using the well-known
// deposit_pending / deposit_confirm_pending / deposit_release_pending template
// journal types.  The journal-type IDs are resolved once at construction time
// and cached; if the pending bundle has not been installed (InstallPendingBundle)
// the constructor will return an error.
//
// Pool-mode vs tx-mode mirror LedgerStore semantics:
//   - pool mode: each public method starts its own transaction.
//   - tx mode  : obtained via WithDB; callers own the transaction lifecycle.
type PendingStore struct {
	pool        *pgxpool.Pool
	db          DBTX
	q           *sqlcgen.Queries
	ledger      *LedgerStore
	classStore  *ClassificationStore

	// resolved IDs — populated by NewPendingStore or lazily on first use
	pendingClassID  int64
	suspenseClassID int64
}

// NewPendingStore constructs a PendingStore.  It resolves the classification
// IDs needed by ExpirePendingOlderThan at construction time — if the pending
// bundle hasn't been installed yet you should call InstallPendingBundle first.
//
// If classification IDs cannot be resolved (bundle not installed) the store
// still works for AddPending / ConfirmPending / CancelPending because those go
// via LedgerStore template execution; only ExpirePendingOlderThan requires the
// IDs.  Resolution is attempted lazily in that method as a fallback.
func NewPendingStore(pool *pgxpool.Pool, ledger *LedgerStore, classStore *ClassificationStore) *PendingStore {
	return &PendingStore{
		pool:       pool,
		db:         pool,
		q:          sqlcgen.New(pool),
		ledger:     ledger,
		classStore: classStore,
	}
}

// WithDB returns a clone bound to an existing transaction or DBTX.  The clone
// shares resolved IDs with the original; the caller owns tx lifecycle.
func (s *PendingStore) WithDB(db DBTX, ledger *LedgerStore, classStore *ClassificationStore) *PendingStore {
	return &PendingStore{
		pool:            nil,
		db:              db,
		q:               sqlcgen.New(db),
		ledger:          ledger,
		classStore:      classStore,
		pendingClassID:  s.pendingClassID,
		suspenseClassID: s.suspenseClassID,
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
				AccountHolder:    systemHolder,
				CurrencyID:       in.CurrencyID,
				ClassificationID: clsIDs.suspense,
				EntryType:        core.EntryTypeDebit,
				Amount:           in.Amount,
			},
			// CR pending (user) — in-flight deposit credited to user pending
			{
				AccountHolder:    in.AccountHolder,
				CurrencyID:       in.CurrencyID,
				ClassificationID: clsIDs.pending,
				EntryType:        core.EntryTypeCredit,
				Amount:           in.Amount,
			},
		},
	}

	// Resolve journal_type_id for "deposit_pending"
	jtID, err := s.q.GetJournalTypeIDByCode(ctx, "deposit_pending")
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("pending: add: journal type 'deposit_pending' not found — install pending bundle first: %w", core.ErrNotFound)
		}
		return nil, fmt.Errorf("pending: add: resolve journal type: %w", err)
	}
	input.JournalTypeID = jtID

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

	// Idempotency check first — avoid the balance query if already posted.
	existing, err := s.q.GetJournalByIdempotencyKey(ctx, in.IdempotencyKey)
	if err == nil {
		return journalFromRow(existing), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("pending: confirm: idempotency check: %w", err)
	}

	// Balance check: user must have enough pending.
	pendingBal, err := s.ledger.GetBalance(ctx, in.AccountHolder, in.CurrencyID, clsIDs.pending)
	if err != nil {
		return nil, fmt.Errorf("pending: confirm: get pending balance: %w", err)
	}
	if pendingBal.LessThan(in.Amount) {
		return nil, fmt.Errorf(
			"pending: confirm: insufficient pending balance: available=%s required=%s: %w",
			pendingBal, in.Amount, core.ErrInsufficientBalance,
		)
	}

	systemHolder := core.SystemAccountHolder(in.AccountHolder)

	input := core.JournalInput{
		IdempotencyKey: in.IdempotencyKey,
		ActorID:        in.ActorID,
		Source:         in.Source,
		Metadata:       in.Metadata,
		Entries: []core.EntryInput{
			// DR pending (user) — clears pending
			{
				AccountHolder:    in.AccountHolder,
				CurrencyID:       in.CurrencyID,
				ClassificationID: clsIDs.pending,
				EntryType:        core.EntryTypeDebit,
				Amount:           in.Amount,
			},
			// DR main_wallet (user) — credits spendable wallet
			{
				AccountHolder:    in.AccountHolder,
				CurrencyID:       in.CurrencyID,
				ClassificationID: clsIDs.mainWallet,
				EntryType:        core.EntryTypeDebit,
				Amount:           in.Amount,
			},
			// CR suspense (system) — clears suspense
			{
				AccountHolder:    systemHolder,
				CurrencyID:       in.CurrencyID,
				ClassificationID: clsIDs.suspense,
				EntryType:        core.EntryTypeCredit,
				Amount:           in.Amount,
			},
			// CR custodial (system) — platform gains custody
			{
				AccountHolder:    systemHolder,
				CurrencyID:       in.CurrencyID,
				ClassificationID: clsIDs.custodial,
				EntryType:        core.EntryTypeCredit,
				Amount:           in.Amount,
			},
		},
	}

	jtID, err := s.q.GetJournalTypeIDByCode(ctx, "deposit_confirm_pending")
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("pending: confirm: journal type 'deposit_confirm_pending' not found: %w", core.ErrNotFound)
		}
		return nil, fmt.Errorf("pending: confirm: resolve journal type: %w", err)
	}
	input.JournalTypeID = jtID

	return s.ledger.PostJournal(ctx, input)
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

	// Idempotency check.
	existing, err := s.q.GetJournalByIdempotencyKey(ctx, in.IdempotencyKey)
	if err == nil {
		return journalFromRow(existing), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("pending: cancel: idempotency check: %w", err)
	}

	// Balance check.
	pendingBal, err := s.ledger.GetBalance(ctx, in.AccountHolder, in.CurrencyID, clsIDs.pending)
	if err != nil {
		return nil, fmt.Errorf("pending: cancel: get pending balance: %w", err)
	}
	if pendingBal.LessThan(in.Amount) {
		return nil, fmt.Errorf(
			"pending: cancel: insufficient pending balance: available=%s required=%s: %w",
			pendingBal, in.Amount, core.ErrInsufficientBalance,
		)
	}

	reason := in.Reason
	if reason == "" {
		reason = "cancelled"
	}

	systemHolder := core.SystemAccountHolder(in.AccountHolder)

	meta := in.Metadata
	if meta == nil {
		meta = make(map[string]string)
	}
	meta["reason"] = reason

	input := core.JournalInput{
		IdempotencyKey: in.IdempotencyKey,
		ActorID:        in.ActorID,
		Source:         in.Source,
		Metadata:       meta,
		Entries: []core.EntryInput{
			// DR pending (user) — clears pending balance
			{
				AccountHolder:    in.AccountHolder,
				CurrencyID:       in.CurrencyID,
				ClassificationID: clsIDs.pending,
				EntryType:        core.EntryTypeDebit,
				Amount:           in.Amount,
			},
			// CR suspense (system) — clears suspense
			{
				AccountHolder:    systemHolder,
				CurrencyID:       in.CurrencyID,
				ClassificationID: clsIDs.suspense,
				EntryType:        core.EntryTypeCredit,
				Amount:           in.Amount,
			},
		},
	}

	jtID, err := s.q.GetJournalTypeIDByCode(ctx, "deposit_release_pending")
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("pending: cancel: journal type 'deposit_release_pending' not found: %w", core.ErrNotFound)
		}
		return nil, fmt.Errorf("pending: cancel: resolve journal type: %w", err)
	}
	input.JournalTypeID = jtID

	return s.ledger.PostJournal(ctx, input)
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

	rows, err := s.q.ListPendingJournalsOlderThan(ctx, sqlcgen.ListPendingJournalsOlderThanParams{
		PendingClassificationID: clsIDs.pending,
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
		bal, err := s.ledger.GetBalance(ctx, row.AccountHolder, row.CurrencyID, clsIDs.pending)
		if err != nil {
			errs = append(errs, fmt.Errorf("holder=%d currency=%d get balance: %w", row.AccountHolder, row.CurrencyID, err))
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
			CurrencyID:     row.CurrencyID,
			Amount:         amount,
			Reason:         "expired",
			IdempotencyKey: fmt.Sprintf("pending:expire:journal=%d", row.JournalID),
			Source:         "pending_timeout_sweeper",
		})
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"pending: expire: cancel journal_id=%d holder=%d: %w",
				row.JournalID, row.AccountHolder, err,
			))
			continue
		}
		cancelled++
	}

	if len(errs) > 0 {
		return cancelled, fmt.Errorf("pending: expire: %d errors: %v", len(errs), errs)
	}
	return cancelled, nil
}

// pendingClassIDs holds the resolved classification IDs needed for entry construction.
type pendingClassIDs struct {
	pending    int64
	suspense   int64
	mainWallet int64
	custodial  int64
}

// resolveClassificationIDs loads all four required classification IDs.
// Results are cached on the store after first resolution.
func (s *PendingStore) resolveClassificationIDs(ctx context.Context) (pendingClassIDs, error) {
	if s.pendingClassID != 0 && s.suspenseClassID != 0 {
		// fast path: already resolved main + pending; resolve all four inline
	}

	resolve := func(code string) (int64, error) {
		cls, err := s.classStore.GetByCode(ctx, code)
		if err != nil {
			if errors.Is(err, core.ErrNotFound) {
				return 0, fmt.Errorf("classification %q not found — install pending bundle first: %w", code, core.ErrNotFound)
			}
			return 0, fmt.Errorf("get classification %q: %w", code, err)
		}
		return cls.ID, nil
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

	// Cache the two used by ExpirePendingOlderThan for next call.
	s.pendingClassID = pendingID
	s.suspenseClassID = suspenseID

	return pendingClassIDs{
		pending:    pendingID,
		suspense:   suspenseID,
		mainWallet: mainWalletID,
		custodial:  custodialID,
	}, nil
}
