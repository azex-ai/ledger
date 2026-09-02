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
	// page ("" when exhausted). Pages ASCENDING from the cursor: an empty
	// cursor is the OLDEST page, not the newest one.
	ListJournals(ctx context.Context, cursor string, limit int32) ([]Journal, string, error)
	// ListRecentJournals returns up to limit journals, NEWEST FIRST, with
	// no cursor -- a fixed-size head sample, not a paginated walk.
	//
	// Deliberately a second method rather than a flag on ListJournals: the
	// two answer different questions, and conflating them is how
	// service.VerifyLedger's step 4 ended up sampling the OLDEST journals
	// while four places in the codebase described it as sampling the most
	// recent ones (2026-09-02 audit, tamper-evident.md M-1). A forged row
	// inserted today lands at the top of this list and nowhere in
	// ListJournals's first page.
	ListRecentJournals(ctx context.Context, limit int32) ([]Journal, error)
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
