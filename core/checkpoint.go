package core

import (
	"time"

	"github.com/shopspring/decimal"
)

// BalanceCheckpoint stores the materialized balance at a point in time. It
// is keyed on internal storage ids (CurrencyID/ClassificationID), not uids
// -- deliberately: this type is the rollup/reconcile engine's internal
// working representation (service.CheckpointReadWriter and friends), never
// returned by any Service accessor, so ids never cross into a public shape
// here (same convention as service.ClassificationDim's doc comment:
// "internal ids never leave the service"). Do not add a Service-facing
// method that returns this type directly -- see RebuiltCheckpoint for the
// uid-based type that DOES cross the library API boundary
// (CheckpointIntegrityStore.RebuildCheckpoint, I-18).
type BalanceCheckpoint struct {
	AccountHolder    int64           `json:"account_holder"`
	CurrencyID       int64           `json:"currency_id"`
	ClassificationID int64           `json:"classification_id"`
	Balance          decimal.Decimal `json:"balance"`
	LastEntryID      int64           `json:"last_entry_id"`
	LastEntryAt      time.Time       `json:"last_entry_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

// RebuiltCheckpoint is the trusted-repair result returned to library
// consumers by CheckpointIntegrityStore.RebuildCheckpoint -- the one place
// a checkpoint crosses from the internal, id-keyed BalanceCheckpoint
// representation into the public library API
// (Service.CheckpointIntegrity()). Per I-18 it speaks uids exclusively; it
// deliberately omits the internal entry-watermark (BalanceCheckpoint's
// LastEntryID) rather than exposing a journal_entries row id, since that
// value is meaningful only to the rollup engine that consumes it, not to a
// library caller auditing a repair.
type RebuiltCheckpoint struct {
	AccountHolder     int64           `json:"account_holder"`
	CurrencyUID       string          `json:"currency_uid"`
	ClassificationUID string          `json:"classification_uid"`
	Balance           decimal.Decimal `json:"balance"`
	LastEntryAt       time.Time       `json:"last_entry_at"`
}

// RollupQueueItem represents a pending rollup work item.
type RollupQueueItem struct {
	ID               int64     `json:"id"`
	AccountHolder    int64     `json:"account_holder"`
	CurrencyID       int64     `json:"currency_id"`
	ClassificationID int64     `json:"classification_id"`
	CreatedAt        time.Time `json:"created_at"`
	// ClaimedUntil is the claim token set at dequeue. It is passed back to
	// MarkRollupProcessed / ReleaseRollupClaim so a worker only acts on a claim
	// it still owns (a concurrent re-dirty or re-claim changes this value).
	ClaimedUntil time.Time `json:"claimed_until"`
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
