package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

var _ core.PeriodCloser = (*PeriodCloseStore)(nil)

// PeriodCloseStore implements core.PeriodCloser using PostgreSQL.
//
// In pool mode (constructed via NewPeriodCloseStore), queries run against the
// pool. In tx mode (bound via WithDB), queries participate in the caller's
// transaction — used so ClosePeriod can be composed with other writes via
// ledger.Service.RunInTx.
type PeriodCloseStore struct {
	// pool is non-nil only in pool mode. Nil signals tx mode.
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

// NewPeriodCloseStore creates a new PeriodCloseStore.
func NewPeriodCloseStore(pool *pgxpool.Pool) *PeriodCloseStore {
	return &PeriodCloseStore{
		pool: pool,
		q:    sqlcgen.New(pool),
	}
}

// WithDB returns a clone of the PeriodCloseStore bound to an existing transaction.
func (s *PeriodCloseStore) WithDB(db DBTX) *PeriodCloseStore {
	return &PeriodCloseStore{
		pool: nil, // tx mode
		q:    sqlcgen.New(db),
	}
}

// ClosePeriod appends a new period-close line. Append-only: this never
// updates or deletes an existing row — reopening a period is done by
// appending a row with an earlier CloseBefore (latest-row-wins, see
// GetActivePeriodClose).
func (s *PeriodCloseStore) ClosePeriod(ctx context.Context, input core.ClosePeriodInput) (*core.PeriodClose, error) {
	if err := input.Validate(); err != nil {
		return nil, fmt.Errorf("postgres: close period: %w", err)
	}

	row, err := s.q.InsertPeriodClose(ctx, sqlcgen.InsertPeriodCloseParams{
		CloseBefore: input.CloseBefore,
		Note:        input.Note,
		ActorID:     input.ActorID,
	})
	if err != nil {
		return nil, wrapStoreError("postgres: close period: insert", err)
	}
	return periodCloseFromRow(row), nil
}

// ActiveCloseLine returns the current close_before line, or the zero Time if
// the period has never been closed.
func (s *PeriodCloseStore) ActiveCloseLine(ctx context.Context) (time.Time, error) {
	row, err := s.q.GetActivePeriodClose(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, nil
		}
		return time.Time{}, fmt.Errorf("postgres: active close line: %w", err)
	}
	return row.CloseBefore, nil
}

// ListPeriodCloses returns the close-line history, most recent first.
func (s *PeriodCloseStore) ListPeriodCloses(ctx context.Context, limit int) ([]core.PeriodClose, error) {
	rows, err := s.q.ListPeriodCloses(ctx, int32(limit))
	if err != nil {
		return nil, fmt.Errorf("postgres: list period closes: %w", err)
	}
	result := make([]core.PeriodClose, len(rows))
	for i, row := range rows {
		result[i] = *periodCloseFromRow(row)
	}
	return result, nil
}

func periodCloseFromRow(row sqlcgen.PeriodClose) *core.PeriodClose {
	return &core.PeriodClose{
		ID:          row.ID,
		CloseBefore: row.CloseBefore,
		Note:        row.Note,
		ActorID:     row.ActorID,
		CreatedAt:   row.CreatedAt,
	}
}
