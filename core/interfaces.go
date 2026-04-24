package core

import (
	"context"
	"time"

	"github.com/shopspring/decimal"
)

// JournalWriter handles journal posting.
type JournalWriter interface {
	PostJournal(ctx context.Context, input JournalInput) (*Journal, error)
	ExecuteTemplate(ctx context.Context, templateCode string, params TemplateParams) (*Journal, error)
	ReverseJournal(ctx context.Context, journalID int64, reason string) (*Journal, error)
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
	Settle(ctx context.Context, reservationID int64, actualAmount decimal.Decimal) error
	Release(ctx context.Context, reservationID int64) error
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
	ListCurrencies(ctx context.Context) ([]Currency, error)
	GetCurrency(ctx context.Context, id int64) (*Currency, error)
}

type CurrencyInput struct {
	Code string
	Name string
}
