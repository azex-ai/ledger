package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

// QueryStore implements server.QueryProvider for read-only list/get queries.
//
// In pool mode (constructed via NewQueryStore), queries run against the pool.
// In tx mode (bound via withDB), queries participate in the caller's
// transaction.
type QueryStore struct {
	// pool is non-nil only in pool mode. Nil signals tx mode.
	pool *pgxpool.Pool
	db   DBTX
	q    *sqlcgen.Queries
}

// NewQueryStore creates a new QueryStore.
func NewQueryStore(pool *pgxpool.Pool) *QueryStore {
	return &QueryStore{
		pool: pool,
		db:   pool,
		q:    sqlcgen.New(pool),
	}
}

// WithDB returns a clone of the QueryStore bound to an existing transaction.
func (s *QueryStore) WithDB(db DBTX) *QueryStore {
	return &QueryStore{
		pool: nil, // tx mode
		db:   db,
		q:    sqlcgen.New(db),
	}
}

// Compile-time check.
var _ core.QueryProvider = (*QueryStore)(nil)

// --- JournalQuerier ---

func (s *QueryStore) GetJournal(ctx context.Context, id int64) (*core.Journal, []core.Entry, error) {
	row, err := s.q.GetJournal(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, fmt.Errorf("postgres: get journal: id %d: %w", id, core.ErrNotFound)
		}
		return nil, nil, fmt.Errorf("postgres: get journal: %w", err)
	}

	entryRows, err := s.q.ListJournalEntries(ctx, id)
	if err != nil {
		return nil, nil, fmt.Errorf("postgres: get journal entries: %w", err)
	}

	entries := make([]core.Entry, len(entryRows))
	for i, e := range entryRows {
		entries[i] = *entryFromRow(e)
	}
	return journalFromRow(row), entries, nil
}

func (s *QueryStore) ListJournals(ctx context.Context, cursorID int64, limit int32) ([]core.Journal, error) {
	rows, err := s.q.ListJournalsCursor(ctx, sqlcgen.ListJournalsCursorParams{
		CursorID:  cursorID,
		PageLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: list journals: %w", err)
	}
	result := make([]core.Journal, len(rows))
	for i, j := range rows {
		result[i] = *journalFromRow(j)
	}
	return result, nil
}

// --- EntryQuerier ---

func (s *QueryStore) ListEntriesByAccount(ctx context.Context, holder, currencyID, cursorID int64, limit int32) ([]core.Entry, error) {
	rows, err := s.q.ListEntriesByAccount(ctx, sqlcgen.ListEntriesByAccountParams{
		AccountHolder: holder,
		CurrencyID:    currencyID,
		CursorID:      cursorID,
		PageLimit:     limit,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: list entries: %w", err)
	}
	result := make([]core.Entry, len(rows))
	for i, e := range rows {
		result[i] = *entryFromRow(e)
	}
	return result, nil
}

// --- ReservationQuerier ---

func (s *QueryStore) ListReservations(ctx context.Context, holder int64, status string, limit int32) ([]core.Reservation, error) {
	rows, err := s.q.ListReservationsByAccount(ctx, sqlcgen.ListReservationsByAccountParams{
		AccountHolder: holder,
		FilterStatus:  status,
		PageLimit:     limit,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: list reservations: %w", err)
	}
	result := make([]core.Reservation, len(rows))
	for i, r := range rows {
		result[i] = *reservationFromRow(r)
	}
	return result, nil
}

// --- SnapshotQuerier ---

func (s *QueryStore) ListSnapshotsByDateRange(ctx context.Context, holder, currencyID int64, start, end time.Time) ([]core.BalanceSnapshot, error) {
	rows, err := s.q.ListSnapshotsByDateRange(ctx, sqlcgen.ListSnapshotsByDateRangeParams{
		AccountHolder: holder,
		CurrencyID:    currencyID,
		StartDate:     pgtype.Date{Time: start, Valid: true},
		EndDate:       pgtype.Date{Time: end, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: list snapshots: %w", err)
	}
	result := make([]core.BalanceSnapshot, len(rows))
	for i, r := range rows {
		result[i] = core.BalanceSnapshot{
			AccountHolder:    r.AccountHolder,
			CurrencyID:       r.CurrencyID,
			ClassificationID: r.ClassificationID,
			SnapshotDate:     r.SnapshotDate.Time,
			Balance:          mustNumericToDecimal(r.Balance),
		}
	}
	return result, nil
}

// --- SystemRollupQuerier ---

func (s *QueryStore) GetSystemRollups(ctx context.Context) ([]core.SystemRollup, error) {
	const realtimeSystemRollupsSQL = `
WITH active AS (
  SELECT DISTINCT account_holder, currency_id, classification_id
  FROM journal_entries
),
realtime AS (
  SELECT
    a.currency_id,
    a.classification_id,
    COALESCE(bc.balance, 0) + COALESCE(d.delta, 0) AS balance
  FROM active a
  INNER JOIN classifications c ON c.id = a.classification_id
  LEFT JOIN balance_checkpoints bc
         ON bc.account_holder    = a.account_holder
        AND bc.currency_id       = a.currency_id
        AND bc.classification_id = a.classification_id
  LEFT JOIN LATERAL (
    SELECT COALESCE(SUM(
      CASE
        WHEN (c.normal_side = 'debit'  AND je.entry_type = 'debit')
          OR (c.normal_side = 'credit' AND je.entry_type = 'credit')
        THEN je.amount
        ELSE -je.amount
      END
    ), 0)::numeric AS delta
    FROM journal_entries je
    WHERE je.account_holder    = a.account_holder
      AND je.currency_id       = a.currency_id
      AND je.classification_id = a.classification_id
      AND je.id                > COALESCE(bc.last_entry_id, 0)
  ) d ON TRUE
)
SELECT currency_id, classification_id, COALESCE(SUM(balance), 0)::numeric AS total_balance, now() AS updated_at
FROM realtime
GROUP BY currency_id, classification_id
ORDER BY currency_id, classification_id`

	rows, err := s.db.Query(ctx, realtimeSystemRollupsSQL)
	if err != nil {
		return nil, fmt.Errorf("postgres: get system rollups: %w", err)
	}
	defer rows.Close()

	result := make([]core.SystemRollup, 0)
	for rows.Next() {
		var (
			currencyID       int64
			classificationID int64
			totalBalance     pgtype.Numeric
			updatedAt        time.Time
		)
		if err := rows.Scan(&currencyID, &classificationID, &totalBalance, &updatedAt); err != nil {
			return nil, fmt.Errorf("postgres: get system rollups: scan: %w", err)
		}
		balance, err := numericToDecimal(totalBalance)
		if err != nil {
			return nil, fmt.Errorf("postgres: get system rollups: convert balance: %w", err)
		}
		result = append(result, core.SystemRollup{
			CurrencyID:       currencyID,
			ClassificationID: classificationID,
			TotalBalance:     balance,
			UpdatedAt:        updatedAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: get system rollups: rows: %w", err)
	}
	return result, nil
}

// --- HealthQuerier ---

func (s *QueryStore) GetHealthMetrics(ctx context.Context) (*core.HealthMetrics, error) {
	pendingRollups, err := s.q.CountPendingRollups(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: health: count pending rollups: %w", err)
	}

	maxAge, err := s.q.GetCheckpointMaxAgeSeconds(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: health: checkpoint max age: %w", err)
	}

	activeRes, err := s.q.CountActiveReservations(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: health: count active reservations: %w", err)
	}

	return &core.HealthMetrics{
		RollupQueueDepth:        pendingRollups,
		CheckpointMaxAgeSeconds: int(maxAge),
		ActiveReservations:      activeRes,
	}, nil
}
