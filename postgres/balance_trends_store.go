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
	dims        *dimCache
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
		dims:        dimCacheFor(pool),
	}
}

// WithDB returns a clone bound to an existing transaction.
func (s *BalanceTrendsStore) WithDB(db DBTX, ls *LedgerStore) *BalanceTrendsStore {
	return &BalanceTrendsStore{
		pool:        s.pool,
		db:          db,
		q:           sqlcgen.New(db),
		ledgerStore: ls,
		dims:        dimCacheForTx(s.dims),
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

	cur, err := s.dims.currencyByUIDOrErr(ctx, s.q, filter.CurrencyUID)
	if err != nil {
		return nil, err
	}
	classificationID := int64(0)
	if filter.ClassificationUID != "" {
		d, err := s.dims.classByUIDOrErr(ctx, s.q, filter.ClassificationUID)
		if err != nil {
			return nil, err
		}
		classificationID = d.ID
	}
	rows, err := s.q.GetBalanceTrendGapFill(ctx, sqlcgen.GetBalanceTrendGapFillParams{
		FromDate:         pgtype.Date{Time: from, Valid: true},
		UntilDate:        pgtype.Date{Time: until, Valid: true},
		Holder:           filter.AccountHolder,
		CurrencyID:       cur.ID,
		ClassificationID: classificationID,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: balance trends: gap fill query: %w", err)
	}

	// Days whose cached snapshot was invalidated by a backdated write are
	// recomputed live below, so the two reads of one business date
	// (/holders/{h}/trends and the as-of endpoint) cannot disagree.
	stale, err := s.staleTrendDays(ctx, filter.AccountHolder, cur.ID, from, until)
	if err != nil {
		return nil, err
	}

	// Determine whether today is in the range — if so we will override.
	today := time.Now().UTC()
	todayDate := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	needsLiveOverride := !todayDate.Before(from) && !todayDate.After(until)

	var liveBalance decimal.Decimal
	if needsLiveOverride {
		liveBalance, err = s.ledgerStore.GetBalance(ctx, filter.AccountHolder, filter.CurrencyUID, filter.ClassificationUID)
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
		} else if stale[day] {
			bal, err = s.liveBalanceAsOf(ctx, filter.AccountHolder, cur.ID, classificationID, day.AddDate(0, 0, 1))
			if err != nil {
				return nil, err
			}
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

// staleTrendDays returns the days in [from, until] whose balance_snapshots
// rows were overtaken by a backdated write -- an entry dated into that day and
// written after the snapshot that was supposed to summarise it.
//
// This exists because GetBalanceTrendGapFill reads balance_snapshots directly
// and therefore inherited none of RollupAdapter.GetSnapshotBalances' self-
// healing. I-14 said "as-of reads self-heal" and named only that one entry
// point; the second reader of the same table was a user-facing HTTP endpoint
// serving values from before the correction (audit A-M5).
//
// Staleness is a property of the DAY, not of one dimension: a snapshot is
// taken for a whole (holder, currency) at once, so its timestamp is the
// baseline every dimension of that day is judged against -- including a
// dimension that has no row of its own because the backdated journal is what
// created it. Judging per dimension would read that case as "not cached,
// therefore not stale" and gap-fill it to zero, which is the second half of
// A-M5.
//
// Days with NO snapshot at all are absent from the result on purpose: those
// are gap-filled from the last known balance, which is the series' contract
// rather than a cache that can go stale.
func (s *BalanceTrendsStore) staleTrendDays(
	ctx context.Context,
	holder, currencyID int64,
	from, until time.Time,
) (map[time.Time]bool, error) {
	rows, err := s.q.ListBalanceTrendSnapshotStaleness(ctx, sqlcgen.ListBalanceTrendSnapshotStalenessParams{
		FromDate:   pgtype.Date{Time: from, Valid: true},
		UntilDate:  pgtype.Date{Time: until, Valid: true},
		Holder:     holder,
		CurrencyID: currencyID,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: balance trends: snapshot staleness: %w", err)
	}

	out := make(map[time.Time]bool, len(rows))
	for _, row := range rows {
		if !row.Day.Valid || row.SnapshotCreatedAt == nil || row.MaxEntryCreatedAt == nil {
			continue
		}
		snapshotAt, err := anyToTime(row.SnapshotCreatedAt)
		if err != nil {
			return nil, fmt.Errorf("postgres: balance trends: snapshot staleness: snapshot_created_at: %w", err)
		}
		entryAt, err := anyToTime(row.MaxEntryCreatedAt)
		if err != nil {
			return nil, fmt.Errorf("postgres: balance trends: snapshot staleness: max_entry_created_at: %w", err)
		}
		if entryAt.After(snapshotAt) {
			d := row.Day.Time
			out[time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)] = true
		}
	}
	return out, nil
}

// liveBalanceAsOf recomputes one trend dimension's balance from
// journal_entries as of cutoff, bypassing balance_snapshots entirely.
func (s *BalanceTrendsStore) liveBalanceAsOf(
	ctx context.Context,
	holder, currencyID, classificationID int64,
	cutoff time.Time,
) (decimal.Decimal, error) {
	raw, err := s.q.GetBalanceAtForTrendDimension(ctx, sqlcgen.GetBalanceAtForTrendDimensionParams{
		Holder:           holder,
		CurrencyID:       currencyID,
		ClassificationID: classificationID,
		Cutoff:           cutoff,
	})
	if err != nil {
		return decimal.Zero, fmt.Errorf("postgres: balance trends: live recompute as of %s: %w", cutoff.Format("2006-01-02"), err)
	}
	bal, err := numericToDecimal(raw)
	if err != nil {
		return decimal.Zero, fmt.Errorf("postgres: balance trends: live recompute convert: %w", err)
	}
	return bal, nil
}
