package service

import (
	"context"
	"fmt"
	"time"

	"github.com/azex-ai/ledger/core"
)

// BalanceLister returns all account balances computed from journal entries
// strictly before the cutoff timestamp. Implemented by RollupAdapter.
type BalanceLister interface {
	ListBalancesAt(ctx context.Context, cutoff time.Time) ([]core.Balance, error)
}

// SnapshotBackfillService fills historical snapshot gaps.
// It satisfies core.SnapshotBackfiller.
type SnapshotBackfillService struct {
	lister  BalanceLister
	counter core.SnapshotCountReader
	sparse  core.SparseSnapshotter
	logger  core.Logger
}

// NewSnapshotBackfillService creates a SnapshotBackfillService.
func NewSnapshotBackfillService(
	lister BalanceLister,
	counter core.SnapshotCountReader,
	sparse core.SparseSnapshotter,
	engine *core.Engine,
) *SnapshotBackfillService {
	return &SnapshotBackfillService{
		lister:  lister,
		counter: counter,
		sparse:  sparse,
		logger:  engine.Logger(),
	}
}

// Compile-time interface assertion.
var _ core.SnapshotBackfiller = (*SnapshotBackfillService)(nil)

// BackfillSnapshots fills missing historical snapshots for [fromDate, toDate].
// Errors on individual days are collected but do not abort the remaining days.
func (s *SnapshotBackfillService) BackfillSnapshots(ctx context.Context, fromDate, toDate time.Time) (*core.BackfillResult, error) {
	if fromDate.After(toDate) {
		return nil, fmt.Errorf("service: backfill: fromDate must not be after toDate")
	}

	result := &core.BackfillResult{
		FromDate: fromDate,
		ToDate:   toDate,
		Errors:   []string{},
	}

	current := normalizeDay(fromDate)
	end := normalizeDay(toDate)

	for !current.After(end) {
		result.DaysProcessed++

		created, err := s.backfillSingleDay(ctx, current)
		if err != nil {
			msg := fmt.Sprintf("%s: %v", current.Format("2006-01-02"), err)
			result.Errors = append(result.Errors, msg)
			s.logger.Error("service: backfill: day failed",
				"date", current.Format("2006-01-02"),
				"error", err,
			)
		} else {
			result.SnapshotsCreated += created
		}

		current = current.AddDate(0, 0, 1)
	}

	return result, nil
}

// backfillSingleDay computes historical balances as of end-of-day and writes
// sparse snapshots. Returns the number of rows inserted.
func (s *SnapshotBackfillService) backfillSingleDay(ctx context.Context, date time.Time) (int, error) {
	// Balances as of the exclusive upper-bound (start of next day).
	cutoff := date.AddDate(0, 0, 1)
	balances, err := s.lister.ListBalancesAt(ctx, cutoff)
	if err != nil {
		return 0, fmt.Errorf("service: backfill: list balances at %s: %w",
			cutoff.Format(time.RFC3339), err)
	}

	inserted := 0
	for _, b := range balances {
		snap := core.BalanceSnapshot{
			AccountHolder:    b.AccountHolder,
			CurrencyID:       b.CurrencyID,
			ClassificationID: b.ClassificationID,
			SnapshotDate:     date,
			Balance:          b.Balance,
		}
		ok, err := s.sparse.UpsertSnapshotSparse(ctx, snap)
		if err != nil {
			return inserted, fmt.Errorf("service: backfill: upsert: holder=%d currency=%d class=%d: %w",
				b.AccountHolder, b.CurrencyID, b.ClassificationID, err)
		}
		if ok {
			inserted++
		}
	}
	return inserted, nil
}

// CheckAndBackfillOnStartup is called once at startup to detect and fill gaps.
// It is idempotent: if any snapshots exist, it returns immediately.
func (s *SnapshotBackfillService) CheckAndBackfillOnStartup(ctx context.Context) error {
	count, err := s.counter.CountSnapshots(ctx)
	if err != nil {
		return fmt.Errorf("service: backfill: count snapshots: %w", err)
	}
	if count > 0 {
		s.logger.Info("service: backfill: snapshots already exist, skipping startup backfill",
			"existing", count)
		return nil
	}

	earliest, err := s.counter.EarliestJournalDate(ctx)
	if err != nil {
		return fmt.Errorf("service: backfill: earliest journal date: %w", err)
	}
	if earliest.IsZero() {
		s.logger.Info("service: backfill: no journals found, skipping startup backfill")
		return nil
	}

	yesterday := normalizeDay(time.Now().UTC().AddDate(0, 0, -1))
	fromDate := normalizeDay(earliest)

	if fromDate.After(yesterday) {
		s.logger.Info("service: backfill: earliest journal is today or future, nothing to backfill")
		return nil
	}

	s.logger.Info("service: backfill: starting startup backfill",
		"from", fromDate.Format("2006-01-02"),
		"to", yesterday.Format("2006-01-02"),
	)

	result, err := s.BackfillSnapshots(ctx, fromDate, yesterday)
	if err != nil {
		return fmt.Errorf("service: backfill: startup backfill: %w", err)
	}

	if len(result.Errors) > 0 {
		s.logger.Error("service: backfill: startup backfill completed with errors",
			"days_processed", result.DaysProcessed,
			"snapshots_created", result.SnapshotsCreated,
			"error_count", len(result.Errors),
		)
	} else {
		s.logger.Info("service: backfill: startup backfill completed",
			"days_processed", result.DaysProcessed,
			"snapshots_created", result.SnapshotsCreated,
		)
	}
	return nil
}

// normalizeDay returns t truncated to midnight UTC.
func normalizeDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
