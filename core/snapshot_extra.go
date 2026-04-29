package core

import (
	"context"
	"time"
)

// SnapshotBackfiller fills missing historical snapshots for a date range.
// Implemented in service layer; exposed via ledger.Service for library callers.
type SnapshotBackfiller interface {
	// BackfillSnapshots inserts snapshots for every day in [fromDate, toDate]
	// that has journal activity. Errors on individual days are collected but do
	// not abort the remaining days.
	BackfillSnapshots(ctx context.Context, fromDate, toDate time.Time) (*BackfillResult, error)
}

// BackfillResult summarises the outcome of a backfill run.
type BackfillResult struct {
	FromDate         time.Time `json:"from_date"`
	ToDate           time.Time `json:"to_date"`
	DaysProcessed    int       `json:"days_processed"`
	SnapshotsCreated int       `json:"snapshots_created"`
	Errors           []string  `json:"errors"`
}

// SparseSnapshotter writes balance snapshots only when the balance has changed
// from the most recent previous snapshot for that account dimension.
type SparseSnapshotter interface {
	// UpsertSnapshotSparse inserts snap only when the balance differs from the
	// most recent existing snapshot for (AccountHolder, CurrencyID,
	// ClassificationID) before snap.SnapshotDate. Returns (inserted, error).
	UpsertSnapshotSparse(ctx context.Context, snap BalanceSnapshot) (bool, error)
}

// LiveBalanceMerger splices current live balances into a date-range snapshot
// result set when the range includes today.
type LiveBalanceMerger interface {
	// MergeWithLive returns snapshots for [startDate, endDate].  When endDate
	// is today (or in the future) the entry for today is replaced by the
	// current live balance from the checkpoint table.
	MergeWithLive(ctx context.Context, holder, currencyID int64, startDate, endDate time.Time) ([]BalanceSnapshot, error)
}

// SnapshotCountReader can report the total number of stored snapshots. Used
// by the startup backfill check.
type SnapshotCountReader interface {
	CountSnapshots(ctx context.Context) (int64, error)
	EarliestJournalDate(ctx context.Context) (time.Time, error)
}
