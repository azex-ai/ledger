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

type mockCheckpointAggregator struct {
	rollups []core.SystemRollup
}

func (m *mockCheckpointAggregator) AggregateCheckpointsByClassification(_ context.Context) ([]core.SystemRollup, error) {
	return m.rollups, nil
}

type mockSystemRollupWriter struct {
	upserted []core.SystemRollup
}

func (m *mockSystemRollupWriter) UpsertSystemRollup(_ context.Context, rollup core.SystemRollup) error {
	m.upserted = append(m.upserted, rollup)
	return nil
}

// --- Tests ---

func TestSystemRollupService_MultipleAccounts(t *testing.T) {
	agg := &mockCheckpointAggregator{
		rollups: []core.SystemRollup{
			{CurrencyID: 1, ClassificationID: 10, TotalBalance: decimal.NewFromInt(1000)},
			{CurrencyID: 1, ClassificationID: 20, TotalBalance: decimal.NewFromInt(500)},
			{CurrencyID: 2, ClassificationID: 10, TotalBalance: decimal.NewFromInt(200)},
		},
	}
	writer := &mockSystemRollupWriter{}
	engine := core.NewEngine()

	svc := NewSystemRollupService(agg, writer, engine)
	err := svc.RefreshSystemRollups(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 3, len(writer.upserted))
	assert.True(t, writer.upserted[0].TotalBalance.Equal(decimal.NewFromInt(1000)))
	assert.True(t, writer.upserted[1].TotalBalance.Equal(decimal.NewFromInt(500)))
	assert.True(t, writer.upserted[2].TotalBalance.Equal(decimal.NewFromInt(200)))
}

func TestSystemRollupService_RefreshAfterUpdate(t *testing.T) {
	// First call
	agg := &mockCheckpointAggregator{
		rollups: []core.SystemRollup{
			{CurrencyID: 1, ClassificationID: 10, TotalBalance: decimal.NewFromInt(100)},
		},
	}
	writer := &mockSystemRollupWriter{}
	engine := core.NewEngine()
	svc := NewSystemRollupService(agg, writer, engine)

	err := svc.RefreshSystemRollups(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, len(writer.upserted))

	// Second call with updated data
	agg.rollups = []core.SystemRollup{
		{CurrencyID: 1, ClassificationID: 10, TotalBalance: decimal.NewFromInt(300)},
	}
	err = svc.RefreshSystemRollups(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, len(writer.upserted))
	assert.True(t, writer.upserted[1].TotalBalance.Equal(decimal.NewFromInt(300)))
}
