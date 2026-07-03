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
	dims *dimCache
}

// NewSnapshotExtraStore creates a SnapshotExtraStore.
func NewSnapshotExtraStore(pool *pgxpool.Pool) *SnapshotExtraStore {
	return &SnapshotExtraStore{pool: pool, q: sqlcgen.New(pool), dims: dimCacheFor(pool)}
}

// Compile-time interface assertions.
var (
	_ core.SparseSnapshotter   = (*SnapshotExtraStore)(nil)
	_ core.SnapshotCountReader = (*SnapshotExtraStore)(nil)
	_ core.LiveBalanceMerger   = (*SnapshotExtraStore)(nil)
)

// --- SparseSnapshotter ---

// UpsertSnapshotSparse inserts snap only when the balance differs from the
// most recent existing snapshot before snap.SnapshotDate. Returns true when
// a row was actually written.
func (s *SnapshotExtraStore) UpsertSnapshotSparse(ctx context.Context, snap core.BalanceSnapshot) (bool, error) {
	cur, err := s.dims.currencyByUIDOrErr(ctx, s.q, snap.CurrencyUID)
	if err != nil {
		return false, err
	}
	cls, err := s.dims.classByUIDOrErr(ctx, s.q, snap.ClassificationUID)
	if err != nil {
		return false, err
	}

	// Check whether a prior snapshot exists for this account dimension.
	prev, err := s.q.GetLatestSnapshotBefore(ctx, sqlcgen.GetLatestSnapshotBeforeParams{
		AccountHolder:    snap.AccountHolder,
		CurrencyID:       cur.ID,
		ClassificationID: cls.ID,
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
		CurrencyID:       cur.ID,
		ClassificationID: cls.ID,
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

// EarliestJournalDate returns the effective_at of the oldest journal_entry
// row (business date, not write date), or time.Time{} when the table is empty.
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
func (s *SnapshotExtraStore) MergeWithLive(ctx context.Context, holder int64, currencyUID string, startDate, endDate time.Time) ([]core.BalanceSnapshot, error) {
	cur, err := s.dims.currencyByUIDOrErr(ctx, s.q, currencyUID)
	if err != nil {
		return nil, err
	}

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
			CurrencyID:    cur.ID,
			StartDate:     pgtype.Date{Time: startDate, Valid: true},
			EndDate:       pgtype.Date{Time: queryEnd, Valid: true},
		})
		if err != nil {
			return nil, fmt.Errorf("postgres: snapshot_extra: list by date range: %w", err)
		}
		historicRows = make([]core.BalanceSnapshot, len(rows))
		for i, r := range rows {
			cls, err := s.dims.classByIDOrErr(ctx, s.q, r.ClassificationID)
			if err != nil {
				return nil, err
			}
			historicRows[i] = core.BalanceSnapshot{
				AccountHolder:     r.AccountHolder,
				CurrencyUID:       currencyUID,
				ClassificationUID: cls.UID,
				SnapshotDate:      r.SnapshotDate.Time,
				Balance:           mustNumericToDecimal(r.Balance),
			}
		}
	}

	if !includeToday {
		return historicRows, nil
	}

	// Fetch live balances from checkpoints for today.
	liveRows, err := s.q.GetBalanceCheckpoints(ctx, sqlcgen.GetBalanceCheckpointsParams{
		AccountHolder: holder,
		CurrencyID:    cur.ID,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: snapshot_extra: live balances: %w", err)
	}

	for _, r := range liveRows {
		cls, err := s.dims.classByIDOrErr(ctx, s.q, r.ClassificationID)
		if err != nil {
			return nil, err
		}
		historicRows = append(historicRows, core.BalanceSnapshot{
			AccountHolder:     r.AccountHolder,
			CurrencyUID:       currencyUID,
			ClassificationUID: cls.UID,
			SnapshotDate:      today,
			Balance:           mustNumericToDecimal(r.Balance),
		})
	}

	// When no checkpoint exists yet, synthesise a zero balance for each
	// classification present in historic rows so callers always have today.
	if len(liveRows) == 0 && len(historicRows) > 0 {
		seen := make(map[string]bool)
		for _, r := range historicRows {
			if !seen[r.ClassificationUID] {
				seen[r.ClassificationUID] = true
				historicRows = append(historicRows, core.BalanceSnapshot{
					AccountHolder:     holder,
					CurrencyUID:       currencyUID,
					ClassificationUID: r.ClassificationUID,
					SnapshotDate:      today,
					Balance:           decimal.Zero,
				})
			}
		}
	}

	return historicRows, nil
}
