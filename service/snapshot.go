package service

import (
	"context"
	"fmt"
	"time"

	"github.com/azex-ai/ledger/core"
)

// CheckpointLister lists all balance checkpoints.
type CheckpointLister interface {
	ListAllCheckpoints(ctx context.Context) ([]core.BalanceCheckpoint, error)
}

// SnapshotWriter writes and reads balance snapshots.
type SnapshotWriter interface {
	InsertSnapshot(ctx context.Context, snap core.BalanceSnapshot) error
	GetSnapshotBalances(ctx context.Context, holder, currencyID int64, date time.Time) ([]core.Balance, error)
}

// SnapshotService handles daily balance snapshots.
type SnapshotService struct {
	checkpoints CheckpointLister
	snapshots   SnapshotWriter
	logger      core.Logger
	metrics     core.Metrics
}

// NewSnapshotService creates a new SnapshotService.
func NewSnapshotService(
	checkpoints CheckpointLister,
	snapshots SnapshotWriter,
	engine *core.Engine,
) *SnapshotService {
	return &SnapshotService{
		checkpoints: checkpoints,
		snapshots:   snapshots,
		logger:      engine.Logger(),
		metrics:     engine.Metrics(),
	}
}

// CreateDailySnapshot reads all balance_checkpoints and inserts them as snapshots for the given date.
func (s *SnapshotService) CreateDailySnapshot(ctx context.Context, date time.Time) error {
	start := time.Now()

	cps, err := s.checkpoints.ListAllCheckpoints(ctx)
	if err != nil {
		return fmt.Errorf("service: snapshot: list checkpoints: %w", err)
	}

	// Normalize date to midnight
	snapshotDate := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)

	for _, cp := range cps {
		snap := core.BalanceSnapshot{
			AccountHolder:    cp.AccountHolder,
			CurrencyID:       cp.CurrencyID,
			ClassificationID: cp.ClassificationID,
			SnapshotDate:     snapshotDate,
			Balance:          cp.Balance,
		}
		if err := s.snapshots.InsertSnapshot(ctx, snap); err != nil {
			return fmt.Errorf("service: snapshot: insert: holder=%d currency=%d class=%d: %w",
				cp.AccountHolder, cp.CurrencyID, cp.ClassificationID, err)
		}
	}

	s.metrics.SnapshotLatency(time.Since(start))
	s.logger.Info("service: snapshot: daily snapshot created",
		"date", snapshotDate.Format("2006-01-02"),
		"count", len(cps),
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
