package core

import (
	"time"

	"github.com/shopspring/decimal"
)

// BalanceCheckpoint stores the materialized balance at a point in time.
type BalanceCheckpoint struct {
	AccountHolder    int64
	CurrencyID       int64
	ClassificationID int64
	Balance          decimal.Decimal
	LastEntryID      int64
	LastEntryAt      time.Time
	UpdatedAt        time.Time
}

// RollupQueueItem represents a pending rollup work item.
type RollupQueueItem struct {
	ID               int64
	AccountHolder    int64
	CurrencyID       int64
	ClassificationID int64
	CreatedAt        time.Time
}

// BalanceSnapshot stores a historical daily balance.
type BalanceSnapshot struct {
	AccountHolder    int64
	CurrencyID       int64
	ClassificationID int64
	SnapshotDate     time.Time
	Balance          decimal.Decimal
}

// SystemRollup stores aggregated system-wide balances.
type SystemRollup struct {
	CurrencyID       int64
	ClassificationID int64
	TotalBalance     decimal.Decimal
	UpdatedAt        time.Time
}
