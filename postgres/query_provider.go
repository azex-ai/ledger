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
	dims *dimCache
}

// NewQueryStore creates a new QueryStore.
func NewQueryStore(pool *pgxpool.Pool) *QueryStore {
	return &QueryStore{
		pool: pool,
		db:   pool,
		q:    sqlcgen.New(pool),
		dims: dimCacheFor(pool),
	}
}

// WithDB returns a clone of the QueryStore bound to an existing transaction.
func (s *QueryStore) WithDB(db DBTX) *QueryStore {
	return &QueryStore{
		pool: nil, // tx mode
		db:   db,
		q:    sqlcgen.New(db),
		dims: s.dims,
	}
}

// Compile-time check.
var _ core.QueryProvider = (*QueryStore)(nil)

// --- JournalQuerier ---

func (s *QueryStore) GetJournal(ctx context.Context, uid string) (*core.Journal, []core.Entry, error) {
	pgUID, err := uidToPG(uid)
	if err != nil {
		return nil, nil, err
	}
	row, err := s.q.GetJournalByUID(ctx, pgUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, fmt.Errorf("postgres: get journal %q: %w", uid, core.ErrNotFound)
		}
		return nil, nil, fmt.Errorf("postgres: get journal: %w", err)
	}

	entryRows, err := s.q.ListJournalEntries(ctx, row.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("postgres: get journal entries: %w", err)
	}

	entries := make([]core.Entry, len(entryRows))
	for i, e := range entryRows {
		entry, err := entryCore(ctx, s.dims, s.q, e.JournalUid, e.AccountHolder, e.CurrencyID, e.ClassificationID, e.EntryType, e.Amount, e.EffectiveAt, e.CreatedAt)
		if err != nil {
			return nil, nil, err
		}
		entries[i] = *entry
	}
	journal, err := journalFromRow(ctx, s.dims, s.q, row)
	if err != nil {
		return nil, nil, err
	}
	return journal, entries, nil
}

func (s *QueryStore) ListJournals(ctx context.Context, cursor string, limit int32) ([]core.Journal, string, error) {
	cursorID := decodeAuditCursor(cursor)
	rows, err := s.q.ListJournalsCursor(ctx, sqlcgen.ListJournalsCursorParams{
		CursorID:  cursorID,
		PageLimit: limit,
	})
	if err != nil {
		return nil, "", fmt.Errorf("postgres: list journals: %w", err)
	}
	result := make([]core.Journal, len(rows))
	for i, j := range rows {
		journal, err := journalFromRow(ctx, s.dims, s.q, j)
		if err != nil {
			return nil, "", err
		}
		result[i] = *journal
	}
	return result, nextAuditCursor(rows, limit), nil
}

// --- EntryQuerier ---

func (s *QueryStore) ListEntriesByAccount(ctx context.Context, holder int64, currencyUID string, cursor string, limit int32) ([]core.Entry, string, error) {
	cur, err := s.dims.currencyByUIDOrErr(ctx, s.q, currencyUID)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.q.ListEntriesByAccount(ctx, sqlcgen.ListEntriesByAccountParams{
		AccountHolder: holder,
		CurrencyID:    cur.ID,
		CursorID:      decodeAuditCursor(cursor),
		PageLimit:     limit,
	})
	if err != nil {
		return nil, "", fmt.Errorf("postgres: list entries: %w", err)
	}
	result := make([]core.Entry, len(rows))
	for i, e := range rows {
		entry, err := entryCore(ctx, s.dims, s.q, e.JournalUid, e.AccountHolder, e.CurrencyID, e.ClassificationID, e.EntryType, e.Amount, e.EffectiveAt, e.CreatedAt)
		if err != nil {
			return nil, "", err
		}
		result[i] = *entry
	}
	nextCursor := ""
	if limit > 0 && int32(len(rows)) == limit {
		nextCursor = encodeCursorString(rows[len(rows)-1].ID.Int64)
	}
	return result, nextCursor, nil
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
		res, err := reservationFromRow(ctx, s.dims, s.q, r)
		if err != nil {
			return nil, err
		}
		result[i] = *res
	}
	return result, nil
}

// --- SnapshotQuerier ---

func (s *QueryStore) ListSnapshotsByDateRange(ctx context.Context, holder int64, currencyUID string, start, end time.Time) ([]core.BalanceSnapshot, error) {
	cur, err := s.dims.currencyByUIDOrErr(ctx, s.q, currencyUID)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListSnapshotsByDateRange(ctx, sqlcgen.ListSnapshotsByDateRangeParams{
		AccountHolder: holder,
		CurrencyID:    cur.ID,
		StartDate:     pgtype.Date{Time: start, Valid: true},
		EndDate:       pgtype.Date{Time: end, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: list snapshots: %w", err)
	}
	result := make([]core.BalanceSnapshot, len(rows))
	for i, r := range rows {
		cls, err := s.dims.classByIDOrErr(ctx, s.q, r.ClassificationID)
		if err != nil {
			return nil, err
		}
		result[i] = core.BalanceSnapshot{
			AccountHolder:     r.AccountHolder,
			CurrencyUID:       currencyUID,
			ClassificationUID: cls.UID,
			SnapshotDate:      r.SnapshotDate.Time,
			Balance:           mustNumericToDecimal(r.Balance),
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
		cur, err := s.dims.currencyByIDOrErr(ctx, s.q, currencyID)
		if err != nil {
			return nil, err
		}
		cls, err := s.dims.classByIDOrErr(ctx, s.q, classificationID)
		if err != nil {
			return nil, err
		}
		result = append(result, core.SystemRollup{
			CurrencyUID:       cur.UID,
			ClassificationUID: cls.UID,
			TotalBalance:      balance,
			UpdatedAt:         updatedAt,
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
