package service

import (
	"context"
	"fmt"
	"time"

	"github.com/azex-ai/ledger/core"
)

// PartitionEnsurer is the port the partition job consumes; implemented by
// postgres.PartitionStore.
type PartitionEnsurer interface {
	// EnsureMonthlyPartitions creates the monthly journal_entries partitions
	// covering [current month .. +monthsAhead]; returns names created.
	EnsureMonthlyPartitions(ctx context.Context, now time.Time, monthsAhead int) ([]string, error)
	// DefaultPartitionHasRows reports rows stranded in the default
	// partition — an alertable signal when partitions are actively managed.
	DefaultPartitionHasRows(ctx context.Context) (bool, error)
	// RebalanceDefault moves stranded default-partition rows into monthly
	// partitions (creating them as needed) and re-attaches an empty default.
	RebalanceDefault(ctx context.Context, now time.Time, monthsAhead int) ([]string, error)
}

// PartitionService keeps the journal_entries monthly-partition horizon ahead
// of the clock (docs/INVARIANTS.md I-13). Run from the worker on
// PartitionInterval, advisory-locked so a single replica does the DDL.
type PartitionService struct {
	store  PartitionEnsurer
	logger core.Logger
}

// NewPartitionService creates a PartitionService.
func NewPartitionService(store PartitionEnsurer, engine *core.Engine) *PartitionService {
	return &PartitionService{store: store, logger: engine.Logger()}
}

// EnsureUpcoming creates any missing monthly partitions through the horizon
// and logs a loud warning if rows are stranded in the default partition.
func (s *PartitionService) EnsureUpcoming(ctx context.Context, now time.Time, monthsAhead int) error {
	created, err := s.store.EnsureMonthlyPartitions(ctx, now, monthsAhead)
	if err != nil {
		return fmt.Errorf("service: partition: ensure monthly: %w", err)
	}
	if len(created) > 0 {
		s.logger.Info("partition: created journal_entries partitions", "partitions", created)
	}
	hasRows, err := s.store.DefaultPartitionHasRows(ctx)
	if err != nil {
		return fmt.Errorf("service: partition: default check: %w", err)
	}
	if hasRows {
		// Should be impossible while the horizon holds — rows landed outside
		// every named partition (created_at outliers?). Rebalance them into
		// monthly partitions now, and log loudly: the cause needs eyes even
		// though the data is healed.
		s.logger.Error("partition: journal_entries_default held rows — rebalancing; investigate created_at outliers")
		rebalanced, err := s.store.RebalanceDefault(ctx, now, monthsAhead)
		if err != nil {
			return fmt.Errorf("service: partition: rebalance default: %w", err)
		}
		s.logger.Info("partition: rebalanced stranded rows into monthly partitions", "partitions", rebalanced)
	}
	return nil
}
