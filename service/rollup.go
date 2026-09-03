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
	DequeueRollupBatch(ctx context.Context, batchSize int) ([]RollupQueueItem, error)
	// MarkRollupProcessed marks the item processed only if claimToken still owns
	// the claim. Returns false (no error) when the claim was lost to a concurrent
	// re-dirty or re-claim, leaving the row pending for its rightful owner.
	MarkRollupProcessed(ctx context.Context, id int64, claimToken time.Time) (bool, error)
	ReleaseRollupClaim(ctx context.Context, id int64, claimToken time.Time) error
	CountPendingRollups(ctx context.Context) (int64, error)
	// CountStuckRollups reports items that exhausted their retry budget
	// (failed_attempts >= 10) and are excluded from CountPendingRollups /
	// DequeueRollupBatch until an operator resets them (B-m10).
	CountStuckRollups(ctx context.Context) (int64, error)
	EnqueueRollup(ctx context.Context, holder, currencyID, classificationID int64) error
}

// CheckpointReadWriter provides checkpoint read/write operations.
type CheckpointReadWriter interface {
	GetCheckpoint(ctx context.Context, holder, currencyID, classificationID int64) (*BalanceCheckpoint, error)
	UpsertCheckpoint(ctx context.Context, cp BalanceCheckpoint) error
}

// EntrySummer sums journal entries for rollup computation.
type EntrySummer interface {
	SumEntriesSince(ctx context.Context, holder, currencyID, sinceEntryID int64) (debitByClass, creditByClass map[int64]decimal.Decimal, maxEntryID int64, maxEntryAt time.Time, err error)
}

// ClassificationDim is one classification's internal dimension row: the
// internal id the entry math is keyed on, plus the uid/code used whenever a
// result crosses into a public shape. Internal ids never leave the service.
type ClassificationDim struct {
	ID         int64
	UID        string
	Code       string
	NormalSide core.NormalSide
}

// ClassificationLister provides classification dimensions and currency
// id<->uid resolution for rollup/reconcile math.
type ClassificationLister interface {
	ClassificationDims(ctx context.Context) ([]ClassificationDim, error)
	CurrencyIDByUID(ctx context.Context, uid string) (int64, error)
	CurrencyUIDByID(ctx context.Context, id int64) (string, error)
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
		// The queue-depth gauges are reported even when there is nothing to
		// dequeue, and that is the case they exist for (2026-09-03 W5).
		// StuckRollups counts items that have exhausted their retries, which
		// DequeueRollupBatch does not hand back -- so a queue that is
		// ENTIRELY stuck dequeues nothing, and returning here left both
		// gauges un-emitted at exactly the moment they were the signal. A
		// gauge that stops being written goes stale rather than going to
		// zero, so a dashboard shows the last healthy value forever.
		// Same shape as the RollupItemFailed note further down: the one
		// branch most worth an alert was the one branch that produced no
		// metric.
		s.reportQueueDepth(ctx)
		return 0, nil
	}

	// Load classifications for normal_side lookup
	clsList, err := s.classifications.ClassificationDims(ctx)
	if err != nil {
		for _, item := range items {
			// ctx may already be cancelled (e.g. shutdown) — release on a
			// detached, short-lived context so the claim doesn't leak until
			// its lease expires. See cleanupContext.
			cleanupCtx, cancel := cleanupContext(ctx)
			// I-M10: counted regardless of whether the release itself
			// succeeded -- a release failing (DB contention) is exactly the
			// moment this signal matters most, and the previous code left
			// the counter flat through it (the one branch most worth an
			// alert was the one branch that produced no metric at all).
			s.metrics.RollupItemFailed()
			if releaseErr := s.queue.ReleaseRollupClaim(cleanupCtx, item.ID, item.ClaimedUntil); releaseErr != nil {
				s.logger.Error("service: rollup: release claim failed",
					"item_id", item.ID,
					"error", releaseErr,
				)
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
			// I-M10: counted regardless of whether the release itself
			// succeeded -- a release failing (DB contention) is exactly the
			// moment this signal matters most, and the previous code left
			// the counter flat through it (the one branch most worth an
			// alert was the one branch that produced no metric at all).
			s.metrics.RollupItemFailed()
			if releaseErr := s.queue.ReleaseRollupClaim(cleanupCtx, item.ID, item.ClaimedUntil); releaseErr != nil {
				s.logger.Error("service: rollup: release claim failed",
					"item_id", item.ID,
					"error", releaseErr,
				)
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

	s.reportQueueDepth(ctx)

	return processed, nil
}

// reportQueueDepth emits the two queue-depth gauges. Called on every tick,
// including one that dequeued nothing.
//
// A read failure leaves the gauge unwritten rather than reporting zero: a
// query that did not answer is not a queue that is empty, and reporting it
// as empty is the shape working-agreements.md §3 forbids.
func (s *RollupService) reportQueueDepth(ctx context.Context) {
	if pending, err := s.queue.CountPendingRollups(ctx); err == nil {
		s.metrics.PendingRollups(pending)
	}
	// Stuck is a distinct gauge from pending (B-m10): pending clears as the
	// queue drains, stuck never will without an operator resetting the item
	// (see cmd/ledger-cli's rollup reset-claim).
	if stuck, err := s.queue.CountStuckRollups(ctx); err == nil {
		s.metrics.StuckRollups(stuck)
	}
}

func (s *RollupService) processItem(
	ctx context.Context,
	item RollupQueueItem,
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

	// Compute delta respecting normal_side via core.Delta — the sole
	// authority for this computation (I-43). Unknown normal_side is fatal —
	// silently treating it as debit-normal would corrupt the checkpoint and is
	// a class of bug that has happened before. The caller releases the rollup
	// queue claim so the item retries on the next batch.
	debit := debitByClass[item.ClassificationID]
	credit := creditByClass[item.ClassificationID]

	ns := normalSides[item.ClassificationID]
	delta, err := core.Delta(ns, debit, credit)
	if err != nil {
		return fmt.Errorf("service: rollup: classification %d: %w", item.ClassificationID, err)
	}

	newBalance := currentBalance.Add(delta)

	// Detect drift: if we had a checkpoint, check for unexpected drift
	if cp != nil && !delta.IsZero() {
		classCode := classCodeMap[item.ClassificationID]
		s.metrics.CheckpointAge(classCode, time.Since(cp.UpdatedAt))

		// H-M9: core.Metrics labels currency by uid, never the internal
		// currencies.id -- resolved once here, only on this (already rare:
		// checkpoint exists AND balance moved) path, not on every item.
		currencyUID, currencyUIDErr := s.classifications.CurrencyUIDByID(ctx, item.CurrencyID)
		if currencyUIDErr != nil {
			s.logger.Warn("service: rollup: resolve currency uid for metrics failed",
				"currency_id", item.CurrencyID,
				"error", currencyUIDErr,
			)
		}

		// BalanceDrift reports the same point-in-time-for-the-item-currently-
		// being-processed semantics as CheckpointAge just above: a reading for
		// THIS item, not an aggregate across every holder sharing this (class,
		// currency) label (the label deliberately omits holder to keep
		// cardinality bounded -- splitting this into a per-holder or a
		// separately-named metric is a larger redesign left for a future
		// change, not this fix).
		//
		// The interface documents this gauge as "Drift between expected and
		// actual balance" (core.Metrics.BalanceDrift), but the previous
		// implementation passed newBalance itself -- the account's balance,
		// not a drift -- and only ever called this on the violation branch,
		// so the gauge could set but never clear: once any account under this
		// label went negative, the series stayed pinned at that stale
		// negative reading forever (surviving even after the violation was
		// fixed), indistinguishable on a dashboard from "still broken"
		// (working-agreements §3). Reporting the actual drift-from-the-
		// zero-floor -- 0 when this item is healthy, the shortfall's
		// magnitude when it is not -- both matches the documented semantics
		// and gives the metric a value it can return to.
		drift := decimal.Zero
		if newBalance.IsNegative() && ns == core.NormalSideDebit {
			// Expected floor is 0 (debit-normal balances must not go
			// negative); drift is the distance below it, reported as a
			// positive magnitude.
			drift = newBalance.Neg()
			s.logger.Warn("service: rollup: negative balance on debit-normal account",
				"holder", item.AccountHolder,
				"currency_id", item.CurrencyID,
				"classification", classCode,
				"balance", newBalance.String(),
			)
			// M-3 fix (I-41 point 3): BalanceDrift's Gauge is labelled
			// (class, currency) without holder, so a healthy item for a
			// DIFFERENT holder sharing this label can Set it back to zero
			// right after this one reports the violation -- the very next
			// line below does exactly that for a healthy item, by design,
			// so the violation this run just found would otherwise become
			// invisible the moment any other holder's item in the same
			// bucket is processed. NegativeBalanceDetected is monotonic and
			// cannot be masked that way; it is the signal to alert on.
			s.metrics.NegativeBalanceDetected(classCode, currencyUID)
		}
		s.metrics.BalanceDrift(classCode, currencyUID, drift)
	}

	// Upsert checkpoint
	if err := s.checkpoints.UpsertCheckpoint(ctx, BalanceCheckpoint{
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
