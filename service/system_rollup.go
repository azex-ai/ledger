package service

import (
	"context"
	"fmt"
	"time"

	"github.com/azex-ai/ledger/core"
)

// CheckpointAggregator aggregates checkpoints by (currency_id, classification_id).
type CheckpointAggregator interface {
	AggregateCheckpointsByClassification(ctx context.Context) ([]core.SystemRollup, error)
}

// SystemRollupWriter upserts system rollup records.
type SystemRollupWriter interface {
	UpsertSystemRollup(ctx context.Context, rollup core.SystemRollup) error
}

// SystemRollupService aggregates balance_checkpoints into system_rollups for O(1) queries.
type SystemRollupService struct {
	aggregator CheckpointAggregator
	writer     SystemRollupWriter
	logger     core.Logger
	metrics    core.Metrics
}

// NewSystemRollupService creates a new SystemRollupService.
func NewSystemRollupService(
	aggregator CheckpointAggregator,
	writer SystemRollupWriter,
	engine *core.Engine,
) *SystemRollupService {
	return &SystemRollupService{
		aggregator: aggregator,
		writer:     writer,
		logger:     engine.Logger(),
		metrics:    engine.Metrics(),
	}
}

// RefreshSystemRollups aggregates all balance_checkpoints by (currency_id, classification_id)
// and upserts into system_rollups.
func (s *SystemRollupService) RefreshSystemRollups(ctx context.Context) error {
	start := time.Now()

	rollups, err := s.aggregator.AggregateCheckpointsByClassification(ctx)
	if err != nil {
		return fmt.Errorf("service: system rollup: aggregate: %w", err)
	}

	for _, r := range rollups {
		r.UpdatedAt = time.Now()
		if err := s.writer.UpsertSystemRollup(ctx, r); err != nil {
			return fmt.Errorf("service: system rollup: upsert currency=%d class=%d: %w",
				r.CurrencyID, r.ClassificationID, err)
		}
	}

	s.logger.Info("service: system rollup: refreshed",
		"count", len(rollups),
		"duration", time.Since(start).String(),
	)

	return nil
}

