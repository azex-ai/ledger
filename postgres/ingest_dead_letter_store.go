package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
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

// ListDeadLetters returns the most recent dead letters, newest first, for
// on-call triage (RUNBOOK).
func (s *IngestDeadLetterStore) ListDeadLetters(ctx context.Context, limit int32) ([]core.IngestDeadLetter, error) {
	rows, err := s.q.ListIngestDeadLetters(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list dead letters: %w", err)
	}
	out := make([]core.IngestDeadLetter, len(rows))
	for i, row := range rows {
		out[i] = core.IngestDeadLetter{
			UID:            pgToUID(row.Uid),
			ChainID:        row.ChainID,
			TxHash:         row.TxHash,
			TxLogSeq:       row.TxlogSeq,
			IdempotencyKey: row.IdempotencyKey,
			Reason:         row.Reason,
			CreatedAt:      row.CreatedAt,
		}
	}
	return out, nil
}
