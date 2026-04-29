package service

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

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

// PlatformBalanceQuerier reads raw platform balance rows from the store.
// Implemented by postgres.PlatformBalanceStore.
type PlatformBalanceQuerier interface {
	GetPlatformBalances(ctx context.Context, currencyID int64) (*core.PlatformBalance, error)
	GetTotalLiabilityByAsset(ctx context.Context, currencyID int64) (decimal.Decimal, error)
	SolvencyCheck(ctx context.Context, currencyID int64) (*core.SolvencyReport, error)
}

// SystemRollupService aggregates balance_checkpoints into system_rollups for O(1) queries.
type SystemRollupService struct {
	aggregator CheckpointAggregator
	writer     SystemRollupWriter
	pbQuerier  PlatformBalanceQuerier
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

// WithPlatformBalanceQuerier attaches a querier for the platform-balance read
// API. Call this after NewSystemRollupService when you need GetPlatformBalances,
// GetTotalLiabilityByAsset, or SolvencyCheck.
func (s *SystemRollupService) WithPlatformBalanceQuerier(q PlatformBalanceQuerier) *SystemRollupService {
	s.pbQuerier = q
	return s
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

// GetPlatformBalances returns a structured per-classification breakdown for the
// given currency. See core.PlatformBalance for field semantics.
// Returns an error if WithPlatformBalanceQuerier has not been called.
func (s *SystemRollupService) GetPlatformBalances(ctx context.Context, currencyID int64) (*core.PlatformBalance, error) {
	if s.pbQuerier == nil {
		return nil, fmt.Errorf("service: system rollup: platform balance querier not configured")
	}
	pb, err := s.pbQuerier.GetPlatformBalances(ctx, currencyID)
	if err != nil {
		return nil, fmt.Errorf("service: system rollup: get platform balances currency=%d: %w", currencyID, err)
	}
	return pb, nil
}

// GetTotalLiabilityByAsset returns the sum of all user-side (holder > 0) balances
// across all classifications for the given currency.
// Returns an error if WithPlatformBalanceQuerier has not been called.
func (s *SystemRollupService) GetTotalLiabilityByAsset(ctx context.Context, currencyID int64) (decimal.Decimal, error) {
	if s.pbQuerier == nil {
		return decimal.Zero, fmt.Errorf("service: system rollup: platform balance querier not configured")
	}
	total, err := s.pbQuerier.GetTotalLiabilityByAsset(ctx, currencyID)
	if err != nil {
		return decimal.Zero, fmt.Errorf("service: system rollup: total liability currency=%d: %w", currencyID, err)
	}
	return total, nil
}

// SolvencyCheck computes a solvency report for the given currency.
// See core.SolvencyReport for field semantics.
// Returns an error if WithPlatformBalanceQuerier has not been called.
func (s *SystemRollupService) SolvencyCheck(ctx context.Context, currencyID int64) (*core.SolvencyReport, error) {
	if s.pbQuerier == nil {
		return nil, fmt.Errorf("service: system rollup: platform balance querier not configured")
	}
	report, err := s.pbQuerier.SolvencyCheck(ctx, currencyID)
	if err != nil {
		return nil, fmt.Errorf("service: system rollup: solvency check currency=%d: %w", currencyID, err)
	}
	return report, nil
}
