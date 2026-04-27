package core

import (
	"time"

	"github.com/shopspring/decimal"
)

// BalanceCheckpoint stores the materialized balance at a point in time.
type BalanceCheckpoint struct {
	AccountHolder    int64           `json:"account_holder"`
	CurrencyID       int64           `json:"currency_id"`
	ClassificationID int64           `json:"classification_id"`
	Balance          decimal.Decimal `json:"balance"`
	LastEntryID      int64           `json:"last_entry_id"`
	LastEntryAt      time.Time       `json:"last_entry_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

// RollupQueueItem represents a pending rollup work item.
type RollupQueueItem struct {
	ID               int64     `json:"id"`
	AccountHolder    int64     `json:"account_holder"`
	CurrencyID       int64     `json:"currency_id"`
	ClassificationID int64     `json:"classification_id"`
	CreatedAt        time.Time `json:"created_at"`
}

// BalanceSnapshot stores a historical daily balance.
type BalanceSnapshot struct {
	AccountHolder    int64           `json:"account_holder"`
	CurrencyID       int64           `json:"currency_id"`
	ClassificationID int64           `json:"classification_id"`
	SnapshotDate     time.Time       `json:"snapshot_date"`
	Balance          decimal.Decimal `json:"balance"`
}

// SystemRollup stores aggregated system-wide balances.
type SystemRollup struct {
	CurrencyID       int64           `json:"currency_id"`
	ClassificationID int64           `json:"classification_id"`
	TotalBalance     decimal.Decimal `json:"total_balance"`
	UpdatedAt        time.Time       `json:"updated_at"`
}
