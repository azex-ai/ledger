package service

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
)

// GlobalSummer sums all debits and credits globally.
type GlobalSummer interface {
	SumGlobalDebitCredit(ctx context.Context) (debit, credit decimal.Decimal, err error)
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
	debit, credit, err := s.global.SumGlobalDebitCredit(ctx)
	if err != nil {
		return nil, fmt.Errorf("service: reconcile: sum global: %w", err)
	}

	gap := debit.Sub(credit)
	balanced := gap.IsZero()

	result := &core.ReconcileResult{
		Balanced:  balanced,
		Gap:       gap,
		CheckedAt: time.Now(),
	}

	if !balanced {
		s.logger.Warn("service: reconcile: accounting equation imbalance",
			"debit_total", debit.String(),
			"credit_total", credit.String(),
			"gap", gap.String(),
		)
		s.metrics.ReconcileGap(0, gap) // currencyID=0 for global
	}

	s.metrics.ReconcileCompleted(balanced)
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

	// For each checkpoint, compute expected balance from entries and compare
	for _, cp := range cps {
		debit := debitByClass[cp.ClassificationID]
		credit := creditByClass[cp.ClassificationID]

		var expected decimal.Decimal
		ns := normalSides[cp.ClassificationID]
		switch ns {
		case core.NormalSideDebit:
			expected = debit.Sub(credit)
		case core.NormalSideCredit:
			expected = credit.Sub(debit)
		default:
			expected = debit.Sub(credit)
		}

		drift := cp.Balance.Sub(expected)
		if !drift.IsZero() {
			result.Balanced = false
			result.Gap = result.Gap.Add(drift.Abs())
			result.Details = append(result.Details, core.ReconcileDetail{
				AccountHolder:    holder,
				CurrencyID:       currencyID,
				ClassificationID: cp.ClassificationID,
				Expected:         expected,
				Actual:           cp.Balance,
				Drift:            drift,
			})

			s.logger.Warn("service: reconcile account: checkpoint drift",
				"holder", holder,
				"currency_id", currencyID,
				"classification_id", cp.ClassificationID,
				"expected", expected.String(),
				"actual", cp.Balance.String(),
				"drift", drift.String(),
			)
			s.metrics.ReconcileGap(currencyID, drift)
		}
	}

	s.metrics.ReconcileCompleted(result.Balanced)
	return result, nil
}
