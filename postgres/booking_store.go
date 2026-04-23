package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

var (
	_ core.Booker        = (*BookingStore)(nil)
	_ core.BookingReader = (*BookingStore)(nil)
)

// BookingStore implements core.Booker and core.BookingReader using PostgreSQL.
type BookingStore struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

// NewBookingStore creates a new BookingStore.
func NewBookingStore(pool *pgxpool.Pool, q *sqlcgen.Queries) *BookingStore {
	return &BookingStore{pool: pool, q: q}
}

// CreateBooking creates a new booking with initial status from the classification lifecycle.
// Idempotent: returns existing booking if idempotency_key already exists.
func (s *BookingStore) CreateBooking(ctx context.Context, input core.CreateBookingInput) (*core.Booking, error) {
	// Check idempotency
	existing, err := s.q.GetBookingByIdempotencyKey(ctx, input.IdempotencyKey)
	if err == nil {
		return bookingFromRow(existing), nil
	}

	// Load classification to get lifecycle
	class, err := s.q.GetClassificationByCode(ctx, input.ClassificationCode)
	if err != nil {
		return nil, fmt.Errorf("postgres: create booking: classification %q: %w", input.ClassificationCode, err)
	}

	var lifecycle core.Lifecycle
	if len(class.Lifecycle) <= 2 {
		return nil, fmt.Errorf("postgres: create booking: classification %q has no lifecycle", input.ClassificationCode)
	}
	if err := json.Unmarshal(class.Lifecycle, &lifecycle); err != nil {
		return nil, fmt.Errorf("postgres: create booking: unmarshal lifecycle: %w", err)
	}

	row, err := s.q.InsertBooking(ctx, sqlcgen.InsertBookingParams{
		ClassificationID: class.ID,
		AccountHolder:    input.AccountHolder,
		CurrencyID:       input.CurrencyID,
		Amount:           decimalToNumeric(input.Amount),
		Status:           string(lifecycle.Initial),
		ChannelName:      input.ChannelName,
		IdempotencyKey:   input.IdempotencyKey,
		Metadata:         anyMetadataToJSON(input.Metadata),
		ExpiresAt:        input.ExpiresAt,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: create booking: %w", err)
	}
	return bookingFromRow(row), nil
}

// Transition advances a booking's status and records an event atomically.
func (s *BookingStore) Transition(ctx context.Context, input core.TransitionInput) (*core.Event, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: transition: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)

	// Lock booking
	op, err := qtx.GetBookingForUpdate(ctx, input.BookingID)
	if err != nil {
		return nil, fmt.Errorf("postgres: transition: get booking %d: %w", input.BookingID, err)
	}

	// Load classification for lifecycle validation
	class, err := qtx.GetClassification(ctx, op.ClassificationID)
	if err != nil {
		return nil, fmt.Errorf("postgres: transition: get classification: %w", err)
	}

	var lifecycle core.Lifecycle
	if err := json.Unmarshal(class.Lifecycle, &lifecycle); err != nil {
		return nil, fmt.Errorf("postgres: transition: unmarshal lifecycle: %w", err)
	}

	fromStatus := core.Status(op.Status)
	if !lifecycle.CanTransition(fromStatus, input.ToStatus) {
		return nil, fmt.Errorf("postgres: transition: %w: %s -> %s", core.ErrInvalidTransition, op.Status, input.ToStatus)
	}

	// Merge metadata
	metadata := jsonToAnyMetadata(op.Metadata)
	if metadata == nil {
		metadata = make(map[string]any)
	}
	for k, v := range input.Metadata {
		metadata[k] = v
	}

	// Determine settled_amount: use input if non-zero, else keep existing
	settledAmount := mustNumericToDecimal(op.SettledAmount)
	if !input.Amount.IsZero() {
		settledAmount = input.Amount
	}

	// Determine channel_ref
	channelRef := op.ChannelRef
	if input.ChannelRef != "" {
		channelRef = input.ChannelRef
	}

	// Update booking
	err = qtx.UpdateBookingTransition(ctx, sqlcgen.UpdateBookingTransitionParams{
		ID:            op.ID,
		Status:        string(input.ToStatus),
		ChannelRef:    channelRef,
		SettledAmount: decimalToNumeric(settledAmount),
		JournalID:     op.JournalID,
		Metadata:      anyMetadataToJSON(metadata),
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: transition: update: %w", err)
	}

	// Insert event (atomic with transition)
	eventRow, err := qtx.InsertEvent(ctx, sqlcgen.InsertEventParams{
		ClassificationCode: class.Code,
		BookingID:          op.ID,
		AccountHolder:      op.AccountHolder,
		CurrencyID:         op.CurrencyID,
		FromStatus:         op.Status,
		ToStatus:           string(input.ToStatus),
		Amount:             op.Amount,
		SettledAmount:      decimalToNumeric(settledAmount),
		JournalID:          0,
		Metadata:           anyMetadataToJSON(metadata),
		OccurredAt:         time.Now(),
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: transition: insert event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: transition: commit: %w", err)
	}

	return eventFromRow(eventRow), nil
}

// GetBooking returns a booking by ID.
func (s *BookingStore) GetBooking(ctx context.Context, id int64) (*core.Booking, error) {
	row, err := s.q.GetBooking(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("postgres: get booking %d: %w", id, err)
	}
	return bookingFromRow(row), nil
}

// ListExpiredBookings returns non-terminal bookings past their expiration time.
func (s *BookingStore) ListExpiredBookings(ctx context.Context, limit int) ([]core.Booking, error) {
	rows, err := s.q.ListExpiredBookings(ctx, int32(limit))
	if err != nil {
		return nil, fmt.Errorf("postgres: list expired bookings: %w", err)
	}
	ops := make([]core.Booking, len(rows))
	for i, row := range rows {
		ops[i] = *bookingFromRow(row)
	}
	return ops, nil
}

// ListBookings returns bookings matching the filter.
func (s *BookingStore) ListBookings(ctx context.Context, filter core.BookingFilter) ([]core.Booking, error) {
	rows, err := s.q.ListBookingsByFilter(ctx, sqlcgen.ListBookingsByFilterParams{
		AccountHolder:    filter.AccountHolder,
		ClassificationID: filter.ClassificationID,
		Status:           filter.Status,
		ID:               filter.Cursor,
		Limit:            int32(filter.Limit),
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: list bookings: %w", err)
	}
	ops := make([]core.Booking, len(rows))
	for i, row := range rows {
		ops[i] = *bookingFromRow(row)
	}
	return ops, nil
}
