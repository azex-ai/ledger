package service

import (
	"context"
	"fmt"
	"time"

	"github.com/azex-ai/ledger/core"
)

// HistoricalBalanceLister lists balances as of an exclusive upper-bound timestamp.
type HistoricalBalanceLister interface {
	ListBalancesAt(ctx context.Context, cutoff time.Time) ([]core.Balance, error)
}

// SnapshotWriter writes and reads balance snapshots.
type SnapshotWriter interface {
	UpsertSnapshot(ctx context.Context, snap core.BalanceSnapshot) error
	GetSnapshotBalances(ctx context.Context, holder, currencyID int64, date time.Time) ([]core.Balance, error)
}

// SnapshotService handles daily balance snapshots.
type SnapshotService struct {
	balances  HistoricalBalanceLister
	snapshots SnapshotWriter
	logger    core.Logger
	metrics   core.Metrics
}

// NewSnapshotService creates a new SnapshotService.
func NewSnapshotService(
	balances HistoricalBalanceLister,
	snapshots SnapshotWriter,
	engine *core.Engine,
) *SnapshotService {
	return &SnapshotService{
		balances:  balances,
		snapshots: snapshots,
		logger:    engine.Logger(),
		metrics:   engine.Metrics(),
	}
}

// CreateDailySnapshot recomputes balances as of the end of the given day and stores them as snapshots.
func (s *SnapshotService) CreateDailySnapshot(ctx context.Context, date time.Time) error {
	start := time.Now()

	// Normalize date to midnight and snapshot balances as of the next midnight.
	snapshotDate := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
	cutoff := snapshotDate.AddDate(0, 0, 1)

	balances, err := s.balances.ListBalancesAt(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("service: snapshot: list balances at %s: %w", cutoff.Format(time.RFC3339), err)
	}

	for _, balance := range balances {
		snap := core.BalanceSnapshot{
			AccountHolder:    balance.AccountHolder,
			CurrencyID:       balance.CurrencyID,
			ClassificationID: balance.ClassificationID,
			SnapshotDate:     snapshotDate,
			Balance:          balance.Balance,
		}
		if err := s.snapshots.UpsertSnapshot(ctx, snap); err != nil {
			return fmt.Errorf("service: snapshot: insert: holder=%d currency=%d class=%d: %w",
				balance.AccountHolder, balance.CurrencyID, balance.ClassificationID, err)
		}
	}

	s.metrics.SnapshotLatency(time.Since(start))
	s.logger.Info("service: snapshot: daily snapshot created",
		"date", snapshotDate.Format("2006-01-02"),
		"count", len(balances),
	)

	return nil
}

// GetSnapshotBalance reads balance snapshots for a specific holder, currency, and date.
func (s *SnapshotService) GetSnapshotBalance(ctx context.Context, holder int64, currencyID int64, date time.Time) ([]core.Balance, error) {
	snapshotDate := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)

	balances, err := s.snapshots.GetSnapshotBalances(ctx, holder, currencyID, snapshotDate)
	if err != nil {
		return nil, fmt.Errorf("service: snapshot: get balances: %w", err)
	}

	return balances, nil
}

// Compile-time interface check.
var _ core.Snapshotter = (*SnapshotService)(nil)
