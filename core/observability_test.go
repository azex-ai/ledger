package core

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// NopLogger is the default sink when no Logger is injected. The contract is
// "every method silently swallows arguments, never panics, never allocates a
// surprise observer." We verify each method runs cleanly with mixed argument
// shapes (no args, kv pairs, structured slog-style).
func TestNopLogger_NeverPanics(t *testing.T) {
	logger := NopLogger()
	require.NotNil(t, logger)

	assert.NotPanics(t, func() {
		logger.Info("hello")
		logger.Info("with args", "key", "value", "count", 42)
		logger.Warn("warn", "err", assert.AnError)
		logger.Error("error path", "stack", "deep")
		// nil-tolerant: callers should not be required to pre-validate args.
		logger.Info("nil interface", "ptr", any(nil))
	})
}

// NopMetrics is the default observability sink. Same contract as NopLogger:
// silent, panic-free, non-allocating. We exercise every method with realistic
// arguments to catch any future drift (e.g. a real implementation accidentally
// embedded with required-field assumptions).
func TestNopMetrics_NeverPanics(t *testing.T) {
	m := NopMetrics()
	require.NotNil(t, m)

	assert.NotPanics(t, func() {
		// Counters
		m.JournalPosted("transfer")
		m.JournalFailed("transfer", "unbalanced")
		m.ReserveCreated()
		m.ReserveSettled()
		m.ReserveReleased()
		m.RollupProcessed(128)
		m.ReconcileCompleted(true)
		m.ReconcileCompleted(false)
		m.IdempotencyCollision("transfer")
		m.TemplateFailed("deposit_confirm", "missing_amount_key")
		m.BookingTransitioned("deposit", "confirmed")

		// Histograms
		m.JournalLatency(15 * time.Millisecond)
		m.RollupLatency(2 * time.Second)
		m.SnapshotLatency(120 * time.Millisecond)
		m.JournalEntryCount("transfer", 4)

		// Gauges
		m.PendingRollups(0)
		m.PendingRollups(1_000_000)
		m.ActiveReservations(42)
		m.CheckpointAge("deposit", time.Hour)

		// Financial
		m.BalanceDrift("deposit", 1, decimal.NewFromInt(0))
		m.BalanceDrift("deposit", 1, decimal.RequireFromString("-0.000000001"))
		m.ReconcileGap(1, decimal.NewFromInt(0))
		m.ReservedAmount(1, decimal.RequireFromString("123.456789"))
	})
}

// Ensure the constructors return distinct, usable singletons (not nil interface
// values that would NPE at the first dispatch). Library callers depend on
// these being safe defaults out of the box.
func TestNopDefaults_ReturnNonNil(t *testing.T) {
	assert.NotNil(t, NopLogger(), "NopLogger must return a usable Logger")
	assert.NotNil(t, NopMetrics(), "NopMetrics must return a usable Metrics")
}
