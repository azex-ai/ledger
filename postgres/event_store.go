package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

var _ core.EventReader = (*EventStore)(nil)

// EventStore implements core.EventReader and event delivery helpers.
type EventStore struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

// NewEventStore creates a new EventStore.
func NewEventStore(pool *pgxpool.Pool, q *sqlcgen.Queries) *EventStore {
	return &EventStore{pool: pool, q: q}
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
	rows, err := s.q.GetPendingEvents(ctx, int32(limit))
	if err != nil {
		return nil, fmt.Errorf("postgres: get pending events: %w", err)
	}
	events := make([]core.Event, len(rows))
	for i, row := range rows {
		events[i] = *eventFromRow(row)
	}
	return events, nil
}

// MarkDelivered marks an event as successfully delivered.
func (s *EventStore) MarkDelivered(ctx context.Context, id int64) error {
	return s.q.UpdateEventDelivered(ctx, id)
}

// MarkRetry schedules an event for retry at the given time.
func (s *EventStore) MarkRetry(ctx context.Context, id int64, nextAttempt time.Time) error {
	return s.q.UpdateEventRetry(ctx, sqlcgen.UpdateEventRetryParams{
		ID:            id,
		NextAttemptAt: nextAttempt,
	})
}

// MarkDead marks an event as permanently failed.
func (s *EventStore) MarkDead(ctx context.Context, id int64) error {
	return s.q.UpdateEventDead(ctx, id)
}
