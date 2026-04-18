package service

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
)

// RollupQueuer provides rollup queue read/write operations.
type RollupQueuer interface {
	DequeueRollupBatch(ctx context.Context, batchSize int) ([]core.RollupQueueItem, error)
	MarkRollupProcessed(ctx context.Context, id int64) error
	CountPendingRollups(ctx context.Context) (int64, error)
}

// CheckpointReadWriter provides checkpoint read/write operations.
type CheckpointReadWriter interface {
	GetCheckpoint(ctx context.Context, holder, currencyID, classificationID int64) (*core.BalanceCheckpoint, error)
	UpsertCheckpoint(ctx context.Context, cp core.BalanceCheckpoint) error
}

// EntrySummer sums journal entries for rollup computation.
type EntrySummer interface {
	SumEntriesSince(ctx context.Context, holder, currencyID, sinceEntryID int64) (debitByClass, creditByClass map[int64]decimal.Decimal, maxEntryID int64, maxEntryAt time.Time, err error)
}

// ClassificationLister lists classifications for normal_side lookup.
type ClassificationLister interface {
	ListClassifications(ctx context.Context, activeOnly bool) ([]core.Classification, error)
}

// RollupService processes the rollup queue to materialize balance checkpoints.
type RollupService struct {
	queue           RollupQueuer
	checkpoints     CheckpointReadWriter
	entries         EntrySummer
	classifications ClassificationLister
	logger          core.Logger
	metrics         core.Metrics
}

// NewRollupService creates a new RollupService.
func NewRollupService(
	queue RollupQueuer,
	checkpoints CheckpointReadWriter,
	entries EntrySummer,
	classifications ClassificationLister,
	engine *core.Engine,
) *RollupService {
	return &RollupService{
		queue:           queue,
		checkpoints:     checkpoints,
		entries:         entries,
		classifications: classifications,
		logger:          engine.Logger(),
		metrics:         engine.Metrics(),
	}
}

// ProcessBatch dequeues up to batchSize items and processes each rollup.
// Returns the number of items processed.
func (s *RollupService) ProcessBatch(ctx context.Context, batchSize int) (int, error) {
	start := time.Now()

	items, err := s.queue.DequeueRollupBatch(ctx, batchSize)
	if err != nil {
		return 0, fmt.Errorf("service: rollup: dequeue batch: %w", err)
	}

	if len(items) == 0 {
		return 0, nil
	}

	// Load classifications for normal_side lookup
	clsList, err := s.classifications.ListClassifications(ctx, false)
	if err != nil {
		return 0, fmt.Errorf("service: rollup: list classifications: %w", err)
	}
	normalSides := make(map[int64]core.NormalSide, len(clsList))
	classCodeMap := make(map[int64]string, len(clsList))
	for _, c := range clsList {
		normalSides[c.ID] = c.NormalSide
		classCodeMap[c.ID] = c.Code
	}

	processed := 0
	for _, item := range items {
		if err := s.processItem(ctx, item, normalSides, classCodeMap); err != nil {
			s.logger.Error("service: rollup: process item failed",
				"item_id", item.ID,
				"holder", item.AccountHolder,
				"currency_id", item.CurrencyID,
				"classification_id", item.ClassificationID,
				"error", err,
			)
			continue
		}
		processed++
	}

	s.metrics.RollupProcessed(processed)
	s.metrics.RollupLatency(time.Since(start))

	// Report pending count
	pending, err := s.queue.CountPendingRollups(ctx)
	if err == nil {
		s.metrics.PendingRollups(pending)
	}

	return processed, nil
}

func (s *RollupService) processItem(
	ctx context.Context,
	item core.RollupQueueItem,
	normalSides map[int64]core.NormalSide,
	classCodeMap map[int64]string,
) error {
	// Get current checkpoint
	cp, err := s.checkpoints.GetCheckpoint(ctx, item.AccountHolder, item.CurrencyID, item.ClassificationID)
	if err != nil {
		return fmt.Errorf("service: rollup: get checkpoint: %w", err)
	}

	var currentBalance decimal.Decimal
	var sinceEntryID int64
	if cp != nil {
		currentBalance = cp.Balance
		sinceEntryID = cp.LastEntryID
	}

	// Sum entries since the last checkpoint
	debitByClass, creditByClass, maxEntryID, maxEntryAt, err := s.entries.SumEntriesSince(
		ctx, item.AccountHolder, item.CurrencyID, sinceEntryID,
	)
	if err != nil {
		return fmt.Errorf("service: rollup: sum entries: %w", err)
	}

	// No new entries
	if maxEntryID == 0 || maxEntryID <= sinceEntryID {
		if err := s.queue.MarkRollupProcessed(ctx, item.ID); err != nil {
			return fmt.Errorf("service: rollup: mark processed: %w", err)
		}
		return nil
	}

	// Compute delta respecting normal_side
	debit := debitByClass[item.ClassificationID]
	credit := creditByClass[item.ClassificationID]

	var delta decimal.Decimal
	ns := normalSides[item.ClassificationID]
	switch ns {
	case core.NormalSideDebit:
		delta = debit.Sub(credit)
	case core.NormalSideCredit:
		delta = credit.Sub(debit)
	default:
		delta = debit.Sub(credit)
	}

	newBalance := currentBalance.Add(delta)

	// Detect drift: if we had a checkpoint, check for unexpected drift
	if cp != nil && !delta.IsZero() {
		classCode := classCodeMap[item.ClassificationID]
		s.metrics.CheckpointAge(classCode, time.Since(cp.UpdatedAt))

		// If balance went negative for a debit-normal account, that's suspicious
		if newBalance.IsNegative() && ns == core.NormalSideDebit {
			s.logger.Warn("service: rollup: negative balance on debit-normal account",
				"holder", item.AccountHolder,
				"currency_id", item.CurrencyID,
				"classification", classCode,
				"balance", newBalance.String(),
			)
			s.metrics.BalanceDrift(classCode, item.CurrencyID, newBalance)
		}
	}

	// Upsert checkpoint
	if err := s.checkpoints.UpsertCheckpoint(ctx, core.BalanceCheckpoint{
		AccountHolder:    item.AccountHolder,
		CurrencyID:       item.CurrencyID,
		ClassificationID: item.ClassificationID,
		Balance:          newBalance,
		LastEntryID:      maxEntryID,
		LastEntryAt:      maxEntryAt,
	}); err != nil {
		return fmt.Errorf("service: rollup: upsert checkpoint: %w", err)
	}

	// Mark processed
	if err := s.queue.MarkRollupProcessed(ctx, item.ID); err != nil {
		return fmt.Errorf("service: rollup: mark processed: %w", err)
	}

	return nil
}
