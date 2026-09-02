package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

// DepositReorgStore persists deposit chain anomalies -- a confirmed
// deposit whose transaction left the canonical chain (deep reorg), and a
// failed deposit whose transaction came back (see core.DepositReorg for why
// a log line and a counter were not enough). Implements
// service.ReorgRecorder.
type DepositReorgStore struct {
	pool *pgxpool.Pool
	db   DBTX
	q    *sqlcgen.Queries
}

// NewDepositReorgStore creates a DepositReorgStore backed by a connection pool.
func NewDepositReorgStore(pool *pgxpool.Pool) *DepositReorgStore {
	return &DepositReorgStore{pool: pool, db: pool, q: sqlcgen.New(pool)}
}

// WithDB returns a clone of the DepositReorgStore bound to an existing transaction.
func (s *DepositReorgStore) WithDB(db DBTX) *DepositReorgStore {
	return &DepositReorgStore{pool: nil, db: db, q: sqlcgen.New(db)}
}

// RecordReorg opens an anomaly row for (bookingUID, kind), or bumps an
// existing row's last_seen_at. Idempotent per (booking, kind): re-detecting
// the same anomaly on every recheck tick must not append a new row, but must
// still record that it is STILL observable.
func (s *DepositReorgStore) RecordReorg(ctx context.Context, kind, bookingUID string, chainID int64, txHash, journalUID string) error {
	if kind != core.ReorgKindDeepReorg && kind != core.ReorgKindShallowReorgFailed {
		return fmt.Errorf("postgres: record reorg: unknown kind %q: %w", kind, core.ErrInvalidInput)
	}
	bookingID, err := uidToPG(bookingUID)
	if err != nil {
		return fmt.Errorf("postgres: record reorg: %w", err)
	}
	if _, err := s.q.RecordDepositReorg(ctx, sqlcgen.RecordDepositReorgParams{
		Uid:        newUID(),
		Kind:       kind,
		BookingUid: bookingID,
		ChainID:    chainID,
		TxHash:     txHash,
		JournalUid: journalUID,
	}); err != nil {
		return fmt.Errorf("postgres: record reorg: %w", err)
	}
	return nil
}

// HasOpenReorg reports whether (bookingUID, kind) has an unresolved anomaly
// row. service.Onchain uses it to keep rechecking a booking whose anomaly is
// still open even after it falls outside the recheck window -- the window is
// a cost bound on FINDING anomalies, not a licence to stop reporting one
// already found.
func (s *DepositReorgStore) HasOpenReorg(ctx context.Context, kind, bookingUID string) (bool, error) {
	bookingID, err := uidToPG(bookingUID)
	if err != nil {
		// A malformed uid cannot name a row; uidToPG already maps that to
		// ErrNotFound. "No open anomaly" is the truthful answer, and the
		// caller's own metadata parsing would have rejected it first.
		if errors.Is(err, core.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("postgres: has open reorg: %w", err)
	}
	_, err = s.q.GetOpenDepositReorg(ctx, sqlcgen.GetOpenDepositReorgParams{
		BookingUid: bookingID,
		Kind:       kind,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("postgres: has open reorg: %w", err)
	}
	return true, nil
}

// ListOpenReorgs returns the oldest still-unresolved anomalies, for on-call
// triage (RUNBOOK §12).
func (s *DepositReorgStore) ListOpenReorgs(ctx context.Context, limit int32) ([]core.DepositReorg, error) {
	rows, err := s.q.ListOpenDepositReorgs(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list open reorgs: %w", err)
	}
	out := make([]core.DepositReorg, len(rows))
	for i, row := range rows {
		out[i] = core.DepositReorg{
			UID:        pgToUID(row.Uid),
			Kind:       row.Kind,
			BookingUID: pgToUID(row.BookingUid),
			ChainID:    row.ChainID,
			TxHash:     row.TxHash,
			JournalUID: row.JournalUid,
			DetectedAt: row.DetectedAt,
			LastSeenAt: row.LastSeenAt,
			ResolvedAt: row.ResolvedAt,
			Resolution: row.Resolution,
		}
	}
	return out, nil
}

// ResolveReorg closes out (bookingUID, kind) with an operator's note.
// Returns core.ErrNotFound when there was no open row to close, so a caller
// cannot mistake "already resolved / never existed" for a successful
// close-out.
func (s *DepositReorgStore) ResolveReorg(ctx context.Context, kind, bookingUID, resolution string) error {
	bookingID, err := uidToPG(bookingUID)
	if err != nil {
		return fmt.Errorf("postgres: resolve reorg: %w", err)
	}
	affected, err := s.q.ResolveDepositReorg(ctx, sqlcgen.ResolveDepositReorgParams{
		BookingUid: bookingID,
		Kind:       kind,
		Resolution: resolution,
	})
	if err != nil {
		return fmt.Errorf("postgres: resolve reorg: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("postgres: resolve reorg: no open %s anomaly for booking %s: %w", kind, bookingUID, core.ErrNotFound)
	}
	return nil
}
