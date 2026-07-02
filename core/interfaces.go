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
	ReverseJournal(ctx context.Context, journalID int64, reason string) (*Journal, error)
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
	ReverseJournalFraction(ctx context.Context, journalID int64, num, den int64, reason string, idempotencyKey string) (*Journal, error)
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
	GetBalance(ctx context.Context, holder int64, currencyID, classificationID int64) (decimal.Decimal, error)
	GetBalances(ctx context.Context, holder int64, currencyID int64) ([]Balance, error)
	BatchGetBalances(ctx context.Context, holderIDs []int64, currencyID int64) (map[int64][]Balance, error)
}

// Reserver handles reserve/settle/lock flow.
type Reserver interface {
	Reserve(ctx context.Context, input ReserveInput) (*Reservation, error)
	// Settle marks an active reservation as settled with the actual amount
	// consumed. actualAmount must be positive and must not exceed the
	// reservation's reserved amount — over-settlement is rejected with
	// ErrInvalidInput, never silently clamped. The unused remainder (reserved
	// minus actual) is implicitly released by the settle transition.
	Settle(ctx context.Context, reservationID int64, actualAmount decimal.Decimal) error
	// Release cancels an active reservation, freeing its entire reserved
	// amount without any accounting effect. It is a no-op on the ledger
	// balance beyond removing the hold — no partial release is supported.
	Release(ctx context.Context, reservationID int64) error
	// SettlePartial settles part of a reservation. amount must be positive.
	// The first call transitions the reservation from active to settling;
	// subsequent calls accumulate settled_amount further, which must never
	// exceed reserved_amount (ErrInvalidInput on overshoot). Once a
	// reservation enters settling, its remaining hold is no longer counted by
	// HeldAmount (mirroring the one-shot Settle's "unused remainder is
	// implicitly released" behavior) — call FinalizeSettlement when no more
	// partial settlements will follow. Calling Settle (the one-shot method)
	// on a settling reservation is rejected; use FinalizeSettlement instead.
	SettlePartial(ctx context.Context, reservationID int64, amount decimal.Decimal) error
	// FinalizeSettlement completes a reservation that has been partially
	// settled via SettlePartial, transitioning it from settling to settled.
	// It is rejected (ErrInvalidTransition) on any other status — in
	// particular, calling it on an active reservation that never received a
	// SettlePartial call is not a valid "settle everything" shortcut; use
	// Settle for that.
	FinalizeSettlement(ctx context.Context, reservationID int64) error
	// HeldAmount returns the sum of reserved_amount across the holder's active
	// reservations in the given currency — the exact figure Reserve subtracts
	// from balance to compute available. Consumers should call this instead of
	// querying the reservations table directly, so available = balance − held
	// can be derived without depending on the ledger's internal schema.
	HeldAmount(ctx context.Context, holder, currencyID int64) (decimal.Decimal, error)
}

// Booker handles classification-driven booking lifecycle.
type Booker interface {
	CreateBooking(ctx context.Context, input CreateBookingInput) (*Booking, error)
	Transition(ctx context.Context, input TransitionInput) (*Event, error)
}

// BookingReader handles booking queries.
type BookingReader interface {
	GetBooking(ctx context.Context, id int64) (*Booking, error)
	ListBookings(ctx context.Context, filter BookingFilter) ([]Booking, error)
}

// EventReader handles event queries.
type EventReader interface {
	GetEvent(ctx context.Context, id int64) (*Event, error)
	ListEvents(ctx context.Context, filter EventFilter) ([]Event, error)
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
	ReconcileAccount(ctx context.Context, holder int64, currencyID int64) (*ReconcileResult, error)
}

// ReconcileResult holds the outcome of a reconciliation check.
type ReconcileResult struct {
	Balanced  bool
	Gap       decimal.Decimal
	Details   []ReconcileDetail
	CheckedAt time.Time
}

type ReconcileDetail struct {
	AccountHolder    int64
	CurrencyID       int64
	ClassificationID int64
	Expected         decimal.Decimal
	Actual           decimal.Decimal
	Drift            decimal.Decimal
}

// Snapshotter handles daily balance snapshots.
type Snapshotter interface {
	CreateDailySnapshot(ctx context.Context, date time.Time) error
	GetSnapshotBalance(ctx context.Context, holder int64, currencyID int64, date time.Time) ([]Balance, error)
}

// ClassificationStore manages dynamic classifications.
type ClassificationStore interface {
	CreateClassification(ctx context.Context, input ClassificationInput) (*Classification, error)
	GetByCode(ctx context.Context, code string) (*Classification, error)
	DeactivateClassification(ctx context.Context, id int64) error
	ListClassifications(ctx context.Context, activeOnly bool) ([]Classification, error)
}

type ClassificationInput struct {
	Code       string
	Name       string
	NormalSide NormalSide
	IsSystem   bool
	Lifecycle  *Lifecycle
}

// JournalTypeStore manages dynamic journal types.
type JournalTypeStore interface {
	CreateJournalType(ctx context.Context, input JournalTypeInput) (*JournalType, error)
	GetJournalTypeByCode(ctx context.Context, code string) (*JournalType, error)
	DeactivateJournalType(ctx context.Context, id int64) error
	ListJournalTypes(ctx context.Context, activeOnly bool) ([]JournalType, error)
}

type JournalTypeInput struct {
	Code string
	Name string
}

// TemplateStore manages entry templates.
type TemplateStore interface {
	CreateTemplate(ctx context.Context, input TemplateInput) (*EntryTemplate, error)
	DeactivateTemplate(ctx context.Context, id int64) error
	GetTemplate(ctx context.Context, code string) (*EntryTemplate, error)
	ListTemplates(ctx context.Context, activeOnly bool) ([]EntryTemplate, error)
}

type TemplateInput struct {
	Code          string
	Name          string
	JournalTypeID int64
	Lines         []TemplateLineInput
}

type TemplateLineInput struct {
	ClassificationID int64
	EntryType        EntryType
	HolderRole       HolderRole
	AmountKey        string
	SortOrder        int
}

// CurrencyStore manages currencies.
type CurrencyStore interface {
	CreateCurrency(ctx context.Context, input CurrencyInput) (*Currency, error)
	DeactivateCurrency(ctx context.Context, id int64) error
	ListCurrencies(ctx context.Context, activeOnly bool) ([]Currency, error)
	GetCurrency(ctx context.Context, id int64) (*Currency, error)
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
