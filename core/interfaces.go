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

// Depositor handles deposit state machine.
type Depositor interface {
	InitDeposit(ctx context.Context, input DepositInput) (*Deposit, error)
	ConfirmingDeposit(ctx context.Context, depositID int64, channelRef string) error
	ConfirmDeposit(ctx context.Context, input ConfirmDepositInput) error
	FailDeposit(ctx context.Context, depositID int64, reason string) error
	ExpireDeposit(ctx context.Context, depositID int64) error
}

// Withdrawer handles withdrawal state machine.
type Withdrawer interface {
	InitWithdraw(ctx context.Context, input WithdrawInput) (*Withdrawal, error)
	ReserveWithdraw(ctx context.Context, withdrawalID int64) error
	ReviewWithdraw(ctx context.Context, withdrawalID int64, approved bool) error
	ProcessWithdraw(ctx context.Context, withdrawalID int64, channelRef string) error
	ConfirmWithdraw(ctx context.Context, withdrawalID int64) error
	FailWithdraw(ctx context.Context, withdrawalID int64, reason string) error
	RetryWithdraw(ctx context.Context, withdrawalID int64) error
}

// ChannelAdapter abstracts a deposit/withdraw channel.
type ChannelAdapter interface {
	Name() string
	SupportsDeposit() bool
	SupportsWithdraw() bool
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
	DeactivateClassification(ctx context.Context, id int64) error
	ListClassifications(ctx context.Context, activeOnly bool) ([]Classification, error)
}

type ClassificationInput struct {
	Code       string
	Name       string
	NormalSide NormalSide
	IsSystem   bool
}

// JournalTypeStore manages dynamic journal types.
type JournalTypeStore interface {
	CreateJournalType(ctx context.Context, input JournalTypeInput) (*JournalType, error)
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
