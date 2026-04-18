package server

import (
	"context"
	"time"

	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
)

// JournalQuerier lists journals with cursor pagination.
type JournalQuerier interface {
	GetJournal(ctx context.Context, id int64) (*core.Journal, []core.Entry, error)
	ListJournals(ctx context.Context, cursorID int64, limit int32) ([]core.Journal, error)
}

// EntryQuerier lists entries with cursor pagination.
type EntryQuerier interface {
	ListEntriesByAccount(ctx context.Context, holder, currencyID, cursorID int64, limit int32) ([]core.Entry, error)
}

// ReservationQuerier lists reservations.
type ReservationQuerier interface {
	ListReservations(ctx context.Context, holder int64, status string, limit int32) ([]core.Reservation, error)
}

// DepositQuerier lists deposits.
type DepositQuerier interface {
	ListDeposits(ctx context.Context, holder int64, limit int32) ([]core.Deposit, error)
}

// WithdrawalQuerier lists withdrawals.
type WithdrawalQuerier interface {
	ListWithdrawals(ctx context.Context, holder int64, limit int32) ([]core.Withdrawal, error)
}

// SnapshotQuerier queries snapshots by date range.
type SnapshotQuerier interface {
	ListSnapshotsByDateRange(ctx context.Context, holder, currencyID int64, start, end time.Time) ([]core.BalanceSnapshot, error)
}

// SystemRollupQuerier reads system rollup balances.
type SystemRollupQuerier interface {
	GetSystemRollups(ctx context.Context) ([]SystemRollupBalance, error)
}

// SystemRollupBalance is the API representation of a system rollup.
type SystemRollupBalance struct {
	CurrencyID       int64           `json:"currency_id"`
	ClassificationID int64           `json:"classification_id"`
	TotalBalance     decimal.Decimal `json:"total_balance"`
	UpdatedAt        time.Time       `json:"updated_at"`
}
