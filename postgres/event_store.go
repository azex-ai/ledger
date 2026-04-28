package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
	"github.com/jackc/pgx/v5/pgxpool"
)

var _ core.EventReader = (*EventStore)(nil)

const eventClaimLease = 2 * time.Minute

// EventStore implements core.EventReader and event delivery helpers.
//
// In pool mode (constructed via NewEventStore), queries run against the pool.
// In tx mode (bound via withDB), queries participate in the caller's transaction.
type EventStore struct {
	// pool is non-nil only in pool mode. Nil signals tx mode.
	pool       *pgxpool.Pool
	q          *sqlcgen.Queries
	claimLease time.Duration
}

// NewEventStore creates a new EventStore. The internal sqlc Queries instance
// is built from pool so library consumers don't need to import the generated
// sqlcgen package.
func NewEventStore(pool *pgxpool.Pool) *EventStore {
	return &EventStore{pool: pool, q: sqlcgen.New(pool), claimLease: eventClaimLease}
}

// WithDB returns a clone of the EventStore bound to an existing transaction.
func (s *EventStore) WithDB(db DBTX) *EventStore {
	return &EventStore{
		pool:       nil, // tx mode
		q:          sqlcgen.New(db),
		claimLease: s.claimLease,
	}
}

// SetClaimLease overrides the default event delivery lease duration.
func (s *EventStore) SetClaimLease(d time.Duration) {
	if d > 0 {
		s.claimLease = d
	}
}

// GetEvent returns an event by ID.
func (s *EventStore) GetEvent(ctx context.Context, id int64) (*core.Event, error) {
	row, err := s.q.GetEvent(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("postgres: get event %d: %w", id, err)
	}
	return eventFromRow(row), nil
}

// ListEvents returns events matching the filter.
func (s *EventStore) ListEvents(ctx context.Context, filter core.EventFilter) ([]core.Event, error) {
	rows, err := s.q.ListEventsByFilter(ctx, sqlcgen.ListEventsByFilterParams{
		ClassificationCode: filter.ClassificationCode,
		BookingID:          filter.BookingID,
		ToStatus:           filter.ToStatus,
		ID:                 filter.Cursor,
		Limit:              int32(filter.Limit),
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: list events: %w", err)
	}
	events := make([]core.Event, len(rows))
	for i, row := range rows {
		events[i] = *eventFromRow(row)
	}
	return events, nil
}

// GetPendingEvents returns events that are pending delivery.
func (s *EventStore) GetPendingEvents(ctx context.Context, limit int) ([]core.Event, error) {
	rows, err := s.q.GetPendingEvents(ctx, sqlcgen.GetPendingEventsParams{
		Limit:         int32(limit),
		NextAttemptAt: time.Now().Add(s.claimLease),
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: get pending events: %w", err)
	}

	events := make([]core.Event, 0, limit)
	for _, row := range rows {
		events = append(events, *eventFromRow(row))
	}
	return events, nil
}

// MarkDelivered marks an event as successfully delivered.
func (s *EventStore) MarkDelivered(ctx context.Context, id int64) error {
	return s.q.UpdateEventDelivered(ctx, id)
}

// MarkRetry schedules an event for retry at the given time.
func (s *EventStore) MarkRetry(ctx context.Context, id int64, nextAttempt time.Time) error {
	if err := s.q.UpdateEventRetry(ctx, sqlcgen.UpdateEventRetryParams{
		ID:            id,
		NextAttemptAt: nextAttempt,
	}); err != nil {
		return fmt.Errorf("postgres: mark event retry: %w", err)
	}
	return nil
}

// MarkDead marks an event as permanently failed.
func (s *EventStore) MarkDead(ctx context.Context, id int64) error {
	return s.q.UpdateEventDead(ctx, id)
}
