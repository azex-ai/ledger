package core

import (
	"time"

	"github.com/shopspring/decimal"
)

// BalanceCheckpoint is the uid-based checkpoint value that crosses the
// library API boundary -- currently the sole such crossing point is
// CheckpointIntegrityStore.RebuildCheckpoint (reached via
// Service.CheckpointIntegrity()). Per I-18 it speaks uids exclusively; it
// deliberately omits the internal entry-watermark
// (service.BalanceCheckpoint's LastEntryID) rather than exposing a
// journal_entries row id, since that value is meaningful only to the
// rollup engine that consumes it, not to a library caller auditing a
// repair.
//
// The rollup/reconcile engine's own internal, id-keyed working
// representation (CurrencyID/ClassificationID int64) lives in
// service.BalanceCheckpoint, not here -- it is never returned by any
// Service accessor, so it never needs to cross this boundary (same
// convention as service.ClassificationDim's doc comment: "internal ids
// never leave the service").
type BalanceCheckpoint struct {
	AccountHolder     int64           `json:"account_holder"`
	CurrencyUID       string          `json:"currency_uid"`
	ClassificationUID string          `json:"classification_uid"`
	Balance           decimal.Decimal `json:"balance"`
	LastEntryAt       time.Time       `json:"last_entry_at"`
}

// BalanceSnapshot stores a historical daily balance.
type BalanceSnapshot struct {
	AccountHolder     int64           `json:"account_holder"`
	CurrencyUID       string          `json:"currency_uid"`
	ClassificationUID string          `json:"classification_uid"`
	SnapshotDate      time.Time       `json:"snapshot_date"`
	Balance           decimal.Decimal `json:"balance"`
}

// SystemRollup stores aggregated system-wide balances.
type SystemRollup struct {
	CurrencyUID       string          `json:"currency_uid"`
	ClassificationUID string          `json:"classification_uid"`
	TotalBalance      decimal.Decimal `json:"total_balance"`
	UpdatedAt         time.Time       `json:"updated_at"`
}
