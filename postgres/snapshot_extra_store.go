package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

// SnapshotExtraStore implements SparseSnapshotter, SnapshotCountReader, and
// LiveBalanceMerger on top of the same pool used by RollupAdapter.
type SnapshotExtraStore struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

// NewSnapshotExtraStore creates a SnapshotExtraStore.
func NewSnapshotExtraStore(pool *pgxpool.Pool) *SnapshotExtraStore {
	return &SnapshotExtraStore{pool: pool, q: sqlcgen.New(pool)}
}

// Compile-time interface assertions.
var (
	_ core.SparseSnapshotter  = (*SnapshotExtraStore)(nil)
	_ core.SnapshotCountReader = (*SnapshotExtraStore)(nil)
	_ core.LiveBalanceMerger  = (*SnapshotExtraStore)(nil)
)

// --- SparseSnapshotter ---

// UpsertSnapshotSparse inserts snap only when the balance differs from the
// most recent existing snapshot before snap.SnapshotDate. Returns true when
// a row was actually written.
func (s *SnapshotExtraStore) UpsertSnapshotSparse(ctx context.Context, snap core.BalanceSnapshot) (bool, error) {
	// Check whether a prior snapshot exists for this account dimension.
	prev, err := s.q.GetLatestSnapshotBefore(ctx, sqlcgen.GetLatestSnapshotBeforeParams{
		AccountHolder:    snap.AccountHolder,
		CurrencyID:       snap.CurrencyID,
		ClassificationID: snap.ClassificationID,
		SnapshotDate:     pgtype.Date{Time: snap.SnapshotDate, Valid: true},
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("postgres: snapshot_extra: get latest before: %w", err)
	}

	// If the balance is unchanged from the previous snapshot, skip the write.
	if err == nil {
		prevBalance := mustNumericToDecimal(prev.Balance)
		if snap.Balance.Equal(prevBalance) {
			return false, nil
		}
	}

	// Write the snapshot.
	if err := s.q.InsertSnapshot(ctx, sqlcgen.InsertSnapshotParams{
		AccountHolder:    snap.AccountHolder,
		CurrencyID:       snap.CurrencyID,
		ClassificationID: snap.ClassificationID,
		SnapshotDate:     pgtype.Date{Time: snap.SnapshotDate, Valid: true},
		Balance:          decimalToNumeric(snap.Balance),
	}); err != nil {
		return false, fmt.Errorf("postgres: snapshot_extra: insert snapshot: %w", err)
	}
	return true, nil
}

// --- SnapshotCountReader ---

// CountSnapshots returns the total number of rows in balance_snapshots.
func (s *SnapshotExtraStore) CountSnapshots(ctx context.Context) (int64, error) {
	n, err := s.q.CountSnapshotsTotal(ctx)
	if err != nil {
		return 0, fmt.Errorf("postgres: snapshot_extra: count snapshots: %w", err)
	}
	return n, nil
}

// EarliestJournalDate returns the created_at of the oldest journal_entry row,
// or time.Time{} when the table is empty.
func (s *SnapshotExtraStore) EarliestJournalDate(ctx context.Context) (time.Time, error) {
	raw, err := s.q.GetEarliestJournalDate(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("postgres: snapshot_extra: earliest journal: %w", err)
	}
	t, err := anyToTime(raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("postgres: snapshot_extra: earliest journal: convert: %w", err)
	}
	// epoch sentinel means no rows exist.
	if t.IsZero() || t.Year() <= 1970 {
		return time.Time{}, nil
	}
	return t, nil
}

// --- LiveBalanceMerger ---

// MergeWithLive returns snapshots for [startDate, endDate]. When endDate
// is today or in the future, today's entry is synthesised from live
// checkpoint balances rather than the snapshot table.
func (s *SnapshotExtraStore) MergeWithLive(ctx context.Context, holder, currencyID int64, startDate, endDate time.Time) ([]core.BalanceSnapshot, error) {
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	// Clamp query end to yesterday when today falls within range.
	queryEnd := endDate
	includeToday := !endDate.Before(today)
	if includeToday {
		queryEnd = today.AddDate(0, 0, -1)
	}

	var historicRows []core.BalanceSnapshot

	if !queryEnd.Before(startDate) {
		rows, err := s.q.ListSnapshotsByDateRange(ctx, sqlcgen.ListSnapshotsByDateRangeParams{
			AccountHolder: holder,
			CurrencyID:    currencyID,
			StartDate:     pgtype.Date{Time: startDate, Valid: true},
			EndDate:       pgtype.Date{Time: queryEnd, Valid: true},
		})
		if err != nil {
			return nil, fmt.Errorf("postgres: snapshot_extra: list by date range: %w", err)
		}
		historicRows = make([]core.BalanceSnapshot, len(rows))
		for i, r := range rows {
			historicRows[i] = core.BalanceSnapshot{
				AccountHolder:    r.AccountHolder,
				CurrencyID:       r.CurrencyID,
				ClassificationID: r.ClassificationID,
				SnapshotDate:     r.SnapshotDate.Time,
				Balance:          mustNumericToDecimal(r.Balance),
			}
		}
	}

	if !includeToday {
		return historicRows, nil
	}

	// Fetch live balances from checkpoints for today.
	liveRows, err := s.q.GetBalanceCheckpoints(ctx, sqlcgen.GetBalanceCheckpointsParams{
		AccountHolder: holder,
		CurrencyID:    currencyID,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: snapshot_extra: live balances: %w", err)
	}

	for _, r := range liveRows {
		historicRows = append(historicRows, core.BalanceSnapshot{
			AccountHolder:    r.AccountHolder,
			CurrencyID:       r.CurrencyID,
			ClassificationID: r.ClassificationID,
			SnapshotDate:     today,
			Balance:          mustNumericToDecimal(r.Balance),
		})
	}

	// When no checkpoint exists yet, synthesise a zero balance for each
	// classification present in historic rows so callers always have today.
	if len(liveRows) == 0 && len(historicRows) > 0 {
		seen := make(map[int64]bool)
		for _, r := range historicRows {
			if !seen[r.ClassificationID] {
				seen[r.ClassificationID] = true
				historicRows = append(historicRows, core.BalanceSnapshot{
					AccountHolder:    holder,
					CurrencyID:       currencyID,
					ClassificationID: r.ClassificationID,
					SnapshotDate:     today,
					Balance:          decimal.Zero,
				})
			}
		}
	}

	return historicRows, nil
}
