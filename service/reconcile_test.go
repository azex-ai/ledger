package service

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// --- Mocks ---

type mockGlobalSummer struct {
	debit, credit decimal.Decimal
}

func (m *mockGlobalSummer) SumGlobalDebitCredit(_ context.Context) (decimal.Decimal, decimal.Decimal, error) {
	return m.debit, m.credit, nil
}

type mockAccountEntrySummer struct {
	debitByClass  map[int64]decimal.Decimal
	creditByClass map[int64]decimal.Decimal
}

func (m *mockAccountEntrySummer) SumEntriesByAccountClassification(_ context.Context, _, _ int64) (map[int64]decimal.Decimal, map[int64]decimal.Decimal, error) {
	return m.debitByClass, m.creditByClass, nil
}

type mockCheckpointReader struct {
	checkpoints []core.BalanceCheckpoint
}

func (m *mockCheckpointReader) GetCheckpoints(_ context.Context, _, _ int64) ([]core.BalanceCheckpoint, error) {
	return m.checkpoints, nil
}

// --- Tests ---

func TestReconciliationService_BalancedSystem(t *testing.T) {
	global := &mockGlobalSummer{
		debit:  decimal.NewFromInt(1000),
		credit: decimal.NewFromInt(1000),
	}
	engine := core.NewEngine()
	svc := NewReconciliationService(global, nil, nil, nil, engine)

	result, err := svc.CheckAccountingEquation(context.Background())
	require.NoError(t, err)
	assert.True(t, result.Balanced)
	assert.True(t, result.Gap.IsZero())
}

func TestReconciliationService_Imbalanced(t *testing.T) {
	global := &mockGlobalSummer{
		debit:  decimal.NewFromInt(1000),
		credit: decimal.NewFromInt(999),
	}
	engine := core.NewEngine()
	svc := NewReconciliationService(global, nil, nil, nil, engine)

	result, err := svc.CheckAccountingEquation(context.Background())
	require.NoError(t, err)
	assert.False(t, result.Balanced)
	assert.True(t, result.Gap.Equal(decimal.NewFromInt(1)))
}

func TestReconciliationService_AccountCheckpointDrift(t *testing.T) {
	cls := &mockClassificationLister{
		classifications: []core.Classification{
			{ID: 10, Code: "asset", NormalSide: core.NormalSideDebit},
		},
	}
	cpReader := &mockCheckpointReader{
		checkpoints: []core.BalanceCheckpoint{
			{
				AccountHolder:    100,
				CurrencyID:       1,
				ClassificationID: 10,
				Balance:          decimal.NewFromInt(500), // checkpoint says 500
			},
		},
	}
	accountEntries := &mockAccountEntrySummer{
		debitByClass:  map[int64]decimal.Decimal{10: decimal.NewFromInt(600)},
		creditByClass: map[int64]decimal.Decimal{10: decimal.NewFromInt(200)},
	}
	// Actual from entries: debit - credit = 600 - 200 = 400, but checkpoint says 500 => drift of 100

	engine := core.NewEngine()
	svc := NewReconciliationService(nil, accountEntries, cpReader, cls, engine)

	result, err := svc.ReconcileAccount(context.Background(), 100, 1)
	require.NoError(t, err)
	assert.False(t, result.Balanced)
	assert.Equal(t, 1, len(result.Details))
	assert.True(t, result.Details[0].Drift.Equal(decimal.NewFromInt(100)))
}
