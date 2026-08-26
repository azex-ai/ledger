package core_test

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"

	"github.com/azex-ai/ledger/core"
)

// partialMetrics demonstrates the embedding pattern core.Metrics's doc
// comment promises: embed core.NoopMetrics, override only the handful of
// signals this consumer actually cares about. Before NoopMetrics was
// exported (it was an unexported nopMetrics), this file could not even
// compile -- core.NoopMetrics did not exist as an identifier reachable from
// outside the core package, so a consumer wanting a partial implementation
// of Metrics' 30 methods had to hand-write every no-op stub itself.
type partialMetrics struct {
	core.NoopMetrics
	journalPostedCodes []string
}

func (m *partialMetrics) JournalPosted(code string) {
	m.journalPostedCodes = append(m.journalPostedCodes, code)
}

// TestNoopMetrics_EmbeddableAsPartialImplementation pins the fix for
// core/metrics.go §5 ("core.Metrics 有 30 个方法, nopMetrics 未导出, 无可嵌入
// 基类"): NoopMetrics must be an exported type any downstream package can
// embed to get a working core.Metrics by overriding only what it uses.
func TestNoopMetrics_EmbeddableAsPartialImplementation(t *testing.T) {
	m := &partialMetrics{}
	// Compile-time proof: embedding core.NoopMetrics plus overriding a
	// single method is enough to satisfy the full core.Metrics interface.
	var _ core.Metrics = m

	m.JournalPosted("deposit_confirm")
	assert.Equal(t, []string{"deposit_confirm"}, m.journalPostedCodes,
		"the overridden method must actually run instead of the embedded no-op")

	// Every method NOT overridden falls through to the embedded NoopMetrics
	// and stays silent -- the whole point of embedding a base instead of
	// requiring 30 hand-written stubs for a consumer that only wants one.
	assert.NotPanics(t, func() {
		m.ReserveCreated()
		m.BalanceDrift("deposit", 1, decimal.NewFromInt(-5))
		m.DepositReviewRequired(1, "over_ceiling")
	})
}
