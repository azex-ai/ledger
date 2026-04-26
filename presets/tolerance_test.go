package presets

import (
	"context"
	"fmt"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

func TestBuildDepositTolerancePlan(t *testing.T) {
	tests := []struct {
		name      string
		expected  decimal.Decimal
		actual    decimal.Decimal
		tolerance decimal.Decimal
		outcome   DepositToleranceOutcome
		manual    bool
		stepCodes []string
	}{
		{
			name:      "exact match",
			expected:  decimal.NewFromInt(100),
			actual:    decimal.NewFromInt(100),
			tolerance: decimal.NewFromInt(1),
			outcome:   DepositToleranceExactMatch,
			stepCodes: []string{"deposit_confirm_pending"},
		},
		{
			name:      "shortfall within tolerance",
			expected:  decimal.NewFromInt(100),
			actual:    decimal.NewFromInt(98),
			tolerance: decimal.NewFromInt(5),
			outcome:   DepositToleranceShortfallAutoReleased,
			stepCodes: []string{"deposit_confirm_pending", "deposit_release_pending"},
		},
		{
			name:      "shortfall beyond tolerance",
			expected:  decimal.NewFromInt(100),
			actual:    decimal.NewFromInt(90),
			tolerance: decimal.NewFromInt(5),
			outcome:   DepositToleranceShortfallPending,
			manual:    true,
			stepCodes: []string{"deposit_confirm_pending"},
		},
		{
			name:      "overage within tolerance",
			expected:  decimal.NewFromInt(100),
			actual:    decimal.NewFromInt(102),
			tolerance: decimal.NewFromInt(5),
			outcome:   DepositToleranceOverageAutoCredited,
			stepCodes: []string{"deposit_confirm_pending", "deposit_confirm"},
		},
		{
			name:      "overage beyond tolerance",
			expected:  decimal.NewFromInt(100),
			actual:    decimal.NewFromInt(110),
			tolerance: decimal.NewFromInt(5),
			outcome:   DepositToleranceOverageRecorded,
			manual:    true,
			stepCodes: []string{"deposit_confirm_pending", "deposit_record_overage"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := BuildDepositTolerancePlan(tt.expected, tt.actual, DepositToleranceConfig{Amount: tt.tolerance})
			require.NoError(t, err)
			assert.Equal(t, tt.outcome, plan.Outcome)
			assert.Equal(t, tt.manual, plan.RequiresManualReview)
			require.Len(t, plan.Steps, len(tt.stepCodes))
			for i, code := range tt.stepCodes {
				assert.Equal(t, code, plan.Steps[i].TemplateCode)
			}
		})
	}
}

func TestExecuteDepositTolerancePlan(t *testing.T) {
	writer := &fakeToleranceJournalWriter{}
	plan, err := BuildDepositTolerancePlan(
		decimal.NewFromInt(100),
		decimal.NewFromInt(102),
		DepositToleranceConfig{Amount: decimal.NewFromInt(5)},
	)
	require.NoError(t, err)

	journals, err := ExecuteDepositTolerancePlan(context.Background(), writer, core.TemplateParams{
		HolderID:       42,
		CurrencyID:     1,
		IdempotencyKey: "dep-42",
		Source:         "deposit",
		Metadata:       map[string]string{"request_id": "req-1"},
	}, plan)
	require.NoError(t, err)
	require.Len(t, journals, 2)
	require.Len(t, writer.calls, 2)

	assert.Equal(t, "deposit_confirm_pending", writer.calls[0].templateCode)
	assert.Equal(t, "dep-42:confirm-pending", writer.calls[0].params.IdempotencyKey)
	assert.Equal(t, "overage_auto_credited", writer.calls[0].params.Metadata["deposit_tolerance_outcome"])
	assert.Equal(t, "req-1", writer.calls[0].params.Metadata["request_id"])

	assert.Equal(t, "deposit_confirm", writer.calls[1].templateCode)
	assert.Equal(t, "dep-42:credit-overage", writer.calls[1].params.IdempotencyKey)
	assert.Equal(t, "2", writer.calls[1].params.Amounts["amount"].String())
	assert.Equal(t, "req-1", writer.calls[1].params.Metadata["request_id"])
}

func TestExecuteDepositTolerancePlan_UsesBatchExecutorWhenAvailable(t *testing.T) {
	writer := &fakeBatchToleranceJournalWriter{}
	plan, err := BuildDepositTolerancePlan(
		decimal.NewFromInt(100),
		decimal.NewFromInt(98),
		DepositToleranceConfig{Amount: decimal.NewFromInt(5)},
	)
	require.NoError(t, err)

	journals, err := ExecuteDepositTolerancePlan(context.Background(), writer, core.TemplateParams{
		HolderID:       42,
		CurrencyID:     1,
		IdempotencyKey: "dep-42",
		Source:         "deposit",
	}, plan)
	require.NoError(t, err)
	require.Len(t, journals, 2)
	require.Len(t, writer.requests, 2)
	assert.Equal(t, "deposit_confirm_pending", writer.requests[0].TemplateCode)
	assert.Equal(t, "dep-42:confirm-pending", writer.requests[0].Params.IdempotencyKey)
	assert.Equal(t, "deposit_release_pending", writer.requests[1].TemplateCode)
	assert.Equal(t, "dep-42:release-shortfall", writer.requests[1].Params.IdempotencyKey)
	assert.Zero(t, len(writer.singleCalls))
}

type fakeToleranceJournalWriter struct {
	calls []fakeToleranceJournalCall
}

type fakeToleranceJournalCall struct {
	templateCode string
	params       core.TemplateParams
}

func (f *fakeToleranceJournalWriter) ExecuteTemplate(_ context.Context, code string, params core.TemplateParams) (*core.Journal, error) {
	f.calls = append(f.calls, fakeToleranceJournalCall{
		templateCode: code,
		params:       params,
	})
	return &core.Journal{
		ID:             int64(len(f.calls)),
		IdempotencyKey: params.IdempotencyKey,
		Metadata:       params.Metadata,
	}, nil
}

func (f *fakeToleranceJournalWriter) PostJournal(context.Context, core.JournalInput) (*core.Journal, error) {
	return nil, nil
}

func (f *fakeToleranceJournalWriter) ReverseJournal(context.Context, int64, string) (*core.Journal, error) {
	return nil, nil
}

type fakeBatchToleranceJournalWriter struct {
	requests    []core.TemplateExecutionRequest
	singleCalls []fakeToleranceJournalCall
}

func (f *fakeBatchToleranceJournalWriter) ExecuteTemplateBatch(_ context.Context, requests []core.TemplateExecutionRequest) ([]*core.Journal, error) {
	f.requests = append(f.requests, requests...)
	journals := make([]*core.Journal, 0, len(requests))
	for i, req := range requests {
		journals = append(journals, &core.Journal{
			ID:             int64(i + 1),
			IdempotencyKey: req.Params.IdempotencyKey,
			Metadata:       req.Params.Metadata,
		})
	}
	return journals, nil
}

func (f *fakeBatchToleranceJournalWriter) ExecuteTemplate(_ context.Context, code string, params core.TemplateParams) (*core.Journal, error) {
	f.singleCalls = append(f.singleCalls, fakeToleranceJournalCall{
		templateCode: code,
		params:       params,
	})
	return nil, fmt.Errorf("unexpected single template execution")
}

func (f *fakeBatchToleranceJournalWriter) PostJournal(context.Context, core.JournalInput) (*core.Journal, error) {
	return nil, nil
}

func (f *fakeBatchToleranceJournalWriter) ReverseJournal(context.Context, int64, string) (*core.Journal, error) {
	return nil, nil
}
