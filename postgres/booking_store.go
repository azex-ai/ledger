package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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
	if err := input.Validate(); err != nil {
		return nil, fmt.Errorf("postgres: create booking: %w", err)
	}

	// Check idempotency
	existing, err := s.q.GetBookingByIdempotencyKey(ctx, input.IdempotencyKey)
	if err == nil {
		return bookingFromRow(existing), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: create booking: check idempotency: %w", err)
	}

	// Load classification to get lifecycle
	class, err := s.q.GetClassificationByCode(ctx, input.ClassificationCode)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: create booking: classification %q: %w", input.ClassificationCode, core.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: create booking: classification %q: %w", input.ClassificationCode, err)
	}

	var lifecycle core.Lifecycle
	if len(class.Lifecycle) <= 2 {
		return nil, fmt.Errorf("postgres: create booking: classification %q has no lifecycle", input.ClassificationCode)
	}
	if err := json.Unmarshal(class.Lifecycle, &lifecycle); err != nil {
		return nil, fmt.Errorf("postgres: create booking: unmarshal lifecycle: %w", err)
	}
	if err := lifecycle.Validate(); err != nil {
		return nil, fmt.Errorf("postgres: create booking: invalid lifecycle: %w", err)
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
		existing, lookupErr := s.q.GetBookingByIdempotencyKey(ctx, input.IdempotencyKey)
		if lookupErr == nil {
			return bookingFromRow(existing), nil
		}
		if lookupErr != nil && !errors.Is(lookupErr, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: create booking: insert: %w (idempotency recheck: %v)", normalizeStoreError(err), lookupErr)
		}
		return nil, wrapStoreError("postgres: create booking", err)
	}
	return bookingFromRow(row), nil
}

// Transition advances a booking's status and records an event atomically.
func (s *BookingStore) Transition(ctx context.Context, input core.TransitionInput) (*core.Event, error) {
	if err := input.Validate(); err != nil {
		return nil, fmt.Errorf("postgres: transition: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: transition: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)

	// Lock booking
	op, err := qtx.GetBookingForUpdate(ctx, input.BookingID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: transition: booking %d: %w", input.BookingID, core.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: transition: get booking %d: %w", input.BookingID, err)
	}

	// Load classification for lifecycle validation
	class, err := qtx.GetClassification(ctx, op.ClassificationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: transition: classification %d: %w", op.ClassificationID, core.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: transition: get classification: %w", err)
	}

	var lifecycle core.Lifecycle
	if err := json.Unmarshal(class.Lifecycle, &lifecycle); err != nil {
		return nil, fmt.Errorf("postgres: transition: unmarshal lifecycle: %w", err)
	}
	if err := lifecycle.Validate(); err != nil {
		return nil, fmt.Errorf("postgres: transition: invalid lifecycle: %w", err)
	}

	fromStatus := core.Status(op.Status)
	if fromStatus == input.ToStatus {
		latestEvent, err := qtx.GetLatestEventForBooking(ctx, op.ID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: transition: latest event: %w", err)
		}
		if err == nil {
			reused, reuseErr := idempotentTransitionEvent(bookingFromRow(op), eventFromRow(latestEvent), input)
			if reuseErr != nil {
				return nil, reuseErr
			}
			if reused != nil {
				if err := tx.Commit(ctx); err != nil {
					return nil, fmt.Errorf("postgres: transition: commit idempotent: %w", err)
				}
				return reused, nil
			}
		}
	}
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

	// Update booking. Preserve the existing journal_id (may be NULL) — only the
	// PostJournal flow is allowed to attach a journal_id, and it does so via a
	// dedicated update path rather than through transitions.
	err = qtx.UpdateBookingTransition(ctx, sqlcgen.UpdateBookingTransitionParams{
		ID:            op.ID,
		Status:        string(input.ToStatus),
		ChannelRef:    channelRef,
		SettledAmount: decimalToNumeric(settledAmount),
		JournalID:     op.JournalID,
		Metadata:      anyMetadataToJSON(metadata),
	})
	if err != nil {
		return nil, wrapStoreError("postgres: transition: update", err)
	}

	// Insert event (atomic with transition). journal_id is NULL for now —
	// it gets backfilled when/if a journal is posted for this transition.
	eventRow, err := qtx.InsertEvent(ctx, sqlcgen.InsertEventParams{
		ClassificationCode: class.Code,
		BookingID:          op.ID,
		AccountHolder:      op.AccountHolder,
		CurrencyID:         op.CurrencyID,
		FromStatus:         op.Status,
		ToStatus:           string(input.ToStatus),
		Amount:             op.Amount,
		SettledAmount:      decimalToNumeric(settledAmount),
		JournalID:          pgtype.Int8{Valid: false},
		Metadata:           anyMetadataToJSON(metadata),
		OccurredAt:         time.Now(),
	})
	if err != nil {
		return nil, wrapStoreError("postgres: transition: insert event", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: transition: commit: %w", err)
	}

	return eventFromRow(eventRow), nil
}

func idempotentTransitionEvent(current *core.Booking, latest *core.Event, input core.TransitionInput) (*core.Event, error) {
	if current == nil || latest == nil {
		return nil, nil
	}
	if current.Status != input.ToStatus || latest.ToStatus != input.ToStatus {
		return nil, nil
	}
	if input.ChannelRef != "" && input.ChannelRef != current.ChannelRef {
		return nil, fmt.Errorf("postgres: transition: channel_ref mismatch on repeated callback: %w", core.ErrConflict)
	}
	if !input.Amount.IsZero() && !input.Amount.Equal(current.SettledAmount) {
		return nil, fmt.Errorf("postgres: transition: settled_amount mismatch on repeated callback: %w", core.ErrConflict)
	}
	return latest, nil
}

// GetBooking returns a booking by ID.
func (s *BookingStore) GetBooking(ctx context.Context, id int64) (*core.Booking, error) {
	row, err := s.q.GetBooking(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: get booking %d: %w", id, core.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: get booking %d: %w", id, err)
	}
	return bookingFromRow(row), nil
}

// ListExpiredBookings returns bookings past their expiration time that can transition to expired.
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
