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

// Reserver handles reserve/settle/lock flow.
type Reserver interface {
	Reserve(ctx context.Context, input ReserveInput) (*Reservation, error)
	// Settle marks an active reservation as settled with the actual amount
	// consumed. input.Amount must be positive and must not exceed the
	// reservation's reserved amount; over-settlement is rejected with
	// ErrInvalidInput, never silently clamped. The unused remainder (reserved
	// minus actual) is implicitly released by the settle transition.
	Settle(ctx context.Context, input SettleInput) error
	// Release cancels an active reservation, freeing its entire reserved
	// amount without any accounting effect. It is a no-op on the ledger
	// balance beyond removing the hold — no partial release is supported.
	Release(ctx context.Context, reservationUID string) error
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
	// Settle for that.
	FinalizeSettlement(ctx context.Context, reservationUID string) error
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
	BatchSweep(ctx context.Context, chainID int64, token string, targets []SweepTarget, signerNonce uint64) (txHash string, err error)
	// GasPrice returns the current suggested gas price (gwei) on chainID, for
	// the caller to compare against SweepPolicy.GasCeiling before
	// broadcasting or gas-bumping.
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
