package service

import (
	"time"

	"github.com/shopspring/decimal"
)

// BalanceCheckpoint is the rollup/reconcile engine's internal, id-keyed
// working representation of a materialized balance -- keyed on
// CurrencyID/ClassificationID, not uids, because that is the shape
// RollupQueuer/CheckpointReadWriter/CheckpointReader's postgres adapter
// operates on directly (dimension math, advisory-lock keys). It is never
// returned by any Service accessor, so it never crosses the library API
// boundary (I-18; same convention as ClassificationDim's doc comment:
// "internal ids never leave the service"). The one place a checkpoint DOES
// cross that boundary -- CheckpointIntegrityStore.RebuildCheckpoint -- uses
// the uid-based core.BalanceCheckpoint instead.
type BalanceCheckpoint struct {
	AccountHolder    int64
	CurrencyID       int64
	ClassificationID int64
	Balance          decimal.Decimal
	LastEntryID      int64
	LastEntryAt      time.Time
	UpdatedAt        time.Time
}

// RollupQueueItem represents a pending rollup work item -- the same kind of
// internal, id-keyed working representation as BalanceCheckpoint, and for
// the same reason never crosses the library API boundary.
type RollupQueueItem struct {
	ID               int64
	AccountHolder    int64
	CurrencyID       int64
	ClassificationID int64
	CreatedAt        time.Time
	// ClaimedUntil is the claim token set at dequeue. It is passed back to
	// MarkRollupProcessed / ReleaseRollupClaim so a worker only acts on a claim
	// it still owns (a concurrent re-dirty or re-claim changes this value).
	ClaimedUntil time.Time
}
