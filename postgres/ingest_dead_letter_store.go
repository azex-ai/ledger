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

// IngestDeadLetterStore persists deposit sightings that could not be
// idempotently reconciled by service.Onchain.IngestDeposit -- a
// core.ErrConflict on CreateBooking is a normalization bug signal (design
// doc §6), not a transient error, so these must never be silently dropped or
// endlessly retried. Implements service.DeadLetterRecorder.
type IngestDeadLetterStore struct {
	pool *pgxpool.Pool
	db   DBTX
	q    *sqlcgen.Queries
}

// NewIngestDeadLetterStore creates an IngestDeadLetterStore backed by a connection pool.
func NewIngestDeadLetterStore(pool *pgxpool.Pool) *IngestDeadLetterStore {
	return &IngestDeadLetterStore{pool: pool, db: pool, q: sqlcgen.New(pool)}
}

// WithDB returns a clone of the IngestDeadLetterStore bound to an existing transaction.
func (s *IngestDeadLetterStore) WithDB(db DBTX) *IngestDeadLetterStore {
	return &IngestDeadLetterStore{pool: nil, db: db, q: sqlcgen.New(db)}
}

// RecordDeadLetter persists sighting as a dead letter keyed by
// idempotencyKey. Idempotent: a repeated conflict on the same sighting
// (e.g. the watcher retrying every scan) is a no-op, not a new row.
func (s *IngestDeadLetterStore) RecordDeadLetter(ctx context.Context, sighting core.DepositSighting, idempotencyKey, reason string) error {
	payload, err := json.Marshal(sighting)
	if err != nil {
		return fmt.Errorf("postgres: record dead letter: marshal payload: %w", err)
	}
	_, err = s.q.InsertIngestDeadLetter(ctx, sqlcgen.InsertIngestDeadLetterParams{
		Uid:            newUID(),
		ChainID:        sighting.ChainID,
		TxHash:         sighting.TxHash,
		TxlogSeq:       sighting.TxLogSeq,
		IdempotencyKey: idempotencyKey,
		Reason:         reason,
		Payload:        payload,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// ON CONFLICT DO NOTHING: already recorded for this key.
			return nil
		}
		return fmt.Errorf("postgres: record dead letter: %w", err)
	}
	return nil
}

// ListDeadLetters returns a page of dead letters, newest first, plus the
// opaque cursor for the next (older) page ("" when exhausted) -- the on-call
// triage surface behind `ledger-cli dead-letters list` and
// GET /deposits/dead-letters (docs/RUNBOOK.md §18).
//
// Paginated rather than "the most recent N": the population is small when
// everything is fine and unbounded when a normalization bug is loose, which
// is exactly when an operator must be able to walk all of it.
func (s *IngestDeadLetterStore) ListDeadLetters(ctx context.Context, cursor string, limit int32) ([]core.IngestDeadLetter, string, error) {
	cursorID, err := decodeCursorString(cursor)
	if err != nil {
		// Same rule as ListBookings/ListEvents: a malformed cursor surfaces
		// instead of silently restarting pagination at page one.
		return nil, "", fmt.Errorf("postgres: list dead letters: %w", err)
	}
	rows, err := s.q.ListIngestDeadLetters(ctx, sqlcgen.ListIngestDeadLettersParams{
		CursorID:  cursorID,
		PageLimit: limit,
	})
	if err != nil {
		return nil, "", fmt.Errorf("postgres: list dead letters: %w", err)
	}
	out := make([]core.IngestDeadLetter, len(rows))
	for i, row := range rows {
		dl, err := deadLetterFromRow(row.Uid, row.ChainID, row.TxHash, row.TxlogSeq, row.IdempotencyKey, row.Reason, row.CreatedAt, row.Booked, row.Payload)
		if err != nil {
			return nil, "", fmt.Errorf("postgres: list dead letters: %w", err)
		}
		out[i] = dl
	}
	nextCursor := ""
	if limit > 0 && int32(len(rows)) == limit {
		nextCursor = encodeCursorString(rows[len(rows)-1].ID)
	}
	return out, nextCursor, nil
}

// GetDeadLetter returns one dead letter by uid, including the deposit
// sighting recorded on it -- everything service.Onchain.ReplayDeadLetter
// needs to re-drive it through the real ingestion path.
func (s *IngestDeadLetterStore) GetDeadLetter(ctx context.Context, uid string) (core.IngestDeadLetter, error) {
	id, err := uidToPG(uid)
	if err != nil {
		return core.IngestDeadLetter{}, fmt.Errorf("postgres: get dead letter: %w", err)
	}
	row, err := s.q.GetIngestDeadLetter(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return core.IngestDeadLetter{}, fmt.Errorf("postgres: get dead letter %s: %w", uid, core.ErrNotFound)
		}
		return core.IngestDeadLetter{}, fmt.Errorf("postgres: get dead letter: %w", err)
	}
	dl, err := deadLetterFromRow(row.Uid, row.ChainID, row.TxHash, row.TxlogSeq, row.IdempotencyKey, row.Reason, row.CreatedAt, row.Booked, row.Payload)
	if err != nil {
		return core.IngestDeadLetter{}, fmt.Errorf("postgres: get dead letter: %w", err)
	}
	return dl, nil
}

// CountUnbookedDeadLetters returns how many dead letters still have no
// booking for their idempotency key, and when the oldest of those was
// recorded (the zero time when there are none). This is the queue depth
// core.Metrics.DeadLetterBacklog reports: "no booking exists" is what makes
// it self-clearing, so nothing has to remember to resolve a row whose
// deposit was credited in the end.
func (s *IngestDeadLetterStore) CountUnbookedDeadLetters(ctx context.Context) (int64, time.Time, error) {
	row, err := s.q.CountUnbookedIngestDeadLetters(ctx)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("postgres: count unbooked dead letters: %w", err)
	}
	oldest := row.OldestCreatedAt
	if oldest.Unix() <= 0 {
		// 'epoch' is the query's no-NULL stand-in for "the queue is empty"
		// (this schema's convention); hand back Go's zero time so callers
		// test it with IsZero rather than against a magic timestamp.
		oldest = time.Time{}
	}
	return row.Unbooked, oldest, nil
}

// deadLetterFromRow maps one dead-letter row (in either query's shape) to the
// core model, decoding the stored sighting payload.
//
// A payload that does not decode is an error, never a zero-valued sighting:
// the payload is the whole reason the row is replayable, and a replay driven
// from a silently-empty sighting would be refused by DepositSighting.Validate
// at best and book the wrong thing at worst.
func deadLetterFromRow(uid pgtype.UUID, chainID int64, txHash string, txLogSeq int32, idempotencyKey, reason string, createdAt time.Time, booked bool, payload []byte) (core.IngestDeadLetter, error) {
	dl := core.IngestDeadLetter{
		UID:            pgToUID(uid),
		ChainID:        chainID,
		TxHash:         txHash,
		TxLogSeq:       txLogSeq,
		IdempotencyKey: idempotencyKey,
		Reason:         reason,
		CreatedAt:      createdAt,
		Booked:         booked,
	}
	if err := json.Unmarshal(payload, &dl.Sighting); err != nil {
		return core.IngestDeadLetter{}, fmt.Errorf("decode dead letter %s payload: %w", dl.UID, err)
	}
	return dl, nil
}
