package service

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
)

type CurrencyReconcileTotals struct {
	CurrencyID int64
	Debit      decimal.Decimal
	Credit     decimal.Decimal
}

// GlobalSummer sums all debits and credits globally, grouped by currency.
type GlobalSummer interface {
	SumGlobalDebitCreditByCurrency(ctx context.Context) ([]CurrencyReconcileTotals, error)
}

// AccountEntrySummer sums all entries for a specific account (no checkpoint filter).
type AccountEntrySummer interface {
	SumEntriesByAccountClassification(ctx context.Context, holder, currencyID int64) (debitByClass, creditByClass map[int64]decimal.Decimal, err error)
}

// CheckpointReader reads checkpoints for reconciliation.
type CheckpointReader interface {
	GetCheckpoints(ctx context.Context, holder, currencyID int64) ([]core.BalanceCheckpoint, error)
}

// ReconciliationService verifies accounting integrity.
type ReconciliationService struct {
	global          GlobalSummer
	accountEntries  AccountEntrySummer
	checkpoints     CheckpointReader
	classifications ClassificationLister
	logger          core.Logger
	metrics         core.Metrics
}

// NewReconciliationService creates a new ReconciliationService.
func NewReconciliationService(
	global GlobalSummer,
	accountEntries AccountEntrySummer,
	checkpoints CheckpointReader,
	classifications ClassificationLister,
	engine *core.Engine,
) *ReconciliationService {
	return &ReconciliationService{
		global:          global,
		accountEntries:  accountEntries,
		checkpoints:     checkpoints,
		classifications: classifications,
		logger:          engine.Logger(),
		metrics:         engine.Metrics(),
	}
}

// CheckAccountingEquation verifies SUM(all debits) == SUM(all credits).
func (s *ReconciliationService) CheckAccountingEquation(ctx context.Context) (*core.ReconcileResult, error) {
	totals, err := s.global.SumGlobalDebitCreditByCurrency(ctx)
	if err != nil {
		return nil, fmt.Errorf("service: reconcile: sum global: %w", err)
	}

	result := &core.ReconcileResult{
		Balanced:  true,
		Gap:       decimal.Zero,
		CheckedAt: time.Now(),
	}

	for _, total := range totals {
		gap := total.Debit.Sub(total.Credit)
		if gap.IsZero() {
			continue
		}

		result.Balanced = false
		result.Gap = result.Gap.Add(gap.Abs())
		result.Details = append(result.Details, core.ReconcileDetail{
			CurrencyID: total.CurrencyID,
			Expected:   total.Debit,
			Actual:     total.Credit,
			Drift:      gap,
		})

		s.logger.Warn("service: reconcile: accounting equation imbalance",
			"currency_id", total.CurrencyID,
			"debit_total", total.Debit.String(),
			"credit_total", total.Credit.String(),
			"gap", gap.String(),
		)
		s.metrics.ReconcileGap(total.CurrencyID, gap)
	}

	s.metrics.ReconcileCompleted(result.Balanced)
	return result, nil
}

// ReconcileAccount verifies checkpoint balances vs actual entry sums for a specific account.
func (s *ReconciliationService) ReconcileAccount(ctx context.Context, holder int64, currencyID int64) (*core.ReconcileResult, error) {
	// Get classifications for normal_side
	clsList, err := s.classifications.ListClassifications(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("service: reconcile account: list classifications: %w", err)
	}
	normalSides := make(map[int64]core.NormalSide, len(clsList))
	for _, c := range clsList {
		normalSides[c.ID] = c.NormalSide
	}

	// Get checkpoints
	cps, err := s.checkpoints.GetCheckpoints(ctx, holder, currencyID)
	if err != nil {
		return nil, fmt.Errorf("service: reconcile account: get checkpoints: %w", err)
	}

	// Get actual entry sums
	debitByClass, creditByClass, err := s.accountEntries.SumEntriesByAccountClassification(ctx, holder, currencyID)
	if err != nil {
		return nil, fmt.Errorf("service: reconcile account: sum entries: %w", err)
	}

	result := &core.ReconcileResult{
		Balanced:  true,
		Gap:       decimal.Zero,
		CheckedAt: time.Now(),
	}

	checkpointByClass := make(map[int64]core.BalanceCheckpoint, len(cps))
	classificationSet := make(map[int64]struct{}, len(cps)+len(debitByClass)+len(creditByClass))
	for _, cp := range cps {
		checkpointByClass[cp.ClassificationID] = cp
		classificationSet[cp.ClassificationID] = struct{}{}
	}
	for classID := range debitByClass {
		classificationSet[classID] = struct{}{}
	}
	for classID := range creditByClass {
		classificationSet[classID] = struct{}{}
	}

	classificationIDs := make([]int64, 0, len(classificationSet))
	for classID := range classificationSet {
		classificationIDs = append(classificationIDs, classID)
	}
	sort.Slice(classificationIDs, func(i, j int) bool { return classificationIDs[i] < classificationIDs[j] })

	// For each classification referenced by either checkpoints or entries, compute the
	// expected balance from entries and compare it to the checkpointed balance.
	for _, classID := range classificationIDs {
		debit := debitByClass[classID]
		credit := creditByClass[classID]

		var expected decimal.Decimal
		ns := normalSides[classID]
		switch ns {
		case core.NormalSideDebit:
			expected = debit.Sub(credit)
		case core.NormalSideCredit:
			expected = credit.Sub(debit)
		default:
			return nil, fmt.Errorf("service: reconcile account: unknown normal_side %q for classification %d: %w", ns, classID, core.ErrInvalidInput)
		}

		actual := decimal.Zero
		if cp, ok := checkpointByClass[classID]; ok {
			actual = cp.Balance
		}

		drift := actual.Sub(expected)
		if !drift.IsZero() {
			result.Balanced = false
			result.Gap = result.Gap.Add(drift.Abs())
			result.Details = append(result.Details, core.ReconcileDetail{
				AccountHolder:    holder,
				CurrencyID:       currencyID,
				ClassificationID: classID,
				Expected:         expected,
				Actual:           actual,
				Drift:            drift,
			})

			s.logger.Warn("service: reconcile account: checkpoint drift",
				"holder", holder,
				"currency_id", currencyID,
				"classification_id", classID,
				"expected", expected.String(),
				"actual", actual.String(),
				"drift", drift.String(),
			)
			s.metrics.ReconcileGap(currencyID, drift)
		}
	}

	s.metrics.ReconcileCompleted(result.Balanced)
	return result, nil
}
