package core

import (
	"context"
	"time"
)

// QueryProvider composes all read-only query interfaces for the API layer.
type QueryProvider interface {
	JournalQuerier
	EntryQuerier
	ReservationQuerier
	SnapshotQuerier
	SystemRollupQuerier
	HealthQuerier
}

// JournalQuerier lists journals with cursor pagination.
type JournalQuerier interface {
	GetJournal(ctx context.Context, id int64) (*Journal, []Entry, error)
	ListJournals(ctx context.Context, cursorID int64, limit int32) ([]Journal, error)
}

// EntryQuerier lists entries with cursor pagination.
type EntryQuerier interface {
	ListEntriesByAccount(ctx context.Context, holder, currencyID, cursorID int64, limit int32) ([]Entry, error)
}

// ReservationQuerier lists reservations.
type ReservationQuerier interface {
	ListReservations(ctx context.Context, holder int64, status string, limit int32) ([]Reservation, error)
}


// SnapshotQuerier queries snapshots by date range.
type SnapshotQuerier interface {
	ListSnapshotsByDateRange(ctx context.Context, holder, currencyID int64, start, end time.Time) ([]BalanceSnapshot, error)
}

// SystemRollupQuerier reads system rollup balances.
type SystemRollupQuerier interface {
	GetSystemRollups(ctx context.Context) ([]SystemRollup, error)
}

// HealthQuerier provides system health metrics.
type HealthQuerier interface {
	GetHealthMetrics(ctx context.Context) (*HealthMetrics, error)
}

// HealthMetrics holds system health data points.
type HealthMetrics struct {
	RollupQueueDepth         int64
	CheckpointMaxAgeSeconds  int
	ActiveReservations       int64
}
