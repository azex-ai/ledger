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
	"github.com/azex-ai/ledger/service"
)

// Compile-time interface assertions.
var (
	_ service.RollupQueuer         = (*RollupAdapter)(nil)
	_ service.CheckpointReadWriter = (*RollupAdapter)(nil)
	_ service.EntrySummer          = (*RollupAdapter)(nil)
	_ service.GlobalSummer         = (*RollupAdapter)(nil)
	_ service.AccountEntrySummer   = (*RollupAdapter)(nil)
	_ service.CheckpointReader     = (*RollupAdapter)(nil)
	_ service.CheckpointLister     = (*RollupAdapter)(nil)
	_ service.SnapshotWriter       = (*RollupAdapter)(nil)
	_ service.CheckpointAggregator = (*RollupAdapter)(nil)
	_ service.SystemRollupWriter   = (*RollupAdapter)(nil)
	_ service.ExpiredReservationFinder = (*RollupAdapter)(nil)
)

// RollupAdapter implements all service-layer store interfaces needed for background services.
type RollupAdapter struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

// NewRollupAdapter creates a new RollupAdapter.
func NewRollupAdapter(pool *pgxpool.Pool) *RollupAdapter {
	return &RollupAdapter{
		pool: pool,
		q:    sqlcgen.New(pool),
	}
}

// --- RollupQueuer ---

func (a *RollupAdapter) DequeueRollupBatch(ctx context.Context, batchSize int) ([]core.RollupQueueItem, error) {
	rows, err := a.q.DequeueRollupBatch(ctx, int32(batchSize))
	if err != nil {
		return nil, fmt.Errorf("postgres: dequeue rollup batch: %w", err)
	}
	items := make([]core.RollupQueueItem, len(rows))
	for i, r := range rows {
		items[i] = core.RollupQueueItem{
			ID:               r.ID,
			AccountHolder:    r.AccountHolder,
			CurrencyID:       r.CurrencyID,
			ClassificationID: r.ClassificationID,
			CreatedAt:        r.CreatedAt,
		}
	}
	return items, nil
}

func (a *RollupAdapter) MarkRollupProcessed(ctx context.Context, id int64) error {
	return a.q.MarkRollupProcessed(ctx, id)
}

func (a *RollupAdapter) CountPendingRollups(ctx context.Context) (int64, error) {
	return a.q.CountPendingRollups(ctx)
}

// --- CheckpointReadWriter ---

func (a *RollupAdapter) GetCheckpoint(ctx context.Context, holder, currencyID, classificationID int64) (*core.BalanceCheckpoint, error) {
	row, err := a.q.GetBalanceCheckpoint(ctx, sqlcgen.GetBalanceCheckpointParams{
		AccountHolder:    holder,
		CurrencyID:       currencyID,
		ClassificationID: classificationID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("postgres: get checkpoint: %w", err)
	}
	return &core.BalanceCheckpoint{
		AccountHolder:    row.AccountHolder,
		CurrencyID:       row.CurrencyID,
		ClassificationID: row.ClassificationID,
		Balance:          mustNumericToDecimal(row.Balance),
		LastEntryID:      row.LastEntryID,
		LastEntryAt:      row.LastEntryAt,
		UpdatedAt:        row.UpdatedAt,
	}, nil
}

func (a *RollupAdapter) UpsertCheckpoint(ctx context.Context, cp core.BalanceCheckpoint) error {
	return a.q.UpsertBalanceCheckpoint(ctx, sqlcgen.UpsertBalanceCheckpointParams{
		AccountHolder:    cp.AccountHolder,
		CurrencyID:       cp.CurrencyID,
		ClassificationID: cp.ClassificationID,
		Balance:          decimalToNumeric(cp.Balance),
		LastEntryID:      cp.LastEntryID,
		LastEntryAt:      cp.LastEntryAt,
	})
}

// --- EntrySummer ---

func (a *RollupAdapter) SumEntriesSince(ctx context.Context, holder, currencyID, sinceEntryID int64) (debitByClass, creditByClass map[int64]decimal.Decimal, maxEntryID int64, maxEntryAt time.Time, err error) {
	rows, err := a.q.SumEntriesSinceCheckpoint(ctx, sqlcgen.SumEntriesSinceCheckpointParams{
		AccountHolder: holder,
		CurrencyID:    currencyID,
		SinceEntryID:  sinceEntryID,
	})
	if err != nil {
		return nil, nil, 0, time.Time{}, fmt.Errorf("postgres: sum entries since: %w", err)
	}

	debitByClass = make(map[int64]decimal.Decimal)
	creditByClass = make(map[int64]decimal.Decimal)

	for _, r := range rows {
		amount, err := anyToDecimal(r.Total)
		if err != nil {
			return nil, nil, 0, time.Time{}, fmt.Errorf("postgres: sum entries since: convert: %w", err)
		}
		switch core.EntryType(r.EntryType) {
		case core.EntryTypeDebit:
			debitByClass[r.ClassificationID] = debitByClass[r.ClassificationID].Add(amount)
		case core.EntryTypeCredit:
			creditByClass[r.ClassificationID] = creditByClass[r.ClassificationID].Add(amount)
		}
	}

	// Get max entry ID
	maxID, err := a.q.GetMaxEntryID(ctx)
	if err != nil {
		return nil, nil, 0, time.Time{}, fmt.Errorf("postgres: sum entries since: max entry: %w", err)
	}
	maxEntryID = maxID
	maxEntryAt = time.Now() // approximate

	return debitByClass, creditByClass, maxEntryID, maxEntryAt, nil
}

// --- GlobalSummer ---

func (a *RollupAdapter) SumGlobalDebitCredit(ctx context.Context) (debit, credit decimal.Decimal, err error) {
	rows, err := a.q.SumGlobalDebitCredit(ctx)
	if err != nil {
		return decimal.Zero, decimal.Zero, fmt.Errorf("postgres: sum global: %w", err)
	}
	for _, r := range rows {
		amount, err := anyToDecimal(r.Total)
		if err != nil {
			return decimal.Zero, decimal.Zero, fmt.Errorf("postgres: sum global: convert: %w", err)
		}
		switch core.EntryType(r.EntryType) {
		case core.EntryTypeDebit:
			debit = debit.Add(amount)
		case core.EntryTypeCredit:
			credit = credit.Add(amount)
		}
	}
	return debit, credit, nil
}

// --- AccountEntrySummer ---

func (a *RollupAdapter) SumEntriesByAccountClassification(ctx context.Context, holder, currencyID int64) (debitByClass, creditByClass map[int64]decimal.Decimal, err error) {
	rows, err := a.q.SumEntriesByAccountClassification(ctx, sqlcgen.SumEntriesByAccountClassificationParams{
		AccountHolder: holder,
		CurrencyID:    currencyID,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("postgres: sum by account: %w", err)
	}

	debitByClass = make(map[int64]decimal.Decimal)
	creditByClass = make(map[int64]decimal.Decimal)
	for _, r := range rows {
		amount, err := anyToDecimal(r.Total)
		if err != nil {
			return nil, nil, fmt.Errorf("postgres: sum by account: convert: %w", err)
		}
		switch core.EntryType(r.EntryType) {
		case core.EntryTypeDebit:
			debitByClass[r.ClassificationID] = amount
		case core.EntryTypeCredit:
			creditByClass[r.ClassificationID] = amount
		}
	}
	return debitByClass, creditByClass, nil
}

// --- CheckpointReader ---

func (a *RollupAdapter) GetCheckpoints(ctx context.Context, holder, currencyID int64) ([]core.BalanceCheckpoint, error) {
	rows, err := a.q.GetBalanceCheckpoints(ctx, sqlcgen.GetBalanceCheckpointsParams{
		AccountHolder: holder,
		CurrencyID:    currencyID,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: get checkpoints: %w", err)
	}
	result := make([]core.BalanceCheckpoint, len(rows))
	for i, r := range rows {
		result[i] = core.BalanceCheckpoint{
			AccountHolder:    r.AccountHolder,
			CurrencyID:       r.CurrencyID,
			ClassificationID: r.ClassificationID,
			Balance:          mustNumericToDecimal(r.Balance),
			LastEntryID:      r.LastEntryID,
			LastEntryAt:      r.LastEntryAt,
			UpdatedAt:        r.UpdatedAt,
		}
	}
	return result, nil
}

// --- CheckpointLister ---

func (a *RollupAdapter) ListAllCheckpoints(ctx context.Context) ([]core.BalanceCheckpoint, error) {
	rows, err := a.q.ListAllBalanceCheckpoints(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: list all checkpoints: %w", err)
	}
	result := make([]core.BalanceCheckpoint, len(rows))
	for i, r := range rows {
		result[i] = core.BalanceCheckpoint{
			AccountHolder:    r.AccountHolder,
			CurrencyID:       r.CurrencyID,
			ClassificationID: r.ClassificationID,
			Balance:          mustNumericToDecimal(r.Balance),
			LastEntryID:      r.LastEntryID,
			LastEntryAt:      r.LastEntryAt,
			UpdatedAt:        r.UpdatedAt,
		}
	}
	return result, nil
}

// --- SnapshotWriter ---

func (a *RollupAdapter) InsertSnapshot(ctx context.Context, snap core.BalanceSnapshot) error {
	return a.q.InsertSnapshot(ctx, sqlcgen.InsertSnapshotParams{
		AccountHolder:    snap.AccountHolder,
		CurrencyID:       snap.CurrencyID,
		ClassificationID: snap.ClassificationID,
		SnapshotDate:     pgtype.Date{Time: snap.SnapshotDate, Valid: true},
		Balance:          decimalToNumeric(snap.Balance),
	})
}

func (a *RollupAdapter) GetSnapshotBalances(ctx context.Context, holder, currencyID int64, date time.Time) ([]core.Balance, error) {
	rows, err := a.q.GetSnapshotBalances(ctx, sqlcgen.GetSnapshotBalancesParams{
		AccountHolder: holder,
		CurrencyID:    currencyID,
		SnapshotDate:  pgtype.Date{Time: date, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: get snapshot balances: %w", err)
	}
	result := make([]core.Balance, len(rows))
	for i, r := range rows {
		result[i] = core.Balance{
			AccountHolder:    r.AccountHolder,
			CurrencyID:       r.CurrencyID,
			ClassificationID: r.ClassificationID,
			Balance:          mustNumericToDecimal(r.Balance),
		}
	}
	return result, nil
}

// --- CheckpointAggregator ---

func (a *RollupAdapter) AggregateCheckpointsByClassification(ctx context.Context) ([]core.SystemRollup, error) {
	rows, err := a.q.AggregateCheckpointsByClassification(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: aggregate checkpoints: %w", err)
	}
	result := make([]core.SystemRollup, len(rows))
	for i, r := range rows {
		bal, err := anyToDecimal(r.TotalBalance)
		if err != nil {
			return nil, fmt.Errorf("postgres: aggregate checkpoints: convert: %w", err)
		}
		result[i] = core.SystemRollup{
			CurrencyID:       r.CurrencyID,
			ClassificationID: r.ClassificationID,
			TotalBalance:     bal,
		}
	}
	return result, nil
}

// --- SystemRollupWriter ---

func (a *RollupAdapter) UpsertSystemRollup(ctx context.Context, rollup core.SystemRollup) error {
	return a.q.UpsertSystemRollup(ctx, sqlcgen.UpsertSystemRollupParams{
		CurrencyID:       rollup.CurrencyID,
		ClassificationID: rollup.ClassificationID,
		TotalBalance:     decimalToNumeric(rollup.TotalBalance),
	})
}

// --- ExpiredReservationFinder ---

func (a *RollupAdapter) GetExpiredReservations(ctx context.Context, limit int) ([]core.Reservation, error) {
	rows, err := a.q.GetExpiredReservations(ctx, int32(limit))
	if err != nil {
		return nil, fmt.Errorf("postgres: get expired reservations: %w", err)
	}
	result := make([]core.Reservation, len(rows))
	for i, r := range rows {
		result[i] = *reservationFromRow(r)
	}
	return result, nil
}

