package observability

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

func TestPrometheusMetrics_ImplementsCoreMetrics(t *testing.T) {
	var _ core.Metrics = NewPrometheusMetrics()
}

// Calling every method must not panic, must increment the underlying
// collectors, and must surface them through the /metrics endpoint.
func TestPrometheusMetrics_EndToEnd(t *testing.T) {
	m := NewPrometheusMetrics()

	m.JournalPosted("transfer")
	m.JournalPosted("transfer")
	m.JournalFailed("transfer", "unbalanced")
	m.ReserveCreated()
	m.ReserveSettled()
	m.ReserveReleased()
	m.RollupProcessed(5)
	m.RollupProcessed(0) // must not error or add zero
	m.ReconcileCompleted(true)
	m.ReconcileCompleted(false)
	m.IdempotencyCollision("withdraw_confirm")
	m.TemplateFailed("deposit_confirm", "missing_amount_key")
	m.BookingTransitioned("deposit", "confirmed")

	m.JournalLatency(15 * time.Millisecond)
	m.RollupLatency(2 * time.Second)
	m.SnapshotLatency(120 * time.Millisecond)
	m.JournalEntryCount("transfer", 4)

	m.PendingRollups(42)
	m.ActiveReservations(7)
	m.CheckpointAge("deposit", time.Hour)
	m.BalanceDrift("deposit", 1, decimal.RequireFromString("0.0001"))
	m.ReconcileGap(1, decimal.RequireFromString("12.34"))
	m.ReservedAmount(1, decimal.RequireFromString("999.99"))

	// Scrape /metrics and verify a representative subset of metrics surfaced.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code)
	body := rec.Body.String()

	wantSubstrings := []string{
		`ledger_journals_posted_total{journal_type="transfer"} 2`,
		`ledger_journals_failed_total{journal_type="transfer",reason="unbalanced"} 1`,
		`ledger_reservations_created_total 1`,
		`ledger_reservations_settled_total 1`,
		`ledger_reservations_released_total 1`,
		`ledger_rollups_processed_total 5`,
		`ledger_reconciliations_completed_total{success="true"} 1`,
		`ledger_reconciliations_completed_total{success="false"} 1`,
		`ledger_idempotency_collisions_total{journal_type="withdraw_confirm"} 1`,
		`ledger_template_failed_total{reason="missing_amount_key",template="deposit_confirm"} 1`,
		`ledger_bookings_transitioned_total{class="deposit",to_status="confirmed"} 1`,
		`ledger_rollups_pending 42`,
		`ledger_reservations_active 7`,
		`ledger_checkpoint_age_seconds{class="deposit"} 3600`,
		`ledger_reconcile_gap_units{currency_id="1"} 12.34`,
		`ledger_reserved_amount_units{currency_id="1"} 999.99`,
	}
	for _, want := range wantSubstrings {
		assert.True(t, strings.Contains(body, want), "missing in /metrics: %s", want)
	}
}

// Empty-string labels must be normalised to "_" so a free-form-string bug
// in the call site doesn't accidentally collapse two distinct dimensions
// into one timeseries (or worse, blow up cardinality silently).
func TestPrometheusMetrics_SafeLabel(t *testing.T) {
	assert.Equal(t, "_", safeLabel(""))
	assert.Equal(t, "transfer", safeLabel("transfer"))
}

// Registry exposes the underlying registry so the host can mount additional
// collectors (Go runtime, process). Nil-safety: the constructor must never
// return a service whose registry is nil.
func TestPrometheusMetrics_RegistryNotNil(t *testing.T) {
	m := NewPrometheusMetrics()
	require.NotNil(t, m.Registry())
}

// Two NewPrometheusMetrics calls must produce independent registries —
// otherwise concurrent tests will collide on metric registration.
func TestPrometheusMetrics_IndependentRegistries(t *testing.T) {
	a := NewPrometheusMetrics()
	b := NewPrometheusMetrics()
	assert.NotSame(t, a.Registry(), b.Registry())
}
