package presets

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
)

type DepositToleranceOutcome string

const (
	DepositToleranceExactMatch            DepositToleranceOutcome = "exact_match"
	DepositToleranceShortfallAutoReleased DepositToleranceOutcome = "shortfall_auto_released"
	DepositToleranceShortfallPending      DepositToleranceOutcome = "shortfall_pending_review"
	DepositToleranceOverageAutoCredited   DepositToleranceOutcome = "overage_auto_credited"
	DepositToleranceOverageRecorded       DepositToleranceOutcome = "overage_recorded_for_review"
)

type DepositToleranceConfig struct {
	Amount decimal.Decimal
}

type TemplateExecution struct {
	TemplateCode      string
	IdempotencySuffix string
	Amounts           map[string]decimal.Decimal
}

type DepositTolerancePlan struct {
	ExpectedAmount       decimal.Decimal
	ActualAmount         decimal.Decimal
	ToleranceAmount      decimal.Decimal
	Delta                decimal.Decimal
	Outcome              DepositToleranceOutcome
	RequiresManualReview bool
	Steps                []TemplateExecution
}

func (c DepositToleranceConfig) Validate() error {
	if c.Amount.IsNegative() {
		return fmt.Errorf("presets: tolerance amount must not be negative: %w", core.ErrInvalidInput)
	}
	return nil
}

func BuildDepositTolerancePlan(expected, actual decimal.Decimal, cfg DepositToleranceConfig) (*DepositTolerancePlan, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !expected.IsPositive() {
		return nil, fmt.Errorf("presets: expected amount must be positive: %w", core.ErrInvalidInput)
	}
	if actual.IsNegative() {
		return nil, fmt.Errorf("presets: actual amount must not be negative: %w", core.ErrInvalidInput)
	}

	plan := &DepositTolerancePlan{
		ExpectedAmount:  expected,
		ActualAmount:    actual,
		ToleranceAmount: cfg.Amount,
		Delta:           expected.Sub(actual).Abs(),
	}

	switch expected.Cmp(actual) {
	case 0:
		plan.Outcome = DepositToleranceExactMatch
		plan.Steps = append(plan.Steps, TemplateExecution{
			TemplateCode:      "deposit_confirm_pending",
			IdempotencySuffix: "confirm-pending",
			Amounts:           map[string]decimal.Decimal{"amount": expected},
		})
	case 1:
		confirmed := actual
		if confirmed.IsPositive() {
			plan.Steps = append(plan.Steps, TemplateExecution{
				TemplateCode:      "deposit_confirm_pending",
				IdempotencySuffix: "confirm-pending",
				Amounts:           map[string]decimal.Decimal{"amount": confirmed},
			})
		}
		shortfall := expected.Sub(actual)
		if shortfall.LessThanOrEqual(cfg.Amount) {
			plan.Outcome = DepositToleranceShortfallAutoReleased
			if shortfall.IsPositive() {
				plan.Steps = append(plan.Steps, TemplateExecution{
					TemplateCode:      "deposit_release_pending",
					IdempotencySuffix: "release-shortfall",
					Amounts:           map[string]decimal.Decimal{"amount": shortfall},
				})
			}
		} else {
			plan.Outcome = DepositToleranceShortfallPending
			plan.RequiresManualReview = true
		}
	case -1:
		plan.Steps = append(plan.Steps, TemplateExecution{
			TemplateCode:      "deposit_confirm_pending",
			IdempotencySuffix: "confirm-pending",
			Amounts:           map[string]decimal.Decimal{"amount": expected},
		})
		overage := actual.Sub(expected)
		if overage.LessThanOrEqual(cfg.Amount) {
			plan.Outcome = DepositToleranceOverageAutoCredited
			plan.Steps = append(plan.Steps, TemplateExecution{
				TemplateCode:      "deposit_confirm",
				IdempotencySuffix: "credit-overage",
				Amounts:           map[string]decimal.Decimal{"amount": overage},
			})
		} else {
			plan.Outcome = DepositToleranceOverageRecorded
			plan.RequiresManualReview = true
			plan.Steps = append(plan.Steps, TemplateExecution{
				TemplateCode:      "deposit_record_overage",
				IdempotencySuffix: "record-overage",
				Amounts:           map[string]decimal.Decimal{"amount": overage},
			})
		}
	}

	return plan, nil
}

func ExecuteDepositTolerancePlan(
	ctx context.Context,
	writer core.JournalWriter,
	base core.TemplateParams,
	plan *DepositTolerancePlan,
) ([]*core.Journal, error) {
	if writer == nil {
		return nil, fmt.Errorf("presets: journal writer is nil: %w", core.ErrInvalidInput)
	}
	if plan == nil {
		return nil, fmt.Errorf("presets: tolerance plan is nil: %w", core.ErrInvalidInput)
	}
	if base.HolderID == 0 {
		return nil, fmt.Errorf("presets: holder_id required: %w", core.ErrInvalidInput)
	}
	if base.CurrencyID <= 0 {
		return nil, fmt.Errorf("presets: currency_id must be positive: %w", core.ErrInvalidInput)
	}
	if base.IdempotencyKey == "" {
		return nil, fmt.Errorf("presets: idempotency key required: %w", core.ErrInvalidInput)
	}

	journals := make([]*core.Journal, 0, len(plan.Steps))
	requests := make([]core.TemplateExecutionRequest, 0, len(plan.Steps))
	for _, step := range plan.Steps {
		requests = append(requests, core.TemplateExecutionRequest{
			TemplateCode: step.TemplateCode,
			Params: core.TemplateParams{
				HolderID:       base.HolderID,
				CurrencyID:     base.CurrencyID,
				IdempotencyKey: fmt.Sprintf("%s:%s", base.IdempotencyKey, step.IdempotencySuffix),
				Amounts:        copyDecimalMap(step.Amounts),
				ActorID:        base.ActorID,
				Source:         base.Source,
				Metadata:       buildToleranceMetadata(base.Metadata, step.TemplateCode, plan),
			},
		})
	}

	if batchWriter, ok := writer.(core.TemplateBatchExecutor); ok {
		return batchWriter.ExecuteTemplateBatch(ctx, requests)
	}

	for _, req := range requests {
		journal, err := writer.ExecuteTemplate(ctx, req.TemplateCode, req.Params)
		if err != nil {
			return journals, fmt.Errorf("presets: execute tolerance step %q: %w", req.TemplateCode, err)
		}
		journals = append(journals, journal)
	}

	return journals, nil
}

func buildToleranceMetadata(base map[string]string, step string, plan *DepositTolerancePlan) map[string]string {
	metadata := make(map[string]string, len(base)+6)
	for k, v := range base {
		metadata[k] = v
	}
	metadata["deposit_tolerance_step"] = step
	metadata["deposit_tolerance_outcome"] = string(plan.Outcome)
	metadata["deposit_tolerance_expected"] = plan.ExpectedAmount.String()
	metadata["deposit_tolerance_actual"] = plan.ActualAmount.String()
	metadata["deposit_tolerance_delta"] = plan.Delta.String()
	metadata["deposit_tolerance_limit"] = plan.ToleranceAmount.String()
	return metadata
}

func copyDecimalMap(in map[string]decimal.Decimal) map[string]decimal.Decimal {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]decimal.Decimal, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
