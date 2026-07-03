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
	GetJournal(ctx context.Context, uid string) (*Journal, []Entry, error)
	// ListJournals returns one page plus the opaque cursor for the next
	// page ("" when exhausted).
	ListJournals(ctx context.Context, cursor string, limit int32) ([]Journal, string, error)
}

// EntryQuerier lists entries with cursor pagination.
type EntryQuerier interface {
	// ListEntriesByAccount returns one page plus the opaque cursor for the
	// next page ("" when exhausted).
	ListEntriesByAccount(ctx context.Context, holder int64, currencyUID string, cursor string, limit int32) ([]Entry, string, error)
}

// ReservationQuerier lists reservations.
type ReservationQuerier interface {
	// ListReservations pages newest-first; cursor is the opaque next_cursor
	// from the previous page ("" = first page). Returns the page plus the
	// next cursor ("" = exhausted).
	ListReservations(ctx context.Context, holder int64, status string, cursor string, limit int32) ([]Reservation, string, error)
}

// SnapshotQuerier queries snapshots by date range.
type SnapshotQuerier interface {
	ListSnapshotsByDateRange(ctx context.Context, holder int64, currencyUID string, start, end time.Time) ([]BalanceSnapshot, error)
}

// SystemRollupQuerier reads aggregated system-wide balances in the response
// shape historically used for rollup snapshots.
type SystemRollupQuerier interface {
	GetSystemRollups(ctx context.Context) ([]SystemRollup, error)
}

// HealthQuerier provides system health metrics.
type HealthQuerier interface {
	GetHealthMetrics(ctx context.Context) (*HealthMetrics, error)
}

// HealthMetrics holds system health data points.
type HealthMetrics struct {
	RollupQueueDepth        int64
	CheckpointMaxAgeSeconds int
	ActiveReservations      int64
}
