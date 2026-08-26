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
	dims *dimCache
}

// NewBookingStore creates a new BookingStore backed by a connection pool. The
// internal sqlc Queries instance is built from pool so library consumers don't
// need to import the generated sqlcgen package.
func NewBookingStore(pool *pgxpool.Pool) *BookingStore {
	return &BookingStore{pool: pool, db: pool, q: sqlcgen.New(pool), dims: dimCacheFor(pool)}
}

// WithDB returns a clone of the BookingStore bound to an existing transaction.
func (s *BookingStore) WithDB(db DBTX) *BookingStore {
	return &BookingStore{
		pool: nil, // tx mode
		db:   db,
		q:    sqlcgen.New(db),
		dims: s.dims,
	}
}

// CreateBooking creates a new booking with initial status from the classification lifecycle.
// Idempotent: same key + same payload returns the existing booking; divergent
// payload returns ErrConflict.
func (s *BookingStore) CreateBooking(ctx context.Context, input core.CreateBookingInput) (*core.Booking, error) {
	ctx, span := ledgerotel.StartSpan(ctx, "ledger.booking.create_booking",
		attribute.String("classification_code", input.ClassificationCode),
		attribute.Int64("account_holder", input.AccountHolder),
		attribute.String("currency_uid", input.CurrencyUID),
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
		return s.ensureBookingMatchesInput(ctx, s.q, existing, input)
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

	cur, err := s.dims.currencyByUIDOrErr(ctx, s.q, input.CurrencyUID)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return nil, fmt.Errorf("postgres: create booking: %w", err)
	}
	// Business precision (I-16): booking amounts feed reporting and later
	// settlement math, so they must respect the currency exponent just like
	// journal entries and reservations do.
	if err := checkAmountPrecision(input.Amount, cur); err != nil {
		ledgerotel.RecordError(span, err)
		return nil, fmt.Errorf("postgres: create booking: %w", err)
	}
	row, err := s.q.InsertBooking(ctx, sqlcgen.InsertBookingParams{
		ClassificationID: class.ID,
		AccountHolder:    input.AccountHolder,
		CurrencyID:       cur.ID,
		Amount:           decimalToNumeric(input.Amount),
		Status:           string(lifecycle.Initial),
		ChannelName:      input.ChannelName,
		IdempotencyKey:   input.IdempotencyKey,
		Metadata:         stringMetadataToJSON(input.Metadata),
		ExpiresAt:        input.ExpiresAt,
		Uid:              newUID(),
	})
	if err != nil {
		existing, lookupErr := s.q.GetBookingByIdempotencyKey(ctx, input.IdempotencyKey)
		if lookupErr == nil {
			return s.ensureBookingMatchesInput(ctx, s.q, existing, input)
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
	return bookingFromRow(ctx, s.dims, s.q, row)
}

// Transition advances a booking's status and records an event atomically.
//
// In pool mode a new transaction is started and committed here.
// In tx mode (bound via withDB) the transition is written into the caller's
// transaction; commit/rollback is the caller's responsibility.
func (s *BookingStore) Transition(ctx context.Context, input core.TransitionInput) (*core.Event, error) {
	ctx, span := ledgerotel.StartSpan(ctx, "ledger.booking.transition",
		attribute.String("booking_uid", input.BookingUID),
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
	pgUID, err := uidToPG(input.BookingUID)
	if err != nil {
		return nil, err
	}
	op, err := qtx.GetBookingForUpdateByUID(ctx, pgUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: transition: booking %q: %w", input.BookingUID, core.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: transition: get booking %q: %w", input.BookingUID, err)
	}

	// Opt-in durable idempotency (I-3) -- see TransitionInput.IdempotencyKey's
	// doc comment for why this is opt-in rather than mandatory. Checked
	// BEFORE the state-comparison-based path below, and independent of the
	// booking's CURRENT status, because it covers a case that path cannot: a
	// retry arriving after a LATER, legitimate transition has already moved
	// the booking past ToStatus.
	if input.IdempotencyKey != "" {
		if receipt, err := qtx.GetBookingTransitionReceiptByIdempotencyKey(ctx, input.IdempotencyKey); err == nil {
			return s.ensureBookingTransitionReceiptMatches(ctx, qtx, receipt, op.ID, input)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: transition: check idempotency: %w", err)
		}
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

	// Business precision (I-16) on the settled amount carried by this
	// transition (zero = "no amount for this transition", skipped).
	if !input.Amount.IsZero() {
		cur, err := s.dims.currencyByIDOrErr(ctx, qtx, op.CurrencyID)
		if err != nil {
			return nil, fmt.Errorf("postgres: transition: %w", err)
		}
		if err := checkAmountPrecision(input.Amount, cur); err != nil {
			return nil, fmt.Errorf("postgres: transition: %w", err)
		}
	}

	fromStatus := core.Status(op.Status)
	if fromStatus == input.ToStatus {
		latestEvent, err := qtx.GetLatestEventForBooking(ctx, op.ID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: transition: latest event: %w", err)
		}
		if err == nil {
			currentBooking, mapErr := bookingFromRow(ctx, s.dims, qtx, op)
			if mapErr != nil {
				return nil, mapErr
			}
			latestCore, mapErr := eventFromRow(ctx, s.dims, qtx, latestEvent)
			if mapErr != nil {
				return nil, mapErr
			}
			reused, reuseErr := idempotentTransitionEvent(currentBooking, latestCore, input)
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
	metadata := jsonToStringMetadata(op.Metadata)
	if metadata == nil {
		metadata = make(map[string]string)
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
		Metadata:      stringMetadataToJSON(metadata),
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
		Metadata:           stringMetadataToJSON(metadata),
		OccurredAt:         time.Now(),
		ActorID:            input.ActorID,
		Source:             input.Source,
		Uid:                newUID(),
	})
	if err != nil {
		return nil, wrapStoreError("postgres: transition: insert event", err)
	}

	if input.IdempotencyKey != "" {
		if err := s.recordBookingTransitionReceipt(ctx, qtx, op.ID, eventRow.ID, input); err != nil {
			return nil, err
		}
	}

	return eventFromRow(ctx, s.dims, qtx, eventRow)
}

// ensureBookingTransitionReceiptMatches is Transition's opt-in idempotency
// check (I-3): a replay with the same booking, to_status, channel_ref and
// amount returns the event the original call produced; any divergence is
// core.ErrConflict.
func (s *BookingStore) ensureBookingTransitionReceiptMatches(ctx context.Context, qtx *sqlcgen.Queries, receipt sqlcgen.BookingTransitionReceipt, bookingID int64, input core.TransitionInput) (*core.Event, error) {
	if receipt.BookingID != bookingID {
		return nil, fmt.Errorf("postgres: transition: idempotency key %q already used for a different booking: %w", input.IdempotencyKey, core.ErrConflict)
	}
	if receipt.ToStatus != string(input.ToStatus) {
		return nil, fmt.Errorf("postgres: transition: idempotency key %q already used for a different to_status (%s): %w", input.IdempotencyKey, receipt.ToStatus, core.ErrConflict)
	}
	if input.ChannelRef != "" && receipt.ChannelRef != input.ChannelRef {
		return nil, fmt.Errorf("postgres: transition: idempotency key %q payload mismatch on channel_ref: %w", input.IdempotencyKey, core.ErrConflict)
	}
	receiptAmount, err := numericToDecimal(receipt.Amount)
	if err != nil {
		return nil, fmt.Errorf("postgres: transition: convert receipt amount: %w", err)
	}
	if !input.Amount.IsZero() && !receiptAmount.Equal(input.Amount) {
		return nil, fmt.Errorf("postgres: transition: idempotency key %q payload mismatch on amount (recorded %s, got %s): %w", input.IdempotencyKey, receiptAmount, input.Amount, core.ErrConflict)
	}

	eventRow, err := qtx.GetEvent(ctx, receipt.EventID)
	if err != nil {
		return nil, fmt.Errorf("postgres: transition: load original event: %w", err)
	}
	return eventFromRow(ctx, s.dims, qtx, eventRow)
}

// recordBookingTransitionReceipt inserts the durable idempotency record for
// a just-applied Transition call. A 23505 on the unique idempotency_key
// index (caught via ON CONFLICT DO NOTHING returning no row, surfaced here
// as pgx.ErrNoRows) means a concurrent call on a DIFFERENT booking raced
// this one to the same key after the caller's own pre-check -- the row lock
// on THIS booking already serializes same-booking racers -- and is reported
// as ErrConflict, matching reservation_operation_receipts' race handling.
func (s *BookingStore) recordBookingTransitionReceipt(ctx context.Context, qtx *sqlcgen.Queries, bookingID, eventID int64, input core.TransitionInput) error {
	if _, err := qtx.InsertBookingTransitionReceipt(ctx, sqlcgen.InsertBookingTransitionReceiptParams{
		BookingID:      bookingID,
		IdempotencyKey: input.IdempotencyKey,
		ToStatus:       string(input.ToStatus),
		ChannelRef:     input.ChannelRef,
		Amount:         decimalToNumeric(input.Amount),
		EventID:        eventID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("postgres: transition: idempotency key %q raced a concurrent application: %w", input.IdempotencyKey, core.ErrConflict)
		}
		return wrapStoreError("postgres: transition: record receipt", err)
	}
	return nil
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
func (s *BookingStore) GetBooking(ctx context.Context, uid string) (*core.Booking, error) {
	pgUID, err := uidToPG(uid)
	if err != nil {
		return nil, err
	}
	row, err := s.q.GetBookingByUID(ctx, pgUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: get booking %q: %w", uid, core.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: get booking %q: %w", uid, err)
	}
	return bookingFromRow(ctx, s.dims, s.q, row)
}

// ListExpiredBookings returns bookings past their expiration time that can transition to expired.
func (s *BookingStore) ListExpiredBookings(ctx context.Context, limit int) ([]core.Booking, error) {
	rows, err := s.q.ListExpiredBookings(ctx, int32(limit))
	if err != nil {
		return nil, fmt.Errorf("postgres: list expired bookings: %w", err)
	}
	ops := make([]core.Booking, len(rows))
	for i, row := range rows {
		b, err := bookingFromRow(ctx, s.dims, s.q, row)
		if err != nil {
			return nil, err
		}
		ops[i] = *b
	}
	return ops, nil
}

// ListBookings returns bookings matching the filter plus the opaque cursor
// for the next page ("" when exhausted).
func (s *BookingStore) ListBookings(ctx context.Context, filter core.BookingFilter) ([]core.Booking, string, error) {
	classificationID := int64(0)
	if filter.ClassificationUID != "" {
		d, err := s.dims.classByUIDOrErr(ctx, s.q, filter.ClassificationUID)
		if err != nil {
			return nil, "", err
		}
		classificationID = d.ID
	}
	cursorID, err := decodeCursorString(filter.Cursor)
	if err != nil {
		cursorID = 0
	}
	rows, err := s.q.ListBookingsByFilter(ctx, sqlcgen.ListBookingsByFilterParams{
		AccountHolder:    filter.AccountHolder,
		ClassificationID: classificationID,
		Status:           filter.Status,
		ID:               cursorID,
		Limit:            int32(filter.Limit),
	})
	if err != nil {
		return nil, "", fmt.Errorf("postgres: list bookings: %w", err)
	}
	ops := make([]core.Booking, len(rows))
	for i, row := range rows {
		b, err := bookingFromRow(ctx, s.dims, s.q, row)
		if err != nil {
			return nil, "", err
		}
		ops[i] = *b
	}
	nextCursor := ""
	if filter.Limit > 0 && len(rows) == filter.Limit {
		nextCursor = encodeCursorString(rows[len(rows)-1].ID)
	}
	return ops, nextCursor, nil
}
