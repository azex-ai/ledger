package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"

	"github.com/azex-ai/ledger/core"
	ledgerotel "github.com/azex-ai/ledger/pkg/otel"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

var (
	_ core.Booker        = (*BookingStore)(nil)
	_ core.BookingReader = (*BookingStore)(nil)
)

// BookingStore implements core.Booker and core.BookingReader using PostgreSQL.
//
// In pool mode (constructed via NewBookingStore), each write operation starts
// its own transaction. In tx mode (bound via withDB), write operations
// participate in the caller's transaction — commit/rollback is the caller's
// responsibility.
type BookingStore struct {
	// pool is non-nil only in pool mode. Nil signals tx mode.
	pool *pgxpool.Pool
	db   DBTX
	q    *sqlcgen.Queries
}

// NewBookingStore creates a new BookingStore backed by a connection pool. The
// internal sqlc Queries instance is built from pool so library consumers don't
// need to import the generated sqlcgen package.
func NewBookingStore(pool *pgxpool.Pool) *BookingStore {
	return &BookingStore{pool: pool, db: pool, q: sqlcgen.New(pool)}
}

// WithDB returns a clone of the BookingStore bound to an existing transaction.
func (s *BookingStore) WithDB(db DBTX) *BookingStore {
	return &BookingStore{
		pool: nil, // tx mode
		db:   db,
		q:    sqlcgen.New(db),
	}
}

// CreateBooking creates a new booking with initial status from the classification lifecycle.
// Idempotent: same key + same payload returns the existing booking; divergent
// payload returns ErrConflict.
func (s *BookingStore) CreateBooking(ctx context.Context, input core.CreateBookingInput) (*core.Booking, error) {
	ctx, span := ledgerotel.StartSpan(ctx, "ledger.booking.create_booking",
		attribute.String("classification_code", input.ClassificationCode),
		attribute.Int64("account_holder", input.AccountHolder),
		attribute.Int64("currency_id", input.CurrencyID),
		attribute.String("idempotency_key", input.IdempotencyKey),
		attribute.String("amount", input.Amount.String()),
	)
	defer span.End()

	if err := input.Validate(); err != nil {
		ledgerotel.RecordError(span, err)
		return nil, fmt.Errorf("postgres: create booking: %w", err)
	}

	// Check idempotency
	existing, err := s.q.GetBookingByIdempotencyKey(ctx, input.IdempotencyKey)
	if err == nil {
		return ensureBookingMatchesInput(ctx, s.q, existing, input)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		retErr := fmt.Errorf("postgres: create booking: check idempotency: %w", err)
		ledgerotel.RecordError(span, retErr)
		return nil, retErr
	}

	// Load classification to get lifecycle
	class, err := s.q.GetClassificationByCode(ctx, input.ClassificationCode)
	if err != nil {
		var retErr error
		if errors.Is(err, pgx.ErrNoRows) {
			retErr = fmt.Errorf("postgres: create booking: classification %q: %w", input.ClassificationCode, core.ErrNotFound)
		} else {
			retErr = fmt.Errorf("postgres: create booking: classification %q: %w", input.ClassificationCode, err)
		}
		ledgerotel.RecordError(span, retErr)
		return nil, retErr
	}

	var lifecycle core.Lifecycle
	if len(class.Lifecycle) <= 2 {
		retErr := fmt.Errorf("postgres: create booking: classification %q has no lifecycle", input.ClassificationCode)
		ledgerotel.RecordError(span, retErr)
		return nil, retErr
	}
	if err := json.Unmarshal(class.Lifecycle, &lifecycle); err != nil {
		retErr := fmt.Errorf("postgres: create booking: unmarshal lifecycle: %w", err)
		ledgerotel.RecordError(span, retErr)
		return nil, retErr
	}
	if err := lifecycle.Validate(); err != nil {
		retErr := fmt.Errorf("postgres: create booking: invalid lifecycle: %w", err)
		ledgerotel.RecordError(span, retErr)
		return nil, retErr
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
			return ensureBookingMatchesInput(ctx, s.q, existing, input)
		}
		var retErr error
		if !errors.Is(lookupErr, pgx.ErrNoRows) {
			retErr = fmt.Errorf("postgres: create booking: insert: %w (idempotency recheck: %v)", normalizeStoreError(err), lookupErr)
		} else {
			retErr = wrapStoreError("postgres: create booking", err)
		}
		ledgerotel.RecordError(span, retErr)
		return nil, retErr
	}
	return bookingFromRow(row), nil
}

// Transition advances a booking's status and records an event atomically.
//
// In pool mode a new transaction is started and committed here.
// In tx mode (bound via withDB) the transition is written into the caller's
// transaction; commit/rollback is the caller's responsibility.
func (s *BookingStore) Transition(ctx context.Context, input core.TransitionInput) (*core.Event, error) {
	ctx, span := ledgerotel.StartSpan(ctx, "ledger.booking.transition",
		attribute.Int64("booking_id", input.BookingID),
		attribute.String("to_status", string(input.ToStatus)),
		attribute.Int64("actor_id", input.ActorID),
		attribute.String("source", input.Source),
	)
	defer span.End()

	if err := input.Validate(); err != nil {
		retErr := fmt.Errorf("postgres: transition: %w", err)
		ledgerotel.RecordError(span, retErr)
		return nil, retErr
	}

	if s.pool == nil {
		// Tx mode: use the caller's transaction directly.
		evt, err := s.transitionWithQueries(ctx, s.q, input)
		ledgerotel.RecordError(span, err)
		return evt, err
	}

	// Pool mode: own the transaction lifecycle.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return nil, fmt.Errorf("postgres: transition: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	evt, err := s.transitionWithQueries(ctx, s.q.WithTx(tx), input)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		ledgerotel.RecordError(span, err)
		return nil, fmt.Errorf("postgres: transition: commit: %w", err)
	}

	return evt, nil
}

func (s *BookingStore) transitionWithQueries(ctx context.Context, qtx *sqlcgen.Queries, input core.TransitionInput) (*core.Event, error) {
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
	maps.Copy(metadata, input.Metadata)

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
		ActorID:            input.ActorID,
		Source:             input.Source,
	})
	if err != nil {
		return nil, wrapStoreError("postgres: transition: insert event", err)
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
