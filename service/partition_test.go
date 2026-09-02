package service

// F-M3 (2026-09-02 audit): PartitionService.EnsureUpcoming had zero test
// coverage -- I-13's three store-level pins all call
// PartitionStore.EnsureMonthlyPartitions directly, skipping this service
// layer entirely. The whole method could be swapped for `return nil` and
// `go test ./...` stayed green.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

type mockPartitionEnsurer struct {
	created            []string
	hasRows            bool
	rebalanced         []string
	ensureCalled       bool
	defaultCheckCalled bool
	rebalanceCalled    bool
}

func (m *mockPartitionEnsurer) EnsureMonthlyPartitions(_ context.Context, _ time.Time, _ int) ([]string, error) {
	m.ensureCalled = true
	return m.created, nil
}

func (m *mockPartitionEnsurer) DefaultPartitionHasRows(_ context.Context) (bool, error) {
	m.defaultCheckCalled = true
	return m.hasRows, nil
}

func (m *mockPartitionEnsurer) RebalanceDefault(_ context.Context, _ time.Time, _ int) ([]string, error) {
	m.rebalanceCalled = true
	return m.rebalanced, nil
}

// TestPartitionService_EnsureUpcoming_HealthyHorizon pins the common path: no
// stranded rows, so RebalanceDefault must never be called. A no-op
// implementation of EnsureUpcoming would also pass this half alone, which is
// exactly why the next test exists.
func TestPartitionService_EnsureUpcoming_HealthyHorizon(t *testing.T) {
	store := &mockPartitionEnsurer{created: []string{"journal_entries_y2026m10"}, hasRows: false}
	engine := core.NewEngine()
	svc := NewPartitionService(store, engine)

	err := svc.EnsureUpcoming(context.Background(), time.Now(), 2)
	require.NoError(t, err)
	assert.True(t, store.ensureCalled, "EnsureMonthlyPartitions must be called")
	assert.True(t, store.defaultCheckCalled, "DefaultPartitionHasRows must be called")
	assert.False(t, store.rebalanceCalled, "RebalanceDefault must not fire when the default partition is empty")
}

// TestPartitionService_EnsureUpcoming_StrandedRowsTriggersRebalance pins the
// self-heal path (docs/INVARIANTS.md I-13): when DefaultPartitionHasRows
// reports true, EnsureUpcoming must call RebalanceDefault. A no-op
// EnsureUpcoming (`return nil` on line 1) calls neither store method and
// goes red on both assertions below.
func TestPartitionService_EnsureUpcoming_StrandedRowsTriggersRebalance(t *testing.T) {
	store := &mockPartitionEnsurer{hasRows: true, rebalanced: []string{"journal_entries_y2026m03"}}
	engine := core.NewEngine()
	svc := NewPartitionService(store, engine)

	err := svc.EnsureUpcoming(context.Background(), time.Now(), 2)
	require.NoError(t, err)
	assert.True(t, store.rebalanceCalled, "stranded default-partition rows must trigger RebalanceDefault")
}
