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

// periodCloseBarrierBudget bounds how long ClosePeriod waits for in-flight
// journal writers to finish before giving up. Ten seconds is generous for
// ordinary writers and short enough that a consumer holding a long RunInTx
// open gets a legible error instead of an indefinite hang.
const periodCloseBarrierBudget = 10 * time.Second

// periodCloseBarrierInterval is the poll interval used while waiting for the
// exclusive barrier. See TryAcquirePeriodCloseBarrier's comment in
// queries/periods.sql for why this polls instead of blocking in the lock
// manager.
const periodCloseBarrierInterval = 100 * time.Millisecond

// acquirePeriodReadBarrier takes the SHARED half of the period-close barrier
// (I-61) in q's transaction. Every journal write path calls this immediately
// before reading the active close line; it is held until that transaction
// ends, so ClosePeriod cannot make a new line active underneath a writer
// that has already decided its effective_at is allowed.
//
// Re-entrant and order-free: the exclusive half never blocks in PostgreSQL's
// lock manager (it polls), so this shared request is never queued behind a
// pending close and cannot participate in a wait-for cycle no matter which
// other locks the calling path already holds. See
// TryAcquirePeriodCloseBarrier in queries/periods.sql.
func acquirePeriodReadBarrier(ctx context.Context, q *sqlcgen.Queries) error {
	if err := q.AcquirePeriodReadBarrier(ctx); err != nil {
		return fmt.Errorf("postgres: period close read barrier: %w", normalizeStoreError(err))
	}
	return nil
}

// ClosePeriod appends a new period-close line. Append-only: this never
// updates or deletes an existing row — reopening a period is done by
// appending a row with an earlier CloseBefore (latest-row-wins, see
// GetActivePeriodClose).
//
// The INSERT happens under the EXCLUSIVE half of the period-close barrier
// (I-61), which is what makes I-15 a statement about concurrent execution
// rather than about single-threaded test order: the line cannot become
// active while a journal write that already read the previous line is still
// in flight. Waiting is bounded (periodCloseBarrierBudget); on exhaustion
// this returns core.ErrTransient naming the reason rather than blocking
// forever or -- worse -- closing the period anyway.
//
// Pool mode opens its own transaction for this: the barrier is
// transaction-scoped, and running the INSERT as a bare autocommit statement
// on the pool would release the lock at the end of the lock statement
// itself, before the INSERT it is supposed to protect.
func (s *PeriodCloseStore) ClosePeriod(ctx context.Context, input core.ClosePeriodInput) (*core.PeriodClose, error) {
	if err := input.Validate(); err != nil {
		return nil, fmt.Errorf("postgres: close period: %w", err)
	}

	if s.pool == nil {
		// Tx mode: the caller owns the transaction. Waiting here holds that
		// transaction open, which is the caller's choice to make by composing
		// ClosePeriod into RunInTx; the budget still bounds it.
		return s.closePeriodWithQueries(ctx, s.q, input)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: close period: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	row, err := s.closePeriodWithQueries(ctx, sqlcgen.New(tx), input)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: close period: commit: %w", err)
	}
	return row, nil
}

func (s *PeriodCloseStore) closePeriodWithQueries(ctx context.Context, q *sqlcgen.Queries, input core.ClosePeriodInput) (*core.PeriodClose, error) {
	if err := acquirePeriodCloseBarrier(ctx, q); err != nil {
		return nil, err
	}

	row, err := q.InsertPeriodClose(ctx, sqlcgen.InsertPeriodCloseParams{
		CloseBefore: input.CloseBefore,
		Note:        input.Note,
		ActorID:     input.ActorID,
		Uid:         newUID(),
	})
	if err != nil {
		return nil, wrapStoreError("postgres: close period: insert", err)
	}
	return periodCloseFromRow(row), nil
}

// acquirePeriodCloseBarrier polls for the exclusive half of the period-close
// barrier until it is granted or the budget runs out. Failure is a real
// failure: the caller does NOT get to append a close line it could not
// serialize against in-flight writers (working-agreements §3 -- fail closed,
// never fall back to the unprotected path).
func acquirePeriodCloseBarrier(ctx context.Context, q *sqlcgen.Queries) error {
	deadline := time.Now().Add(periodCloseBarrierBudget)
	attempts := 0
	for {
		got, err := q.TryAcquirePeriodCloseBarrier(ctx)
		if err != nil {
			return fmt.Errorf("postgres: close period: acquire barrier: %w", normalizeStoreError(err))
		}
		attempts++
		if got {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"postgres: close period: journal writes still in flight after %s (%d attempts); "+
					"the close line was NOT appended -- retry once the in-flight transactions finish: %w",
				periodCloseBarrierBudget, attempts, core.ErrTransient,
			)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("postgres: close period: acquire barrier: %w", ctx.Err())
		case <-time.After(periodCloseBarrierInterval):
		}
	}
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
		UID:         pgToUID(row.Uid),
		CloseBefore: row.CloseBefore,
		Note:        row.Note,
		ActorID:     row.ActorID,
		CreatedAt:   row.CreatedAt,
	}
}
