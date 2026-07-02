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
	// MarkRollupProcessed marks the item processed only if claimToken still owns
	// the claim. Returns false (no error) when the claim was lost to a concurrent
	// re-dirty or re-claim, leaving the row pending for its rightful owner.
	MarkRollupProcessed(ctx context.Context, id int64, claimToken time.Time) (bool, error)
	ReleaseRollupClaim(ctx context.Context, id int64, claimToken time.Time) error
	CountPendingRollups(ctx context.Context) (int64, error)
	EnqueueRollup(ctx context.Context, holder, currencyID, classificationID int64) error
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
		for _, item := range items {
			// ctx may already be cancelled (e.g. shutdown) — release on a
			// detached, short-lived context so the claim doesn't leak until
			// its lease expires. See cleanupContext.
			cleanupCtx, cancel := cleanupContext(ctx)
			if releaseErr := s.queue.ReleaseRollupClaim(cleanupCtx, item.ID, item.ClaimedUntil); releaseErr != nil {
				s.logger.Error("service: rollup: release claim failed",
					"item_id", item.ID,
					"error", releaseErr,
				)
			} else {
				s.metrics.RollupItemFailed()
			}
			cancel()
		}
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
			cleanupCtx, cancel := cleanupContext(ctx)
			if releaseErr := s.queue.ReleaseRollupClaim(cleanupCtx, item.ID, item.ClaimedUntil); releaseErr != nil {
				s.logger.Error("service: rollup: release claim failed",
					"item_id", item.ID,
					"error", releaseErr,
				)
			} else {
				s.metrics.RollupItemFailed()
			}
			cancel()
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
		// No checkpoint write happened; if the claim was lost (marked=false) the
		// rightful owner will reprocess, so there is nothing more to do either way.
		if _, err := s.queue.MarkRollupProcessed(ctx, item.ID, item.ClaimedUntil); err != nil {
			return fmt.Errorf("service: rollup: mark processed: %w", err)
		}
		return nil
	}

	// Compute delta respecting normal_side. Unknown normal_side is fatal —
	// silently treating it as debit-normal would corrupt the checkpoint and is
	// a class of bug that has happened before. The caller releases the rollup
	// queue claim so the item retries on the next batch.
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
		return fmt.Errorf("service: rollup: unknown normal_side %q for classification %d: %w", ns, item.ClassificationID, core.ErrInvalidInput)
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

	// Mark processed (claim-token scoped). If the claim was lost to a concurrent
	// re-dirty or re-claim, marked is false: our checkpoint upsert above was still
	// valid (it is monotonic), but the rightful owner will reprocess this row, so
	// we skip the coalesced-enqueue recovery and return without releasing (the
	// caller only releases on a returned error, which we must not raise here).
	marked, err := s.queue.MarkRollupProcessed(ctx, item.ID, item.ClaimedUntil)
	if err != nil {
		return fmt.Errorf("service: rollup: mark processed: %w", err)
	}
	if !marked {
		return nil
	}

	// Recover a coalesced enqueue. A journal for THIS (holder, currency,
	// classification) that committed after our snapshot but before
	// MarkRollupProcessed above had its EnqueueRollup suppressed by the still
	// -pending queue row (the partial unique index is per dimension). Now that
	// the row is processed, re-read entries past the checkpoint we just wrote;
	// if this classification has any, re-enqueue so the next batch materializes
	// them. Must run AFTER MarkRollupProcessed, else this re-enqueue would be
	// coalesced away too. Balance reads stay correct meanwhile (the delta covers
	// id > last_entry_id); this only keeps the checkpoint from lagging.
	//
	// This whole stage is best-effort: the checkpoint is already committed and
	// MarkRollupProcessed has succeeded, so the rollup itself is done. If we
	// returned an error here, ProcessBatch would call ReleaseRollupClaim, whose
	// `processed_at IS NULL` guard no longer matches — the item would be logged
	// as failed while actually being done, orphaned with failed_attempts never
	// bumped. So a re-check failure is logged and swallowed; the coalesced entry
	// stays unmaterialized only until the next journal for this dimension, and
	// balances remain correct via the delta path meanwhile.
	freshDebit, freshCredit, _, _, err := s.entries.SumEntriesSince(ctx, item.AccountHolder, item.CurrencyID, maxEntryID)
	if err != nil {
		s.logger.Warn("service: rollup: recheck entries after processing failed",
			"holder", item.AccountHolder,
			"currency_id", item.CurrencyID,
			"classification_id", item.ClassificationID,
			"error", err,
		)
		return nil
	}
	_, hasDebit := freshDebit[item.ClassificationID]
	_, hasCredit := freshCredit[item.ClassificationID]
	if hasDebit || hasCredit {
		if err := s.queue.EnqueueRollup(ctx, item.AccountHolder, item.CurrencyID, item.ClassificationID); err != nil {
			// Best-effort catch-up: the checkpoint is already correctly written
			// and balances stay correct via the delta. A failed re-enqueue only
			// delays re-materialization until the next journal for this
			// dimension, so log it rather than failing the successful rollup.
			s.logger.Warn("service: rollup: re-enqueue after coalesced enqueue failed",
				"holder", item.AccountHolder,
				"currency_id", item.CurrencyID,
				"classification_id", item.ClassificationID,
				"error", err,
			)
		}
	}

	return nil
}
