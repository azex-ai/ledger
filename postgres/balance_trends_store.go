package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

// BalanceTrendsStore implements core.BalanceTrendReader using PostgreSQL.
// Gap-filling is done at the SQL level via generate_series; today's balance
// is overridden with the live checkpoint+delta value computed in Go.
type BalanceTrendsStore struct {
	pool        *pgxpool.Pool
	db          DBTX
	q           *sqlcgen.Queries
	ledgerStore *LedgerStore // used for live balance override
}

// Compile-time check.
var _ core.BalanceTrendReader = (*BalanceTrendsStore)(nil)

// NewBalanceTrendsStore creates a BalanceTrendsStore backed by a connection pool.
func NewBalanceTrendsStore(pool *pgxpool.Pool, ledgerStore *LedgerStore) *BalanceTrendsStore {
	return &BalanceTrendsStore{
		pool:        pool,
		db:          pool,
		q:           sqlcgen.New(pool),
		ledgerStore: ledgerStore,
	}
}

// WithDB returns a clone bound to an existing transaction.
func (s *BalanceTrendsStore) WithDB(db DBTX, ls *LedgerStore) *BalanceTrendsStore {
	return &BalanceTrendsStore{
		pool:        s.pool,
		db:          db,
		q:           sqlcgen.New(db),
		ledgerStore: ls,
	}
}

// GetBalanceTrends returns one BalanceTrendPoint per calendar day in
// [filter.From, filter.Until].
//
// Days without snapshots are forward-filled from the most recent known
// balance (SQL-side, via generate_series + window function group trick).
//
// If filter.Until includes today, the final day's balance is overridden
// with the live checkpoint+delta balance so the series is always current.
func (s *BalanceTrendsStore) GetBalanceTrends(ctx context.Context, filter core.BalanceTrendFilter) ([]core.BalanceTrendPoint, error) {
	from := filter.From
	until := filter.Until

	// Normalise to UTC midnight for consistent date arithmetic.
	from = time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	until = time.Date(until.Year(), until.Month(), until.Day(), 0, 0, 0, 0, time.UTC)

	if until.Before(from) {
		return nil, fmt.Errorf("postgres: balance trends: until must not be before from")
	}

	rows, err := s.q.GetBalanceTrendGapFill(ctx, sqlcgen.GetBalanceTrendGapFillParams{
		FromDate:         pgtype.Date{Time: from, Valid: true},
		UntilDate:        pgtype.Date{Time: until, Valid: true},
		Holder:           filter.AccountHolder,
		CurrencyID:       filter.CurrencyID,
		ClassificationID: filter.ClassificationID,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: balance trends: gap fill query: %w", err)
	}

	// Determine whether today is in the range — if so we will override.
	today := time.Now().UTC()
	todayDate := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	needsLiveOverride := !todayDate.Before(from) && !todayDate.After(until)

	var liveBalance decimal.Decimal
	if needsLiveOverride {
		liveBalance, err = s.ledgerStore.GetBalance(ctx, filter.AccountHolder, filter.CurrencyID, filter.ClassificationID)
		if err != nil {
			// Non-fatal: fall back to snapshot value rather than failing the whole series.
			// This can happen when the account has no entries yet.
			liveBalance = decimal.Zero
		}
	}

	points := make([]core.BalanceTrendPoint, 0, len(rows))
	for _, row := range rows {
		if !row.Day.Valid {
			continue
		}
		day := time.Date(row.Day.Time.Year(), row.Day.Time.Month(), row.Day.Time.Day(), 0, 0, 0, 0, time.UTC)

		bal, err := anyToDecimal(row.Balance)
		if err != nil {
			return nil, fmt.Errorf("postgres: balance trends: convert balance on %s: %w", day.Format("2006-01-02"), err)
		}
		inflow, err := anyToDecimal(row.Inflow)
		if err != nil {
			return nil, fmt.Errorf("postgres: balance trends: convert inflow on %s: %w", day.Format("2006-01-02"), err)
		}
		outflow, err := anyToDecimal(row.Outflow)
		if err != nil {
			return nil, fmt.Errorf("postgres: balance trends: convert outflow on %s: %w", day.Format("2006-01-02"), err)
		}

		// Override today's balance with the live value.
		if needsLiveOverride && day.Equal(todayDate) {
			bal = liveBalance
		}

		points = append(points, core.BalanceTrendPoint{
			Date:    day,
			Balance: bal,
			Inflow:  inflow,
			Outflow: outflow,
		})
	}

	return points, nil
}
