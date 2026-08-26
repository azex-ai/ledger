package core

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// JournalWriter handles journal posting.
type JournalWriter interface {
	PostJournal(ctx context.Context, input JournalInput) (*Journal, error)
	// PostAuthorized posts a journal from an AuthorizedJournal obtained via
	// Authorize (design doc §7.5): the caller already ran Authorize -- and
	// any Attestor call it needed -- strictly before opening a transaction,
	// so PostAuthorized itself never calls the Attestor and is safe to call
	// from inside a RunInTx callback (tx mode), closing the gap where
	// PostJournal's tx-mode branch always posted unsigned. See
	// AuthorizedJournal's doc comment.
	PostAuthorized(ctx context.Context, authorized AuthorizedJournal) (*Journal, error)
	ExecuteTemplate(ctx context.Context, templateCode string, params TemplateParams) (*Journal, error)
	// ReverseJournal reverses a journal in full. It rejects (ErrConflict) if
	// journalID already has any reversal recorded against it — full or
	// partial — since a full reversal after a partial one would double-count
	// the portion already reversed. Use ReverseJournalFraction for additional
	// partial reversals once the journal has any reversal history.
	ReverseJournal(ctx context.Context, journalUID string, reason string) (*Journal, error)
	// ReverseJournalFraction reverses num/den of journalID's entries (0 < num
	// <= den, see ValidateReversalFraction). Each entry's share is computed by
	// scaling its currency-and-side group's total by num/den and splitting it
	// back across the group's entries via Allocate, so the resulting reversal
	// journal is itself per-currency balanced and never reversed-amount
	// exceeds any original entry's amount. Multiple partial reversals of the
	// same journal are allowed; their cumulative amount per entry is enforced
	// (ErrConflict on overshoot) via a row lock on the original journal, so
	// concurrent partial reversals of the same journal serialize safely.
	// idempotencyKey follows the library's standard idempotency contract —
	// the same key replayed returns the original reversal; a reused key with
	// a different (journalID, num, den, reason) is a conflict.
	//
	// num == den (e.g. 1/1) is the "reverse everything remaining" form: each
	// entry is reversed by exactly its original amount minus what prior
	// reversals already covered. Use it to complete a reversal whose earlier
	// fractional steps rounded up (fractions always scale the ORIGINAL
	// amount, so e.g. two 1/3 steps of 100.01 cover 33.34+33.34 and the exact
	// remainder 33.33 is not expressible as a fraction of the original).
	ReverseJournalFraction(ctx context.Context, journalUID string, num, den int64, reason string, idempotencyKey string) (*Journal, error)

	// AuthorizeReversal pre-authorizes the reversal ReverseJournal (num=1,
	// den=1) or ReverseJournalFraction (any valid num/den) would post,
	// entirely outside any database transaction, mirroring
	// Authorize/AuthorizeTemplate's split for a plain JournalInput (design
	// doc §7.5) -- extended to cover reversals by board #15 (W2-T1),
	// because a reversal's entries are DERIVED from the original journal's
	// (read from the DB) rather than caller-supplied, so they cannot be
	// signed until that read happens.
	//
	// idempotencyKey must never be empty: callers reproducing
	// ReverseJournal's auto-derived key must pass
	// fmt.Sprintf("reversal:%s:%s", journalUID, reason) explicitly (see
	// ReverseJournal's doc comment for why that exact format matters); this
	// method never invents one itself.
	//
	// Like Authorize, this must run on a pool-mode store (not one obtained
	// via WithDB / a RunInTx callback) and returns core.ErrInvalidInput
	// otherwise.
	//
	// Callers must not treat the returned AuthorizedJournal as a stable
	// commitment: because its Input.Entries are derived from mutable state
	// (the original journal's prior reversal history, for the num==den
	// "reverse everything remaining" form), a concurrent partial reversal
	// landing between this call and the eventual post can invalidate it.
	// That is not a signing bug -- it is why ReverseJournal /
	// ReverseJournalFraction, not PostAuthorized, are the only supported
	// consumers of this method's result: they re-derive the same entries
	// fresh under the original journal's row lock and compare digests,
	// failing the post outright (never silently re-signing or falling back
	// to unsigned) if the two disagree. Posting this result via the
	// generic PostAuthorized instead would skip that row-locked
	// re-verification and the overshoot/already-fully-reversed checks
	// ReverseJournal/ReverseJournalFraction perform -- do not do that.
	AuthorizeReversal(ctx context.Context, journalUID string, num, den int64, reason string, idempotencyKey string) (AuthorizedJournal, error)
}

// TemplateBatchExecutor executes multiple templates as a single atomic unit:
// implementations MUST post all requested journals or none at all (e.g. one
// DB transaction covering the whole batch) — partial application on error is
// not a conforming implementation. The postgres adapter satisfies this via a
// single transaction (or the caller's transaction, in tx mode).
type TemplateBatchExecutor interface {
	ExecuteTemplateBatch(ctx context.Context, requests []TemplateExecutionRequest) ([]*Journal, error)
}

// BalanceReader handles balance queries.
type BalanceReader interface {
	GetBalance(ctx context.Context, holder int64, currencyUID, classificationUID string) (decimal.Decimal, error)
	GetBalances(ctx context.Context, holder int64, currencyUID string) ([]Balance, error)
	BatchGetBalances(ctx context.Context, holderIDs []int64, currencyUID string) (map[int64][]Balance, error)
	// GetBalanceBreakdown aggregates the holder's classification balances by
	// BalanceRole and layers reservation holds on top (see BalanceBreakdown).
	// The whole read is snapshot-consistent: role sums and the holds figure
	// describe the same point in time.
	GetBalanceBreakdown(ctx context.Context, holder int64, currencyUID string) (*BalanceBreakdown, error)
}

// CheckpointIntegrityStore provides trusted, entries-only balance operations
// that never consult balance_checkpoints, so checkpoint tampering has zero
// influence on either method (docs/INVARIANTS.md I-23).
type CheckpointIntegrityStore interface {
	// RecomputeBalance ignores the checkpoint entirely and sums every
	// journal_entries row for the dimension from entry 0. It is slow relative
	// to BalanceReader.GetBalance (checkpoint + delta) because it rescans full
	// history — callers on the withdrawal / large-amount path MUST use this
	// instead of GetBalance for exactly that reason: checkpoint tampering
	// cannot affect a value that never reads the checkpoint.
	RecomputeBalance(ctx context.Context, holder int64, currencyUID, classificationUID string) (decimal.Decimal, error)
	// RebuildCheckpoint is the trusted operator entry point that repairs a
	// checkpoint already found to have drifted by reconcile's
	// checkpoint_balance check. It locks the dimension, recomputes balance and
	// watermark from entry 0, and unconditionally overwrites the existing
	// checkpoint row — unlike the rollup worker's monotonic upsert, which can
	// never repair a checkpoint whose last_entry_id was tampered to look
	// "ahead" of the true watermark.
	//
	// Detection (reconcile) and correction (this method) are deliberately
	// separate calls: nothing in this library invokes RebuildCheckpoint
	// automatically, because auto-correcting while an attack may still be in
	// progress would destroy the forensic evidence the drift represents.
	//
	// A manual repair has the same evidence-destroying property automatic
	// repair does — the moment the checkpoint is overwritten, the drift is
	// gone from balance_checkpoints. So every call durably records the
	// before/after values and the resulting drift in the append-only
	// checkpoint_rebuilds table (migration 050), in the same transaction as
	// the overwrite: a repair can never happen without leaving forensic
	// evidence. actorID identifies who/what triggered the rebuild (0 if
	// unknown), stored alongside the record — same convention as
	// JournalInput.ActorID.
	//
	// Returns core.ErrRollupPending if a rollup_queue item is still pending or
	// claimed for the dimension — see that error's doc comment.
	//
	// Returns the uid-based BalanceCheckpoint: this method is the one place
	// a checkpoint crosses into the public library API, so the result
	// speaks uids exclusively (I-18) rather than the internal ids
	// service.BalanceCheckpoint (and the input currencyUID/classificationUID
	// were resolved from) carries.
	RebuildCheckpoint(ctx context.Context, holder int64, currencyUID, classificationUID string, actorID int64) (*BalanceCheckpoint, error)
}

// Reserver handles reserve/settle/lock flow.
type Reserver interface {
	Reserve(ctx context.Context, input ReserveInput) (*Reservation, error)
	// Settle marks an active reservation as settled with the actual amount
	// consumed. input.Amount must be positive and must not exceed the
	// reservation's reserved amount; over-settlement is rejected with
	// ErrInvalidInput, never silently clamped. The unused remainder (reserved
	// minus actual) is implicitly released by the settle transition.
	// input.IdempotencyKey is REQUIRED (I-3): see SettleInput's doc comment
	// for why the reservation's own state machine cannot serve as a replay
	// signal for a terminal transition.
	Settle(ctx context.Context, input SettleInput) error
	// Release cancels an active reservation, freeing its entire reserved
	// amount without any accounting effect. It is a no-op on the ledger
	// balance beyond removing the hold — no partial release is supported.
	// input.IdempotencyKey is REQUIRED (I-3); see ReleaseInput's doc comment.
	Release(ctx context.Context, input ReleaseInput) error
	// SettlePartial settles part of a reservation. input.Amount must be
	// positive. The first call transitions the reservation from active to
	// settling; subsequent calls accumulate settled_amount further, which
	// must never exceed reserved_amount (ErrInvalidInput on overshoot).
	// A settling reservation's unsettled remainder (reserved minus settled)
	// STAYS held against the balance until FinalizeSettlement; releasing it
	// early would let a concurrent Reserve over-commit (see I-11 and
	// TestReserverStore_SettlePartial_RemainderStillHeld). Calling Settle
	// (the one-shot method) on a settling reservation is rejected; use
	// FinalizeSettlement instead.
	SettlePartial(ctx context.Context, input SettlePartialInput) error
	// FinalizeSettlement completes a reservation that has been partially
	// settled via SettlePartial, transitioning it from settling to settled.
	// It is rejected (ErrInvalidTransition) on any other status — in
	// particular, calling it on an active reservation that never received a
	// SettlePartial call is not a valid "settle everything" shortcut; use
	// Settle for that. input.IdempotencyKey is REQUIRED (I-3); see
	// FinalizeSettlementInput's doc comment.
	FinalizeSettlement(ctx context.Context, input FinalizeSettlementInput) error
	// HeldAmount returns the holder's outstanding holds in the given currency:
	// full reserved_amount for active reservations plus the unsettled
	// remainder of settling ones — the exact figure Reserve subtracts
	// from balance to compute available. Consumers should call this instead of
	// querying the reservations table directly, so available = balance − held
	// can be derived without depending on the ledger's internal schema.
	HeldAmount(ctx context.Context, holder int64, currencyUID string) (decimal.Decimal, error)
}

// Booker handles classification-driven booking lifecycle.
type Booker interface {
	CreateBooking(ctx context.Context, input CreateBookingInput) (*Booking, error)
	Transition(ctx context.Context, input TransitionInput) (*Event, error)
}

// BookingReader handles booking queries.
type BookingReader interface {
	GetBooking(ctx context.Context, uid string) (*Booking, error)
	// ListBookings returns one page plus the opaque cursor for the next
	// page ("" when exhausted).
	ListBookings(ctx context.Context, filter BookingFilter) ([]Booking, string, error)
}

// EventReader handles event queries.
type EventReader interface {
	GetEvent(ctx context.Context, uid string) (*Event, error)
	// ListEvents returns one page plus the opaque cursor for the next page
	// ("" when exhausted).
	ListEvents(ctx context.Context, filter EventFilter) ([]Event, string, error)
}

// EventDeliverer delivers events to external consumers (webhooks, queues, etc.).
type EventDeliverer interface {
	Deliver(ctx context.Context, event Event) error
}

// RollupWorker processes async checkpoint updates.
type RollupWorker interface {
	ProcessBatch(ctx context.Context, batchSize int) (int, error)
}

// Reconciler checks accounting equation integrity.
type Reconciler interface {
	CheckAccountingEquation(ctx context.Context) (*ReconcileResult, error)
	ReconcileAccount(ctx context.Context, holder int64, currencyUID string) (*ReconcileResult, error)
}

// ReconcileResult holds the outcome of a reconciliation check.
type ReconcileResult struct {
	Balanced  bool
	Gap       decimal.Decimal
	Details   []ReconcileDetail
	CheckedAt time.Time
}

type ReconcileDetail struct {
	AccountHolder     int64
	CurrencyUID       string
	ClassificationUID string
	Expected          decimal.Decimal
	Actual            decimal.Decimal
	Drift             decimal.Decimal
}

// Snapshotter handles daily balance snapshots.
type Snapshotter interface {
	CreateDailySnapshot(ctx context.Context, date time.Time) error
	GetSnapshotBalance(ctx context.Context, holder int64, currencyUID string, date time.Time) ([]Balance, error)
}

// ClassificationStore manages dynamic classifications.
type ClassificationStore interface {
	CreateClassification(ctx context.Context, input ClassificationInput) (*Classification, error)
	GetByCode(ctx context.Context, code string) (*Classification, error)
	DeactivateClassification(ctx context.Context, uid string) error
	ListClassifications(ctx context.Context, activeOnly bool) ([]Classification, error)
	// SetBalanceRole retags a classification's balance role. Intended for
	// expand-style upgrades (BalanceRoleNone -> a real role) — changing
	// between two non-empty roles re-buckets historical balances in the
	// breakdown view and should be treated as a deliberate migration.
	SetBalanceRole(ctx context.Context, uid string, role BalanceRole) error
	// SetDisplayLabelIfEmpty sets the user-facing display label only when the
	// current label is '' — presets use it to seed defaults on existing
	// installs without ever clobbering an operator's override.
	SetDisplayLabelIfEmpty(ctx context.Context, uid string, label string) error
	// SetLifecycleIfEmpty seeds a classification's lifecycle only when it
	// currently has none ('{}') — for rows that predate the lifecycle column
	// (e.g. migration 011's seed 'deposit'/'withdraw' classifications) and
	// were never assigned one. Same expand-safe stance as
	// SetDisplayLabelIfEmpty: never clobbers a lifecycle an operator has
	// since customized.
	SetLifecycleIfEmpty(ctx context.Context, uid string, lifecycle *Lifecycle) error
}

type ClassificationInput struct {
	Code         string
	Name         string
	NormalSide   NormalSide
	IsSystem     bool
	DisplayLabel string
	BalanceRole  BalanceRole
	Lifecycle    *Lifecycle
}

// Validate checks that the input is well-formed and, for a non-system
// classification, that BalanceRole was explicitly declared (M-4 fix,
// docs/INVARIANTS.md I-37 addendum: `.local/independent-review-2026-08-26.md`,
// docs/plans/2026-08-26-audit-remediation-contracts.md follow-on
// fix-backend-1 batch, board #43).
//
// Before this rule, BalanceRoleNone ("") carried two different intents for a
// non-system classification -- "this is a deliberate memo/cost account, not
// a liability" (fee_expense) and "nobody has tagged this yet" -- and nothing
// distinguished them. In this library's own convention a real user-side
// liability is NOT necessarily credit-normal (main_wallet, the canonical
// liability-shaped classification, is debit-normal by construction: DR
// increases what the platform owes the holder), so normal_side cannot be
// used to infer intent either -- balance_role is the ONLY signal, and this
// is exactly the field that goes missing when a classification is copied
// from an existing preset without also copying its role. Requiring an
// explicit choice -- BalanceRoleAvailable/Pending/Locked for a real
// liability bucket, or the new BalanceRoleMemo for a deliberate
// non-liability memo/cost account -- closes the ambiguity at the only point
// it can structurally be closed: creation time, before the information is
// gone. is_system classifications are exempt: they are never part of the
// holder-facing breakdown by construction and BalanceRoleNone remains their
// natural (and only sensible) value.
func (i ClassificationInput) Validate() error {
	if i.Code == "" {
		return fmt.Errorf("core: classification: code required: %w", ErrInvalidInput)
	}
	if i.Name == "" {
		return fmt.Errorf("core: classification: name required: %w", ErrInvalidInput)
	}
	if !i.NormalSide.IsValid() {
		return fmt.Errorf("core: classification: invalid normal side %q: %w", i.NormalSide, ErrInvalidInput)
	}
	if !i.BalanceRole.IsValid() {
		return fmt.Errorf("core: classification: invalid balance role %q: %w", i.BalanceRole, ErrInvalidInput)
	}
	if !i.IsSystem && i.BalanceRole == BalanceRoleNone {
		return fmt.Errorf("core: classification: non-system classification %q must declare an explicit balance_role (available/pending/locked for a real liability bucket, or memo for a deliberate non-liability memo/cost account): %w", i.Code, ErrInvalidInput)
	}
	return nil
}

// JournalTypeStore manages dynamic journal types.
type JournalTypeStore interface {
	CreateJournalType(ctx context.Context, input JournalTypeInput) (*JournalType, error)
	GetJournalTypeByCode(ctx context.Context, code string) (*JournalType, error)
	DeactivateJournalType(ctx context.Context, uid string) error
	ListJournalTypes(ctx context.Context, activeOnly bool) ([]JournalType, error)
	// SetDisplayLabelIfEmpty sets the user-facing display label only when the
	// current label is '' (see ClassificationStore.SetDisplayLabelIfEmpty).
	SetDisplayLabelIfEmpty(ctx context.Context, uid string, label string) error
}

type JournalTypeInput struct {
	Code         string
	Name         string
	DisplayLabel string
}

// HolderReader serves the holder-scoped wallet read surface: balances,
// translated transactions, and active holds for ONE account holder
// (docs/plans/2026-07-08-holder-scoped-wallet-surface.md). Read-only.
type HolderReader interface {
	// ListHolderBalances returns one HolderBalance per currency the holder
	// has touched. currencyUID filters to a single currency when non-empty.
	ListHolderBalances(ctx context.Context, holder int64, currencyUID string) ([]HolderBalance, error)
	// ListHolderTransactions returns the translated transaction view, newest
	// first, cursor-paginated at journal granularity (a journal's rows are
	// never split across pages). Empty cursor starts from the newest.
	ListHolderTransactions(ctx context.Context, holder int64, cursor string, limit int32) ([]HolderTransaction, string, error)
	// ListHolderHolds returns the holder's outstanding reservation holds.
	ListHolderHolds(ctx context.Context, holder int64) ([]HolderHold, error)
}

// TemplateStore manages entry templates.
type TemplateStore interface {
	CreateTemplate(ctx context.Context, input TemplateInput) (*EntryTemplate, error)
	DeactivateTemplate(ctx context.Context, uid string) error
	GetTemplate(ctx context.Context, code string) (*EntryTemplate, error)
	ListTemplates(ctx context.Context, activeOnly bool) ([]EntryTemplate, error)
}

type TemplateInput struct {
	Code           string
	Name           string
	JournalTypeUID string
	Lines          []TemplateLineInput
}

type TemplateLineInput struct {
	ClassificationUID string
	EntryType         EntryType
	HolderRole        HolderRole
	AmountKey         string
	SortOrder         int
}

// CurrencyStore manages currencies.
type CurrencyStore interface {
	CreateCurrency(ctx context.Context, input CurrencyInput) (*Currency, error)
	DeactivateCurrency(ctx context.Context, uid string) error
	ListCurrencies(ctx context.Context, activeOnly bool) ([]Currency, error)
	GetCurrency(ctx context.Context, uid string) (*Currency, error)
}

type CurrencyInput struct {
	Code string
	Name string
	// Exponent is the maximum number of decimal places entries in this
	// currency may carry. Required — zero is a legitimate value (e.g. JPY),
	// not a "use the default" sentinel, so callers must state it explicitly.
	// Must be in [0, 18].
	Exponent int32
}

func (i CurrencyInput) Validate() error {
	if i.Code == "" {
		return fmt.Errorf("core: currency: code required: %w", ErrInvalidInput)
	}
	if i.Name == "" {
		return fmt.Errorf("core: currency: name required: %w", ErrInvalidInput)
	}
	if i.Exponent < 0 || i.Exponent > 18 {
		return fmt.Errorf("core: currency: exponent must be between 0 and 18, got %d: %w", i.Exponent, ErrInvalidInput)
	}
	return nil
}

// AccountPolicyStore manages per-dimension account freeze/close + balance-floor
// overrides. See core.AccountPolicy for the dimension model and
// docs/INVARIANTS.md I-17 for the enforcement contract.
type AccountPolicyStore interface {
	// SetPolicy creates or updates the policy for the exact
	// (account_holder, currency_id, classification_id) dimension in input,
	// appending an audit row (account_policy_changes) in the same transaction.
	SetPolicy(ctx context.Context, input AccountPolicyInput) (*AccountPolicy, error)
	// GetPolicy returns the policy row for the exact dimension (no priority
	// matching — use the write-path's internal resolver for "effective
	// policy" lookups). Returns ErrNotFound if no row exists at that exact
	// dimension.
	GetPolicy(ctx context.Context, holder int64, currencyUID, classificationUID string) (*AccountPolicy, error)
	// ListPolicies returns every policy row for holder, across all
	// currencies and classifications.
	ListPolicies(ctx context.Context, holder int64) ([]AccountPolicy, error)
}

// PeriodCloser manages the accounting period close line (append-only,
// latest-row-wins). See docs/INVARIANTS.md I-15.
type PeriodCloser interface {
	ClosePeriod(ctx context.Context, input ClosePeriodInput) (*PeriodClose, error)
	// ActiveCloseLine returns the current close_before line, or the zero Time
	// if the period has never been closed.
	ActiveCloseLine(ctx context.Context) (time.Time, error)
	ListPeriodCloses(ctx context.Context, limit int) ([]PeriodClose, error)
}

// TrialBalanceReader computes a trial balance report.
type TrialBalanceReader interface {
	TrialBalance(ctx context.Context, currencyUID string, asOf time.Time) (*TrialBalanceReport, error)
}

// AddressRegistry persists the one-holder-to-one-address deposit address
// registry (design doc §2). It is a pure store: callers derive the address
// with DeriveDepositAddress and pass the result in -- the registry never
// derives addresses itself.
type AddressRegistry interface {
	// EnsureAddress upserts input, returning the existing row unchanged if
	// holder was already registered (account_holder is UNIQUE, so a holder
	// can never be issued a second address). On conflict, input's
	// Address/Factory/InitHash are NOT compared against the existing row --
	// reconciling a mismatch is the caller's responsibility, not the
	// store's.
	EnsureAddress(ctx context.Context, input AddressRegistrationInput) (*DepositAddress, error)
	// GetByAddress reverse-looks-up the holder for an observed on-chain
	// address. address must be in the same canonical EIP-55 casing the row
	// was registered with. Returns ErrNotFound if unregistered.
	GetByAddress(ctx context.Context, address string) (*DepositAddress, error)
	// ListAddresses returns every registered deposit address, for the
	// watcher to build its `to ∈ registry` filter set.
	ListAddresses(ctx context.Context) ([]DepositAddress, error)
}

// ChainCursorStore persists the deposit watcher's per-chain log-scan
// progress (core.ChainCursor), so a restart resumes from where it left off
// instead of rescanning from genesis or silently skipping unseen blocks
// (design doc §3/§6). Implemented by postgres.ChainCursorStore.
type ChainCursorStore interface {
	// GetCursor returns chainID's cursor. Returns ErrNotFound if the chain
	// has never been scanned -- callers should start from block 0 (or a
	// configured start height) in that case.
	GetCursor(ctx context.Context, chainID int64) (*ChainCursor, error)
	// SetCursor advances chainID's cursor to lastScannedBlock (upsert).
	// Callers must call this monotonically; the store does not enforce it.
	SetCursor(ctx context.Context, chainID int64, lastScannedBlock int64) error
}

// ChainScanner enumerates on-chain balances for the sweep job. One
// implementation per chain family -- chains/evm is the only one this period
// (design doc §1/§4).
type ChainScanner interface {
	// ScanBalances returns the current balance of token (a contract address,
	// or core.SweepNativeToken for the chain's native asset) at every
	// address in addresses, on chainID.
	ScanBalances(ctx context.Context, chainID int64, token string, addresses []string) (map[string]decimal.Decimal, error)
}

// ChainReader reads chain state for the deposit watcher (service/onchain.go):
// forward-scanning for new deposits, recheck-polling for confirmation
// advancement, and reorg detection (design doc §3/§6). One implementation
// per chain family -- chains/evm is the only one this period.
type ChainReader interface {
	// LatestBlock returns the current chain tip for chainID.
	LatestBlock(ctx context.Context, chainID int64) (int64, error)
	// FetchDeposits scans [fromBlock, toBlock] (inclusive) for ERC-20 Transfer
	// logs whose `to` is in addresses, returning normalized sightings with
	// Confirmations computed against the chain tip at scan time. Callers are
	// responsible for chunking large address lists to whatever limit the
	// underlying provider imposes (design doc §3: "provider topic 上限").
	FetchDeposits(ctx context.Context, chainID int64, fromBlock, toBlock int64, addresses []string) ([]DepositSighting, error)
	// TxIncluded reports whether txHash is still included in the canonical
	// chain -- used both for the shallow-reorg recheck (a pending/confirming
	// booking's tx vanished before reaching the confirmation threshold) and
	// the deep-reorg recheck (a confirmed booking's tx vanished after).
	TxIncluded(ctx context.Context, chainID int64, txHash string) (bool, error)
}

// Sweeper collects balances from a batch of registered deposit addresses
// into the deploying factory's configured treasury (design doc §4). Sweep
// bookings never post a journal -- see presets.SweepLifecycle -- so this
// port only needs to report the collection transaction's hash for the
// caller to track.
//
// Nonce management is "record first, then broadcast" (design doc §4): the
// caller obtains a nonce via NextNonce, persists it on the sweep booking's
// metadata *before* calling BatchSweep, and reuses that same persisted nonce
// on every retry -- including gas-bump replacements, which re-call BatchSweep
// with the identical nonce rather than requesting a new one.
type Sweeper interface {
	// NextNonce returns the next usable account nonce for the sweeper key on
	// chainID. Callers must persist the result before broadcasting anything
	// with it (design doc §4 "先记后发").
	NextNonce(ctx context.Context, chainID int64) (uint64, error)
	// BatchSweep builds, signs (via Signer), and broadcasts
	// factory.batchSweep for token over targets using the pinned
	// signerNonce, returning the resulting transaction hash. Re-calling with
	// the same signerNonce is a gas-bumped replacement, not a new
	// transaction -- the caller tracks the latest returned hash for
	// confirmation polling. targets carries AccountHolder alongside each
	// address because the factory's batchSweep ABI takes CREATE2 salts
	// (nonces), not addresses -- CREATE2 is one-way, so the adapter cannot
	// recover a holder's salt from its address alone.
	//
	// priorTxHash is the hash of the transaction this call is replacing --
	// empty on the first-ever dispatch for signerNonce. Callers should pass
	// the most durable hash they have (a booking's persisted ChannelRef, at
	// minimum): an implementation MAY use it to read the actual fee still
	// pending on chain (via a node RPC) as the floor a gas-bump replacement
	// must beat, rather than relying only on its own process-local memory of
	// the last fee it used -- that memory does not survive a restart, while
	// a hash sourced from durable storage does (onchain-money-path.md).
	BatchSweep(ctx context.Context, chainID int64, token string, targets []SweepTarget, signerNonce uint64, priorTxHash string) (txHash string, err error)
	// GasPrice returns the current gas price (gwei) on chainID, for the
	// caller to compare against SweepPolicy.GasCeiling before broadcasting
	// or gas-bumping. An implementation MUST report the same basis it will
	// actually pay in BatchSweep's non-retry fee cap -- reporting a
	// different, lower-tending estimate here (e.g. a legacy suggested gas
	// price when the real payment uses a wider EIP-1559 fee-cap formula)
	// makes GasCeiling a soft threshold instead of the upper bound its own
	// doc comment (core.SweepPolicy.GasCeiling) promises
	// (onchain-money-path.md Minor).
	GasPrice(ctx context.Context, chainID int64) (decimal.Decimal, error)
}

// DepositConfirmer is the deposit path's second, independent data source for
// reconciliation (design doc §9.3: M3 compensating controls). A consumer
// wires this by pointing a second core.ChainReader-equivalent implementation
// (chains/evm's Reader already satisfies this method shape) at a DIFFERENT
// RPC provider -- no new adapter code is required, just a second instance.
// Nil (the default) disables the reconciliation gate entirely; only the
// threshold gate (TokenConfig.AutoCreditCeiling) applies.
type DepositConfirmer interface {
	// ConfirmDeposit re-derives, from this provider's own view of the chain,
	// the amount transferred by the log at (chainID, txHash, txLogSeq) and
	// whether that transaction is currently included on the canonical chain.
	// The caller compares amount/included against the primary sighting;
	// disagreement (either source) routes the deposit to review rather than
	// auto-crediting it.
	ConfirmDeposit(ctx context.Context, chainID int64, txHash string, txLogSeq int32) (amount decimal.Decimal, included bool, err error)
}

// Signer abstracts the private key that authorizes sweep transactions, so
// the library's default local-key implementation can later be swapped for a
// KMS/HSM adapter without touching sweep orchestration (design doc §0).
// Signer never touches factory ownership/treasury-change keys (design doc
// §5.5) -- it only signs sweep transactions.
type Signer interface {
	SignTx(ctx context.Context, chainID int64, unsignedTx []byte) (signedTx []byte, err error)
}

// Attestor abstracts the key that authorizes a posting (design doc §7,
// P5). The private key must never enter the database: a DB compromise
// alone must not yield the ability to mint a valid authorization. Beyond
// that, key custody (a local in-process key, a KMS/HSM, or anything else)
// is an implementation detail behind this port -- the library ships
// authdev.LocalAttestor as its default, production-ready implementation
// (Team Lead's 2026-08-21 simplification: a remote KMS's latency/
// availability tradeoffs were solving a deployment problem this project
// does not have; a monolith's own threat model already concedes "app
// process + signing key both compromised" as out of scope, design doc §1
// non-goal 2, so a local key satisfies the same guarantee a remote one
// would). Deliberately distinct from Signer (EVM sweep transactions) --
// different key, different blast radius, never the same instance
// (integrity contracts §4).
//
// Deployment note (not enforced in code -- a config-loading concern for
// the composition root): the signing key must not live in the same
// secrets store / env bundle as DATABASE_URL. A single leaked bundle
// should not both let an attacker write to the DB AND sign a journal as
// if it were legitimate -- that would collapse the two rows design doc §1
// keeps separate ("app DB 凭证" vs "app + KMS 同时失陷").
type Attestor interface {
	Sign(ctx context.Context, digest []byte) (signature []byte, keyID string, err error)
}

// AuthVerifier needs only the public key, so verification can run entirely
// outside the database host -- that independence is the whole point
// (design doc §7, P5).
type AuthVerifier interface {
	Verify(ctx context.Context, digest, signature []byte, keyID string) error
}

// VerifiedBalanceReader computes a dimension's balance while additionally
// requiring every journal that contributed an entry to that dimension to
// carry a valid P5 per-journal authorization (design doc §7,
// contracts §W2-1/W2-2). Same signature shape as
// CheckpointIntegrityStore.RecomputeBalance deliberately -- both are
// entries-only, checkpoint-independent recomputes; this one adds an
// authorization gate on top.
//
// Unlike RecomputeBalance, this can come back UNDEFINED. If even one
// contributing journal fails core.VerifyJournalAuth, the result is NOT "the
// balance computed while skipping that journal": a reversal journal's net
// contribution to a dimension can be negative, so silently excluding an
// unauthorized journal could report a balance HIGHER than the true one --
// worse than refusing to answer (contracts §W2-1). UNDEFINED is signaled by
// a non-nil error wrapping core.ErrUnauthorizedJournal; the returned
// decimal.Decimal is the zero value in that case and MUST NOT be read as a
// real balance. Callers must check the error before using the amount --
// same discipline as every other error-returning balance method in this
// package (GetBalance, RecomputeBalance), not a new pattern.
//
// A dimension with zero contributing journals returns (decimal.Zero, nil):
// "every journal that touched this account is authorized" is vacuously
// true when no journal ever touched it.
//
// This is a mechanism, not a policy: the library does not decide whether
// any given call site (Reserve, a withdrawal handler, ...) should call
// this at all, nor at what amount threshold -- that is entirely the
// consumer's choice (contracts §W2-3). This interface's shape is also the
// seam a future batch-attestation-backed implementation (T4, not started
// this wave) would replace behind, without changing any caller
// (contracts §W2-2) -- the naive reference implementation
// (postgres.VerifiedBalanceStore) verifies every contributing journal
// individually; a T4 implementation could instead consult a signed batch
// digest covering many journals at once, but the signature (this
// interface) stays the same either way.
type VerifiedBalanceReader interface {
	VerifiedBalance(ctx context.Context, holder int64, currencyUID, classificationUID string) (decimal.Decimal, error)
}

// Anchor publishes an attestation head to storage the ledger's own database
// credentials cannot reach (design doc §8.3, P6). Implementations live in
// the consumer's composition root (object-lock bucket in a separate cloud
// account; optionally a public chain). The library ships only a
// local-filesystem implementation for dev (integrity contracts §7: the
// anchor's carrier is a real, unresolved deployment choice -- unlike the
// signing key, the whole point of an anchor is living somewhere the
// ledger's own DB credentials cannot reach, so "just use a local key" is
// not an equivalent simplification here).
type Anchor interface {
	// Publish is idempotent per seq: re-publishing the same seq with identical
	// bytes must succeed, with different bytes must return an error.
	Publish(ctx context.Context, seq int64, head []byte) error
	// Head returns the highest seq the anchor knows about, or 0 if empty.
	// It must read from the anchor, never from the ledger database.
	Head(ctx context.Context) (seq int64, head []byte, err error)
}
