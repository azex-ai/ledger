// Package observability provides production observability adapters that bind
// the core.Metrics and core.Logger interfaces to concrete implementations.
//
// The Prometheus adapter is the canonical example: it wires every metric the
// core engine emits into a single *prometheus.Registry which the service can
// expose via promhttp.HandlerFor.
package observability

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
)

// safeLabel returns the empty-string sentinel "_" for empty labels and
// otherwise returns the value verbatim. Prometheus accepts empty labels but
// they hide a probable bug in the call site.
func safeLabel(v string) string {
	if v == "" {
		return "_"
	}
	return v
}

// int64Label converts a numeric ID to a label string. Currency IDs are bounded
// (single-digit / low-thousands), so cardinality stays tame.
func int64Label(v int64) string {
	return strconv.FormatInt(v, 10)
}

// decimalToFloat converts a shopspring Decimal to a float64 for use as a gauge
// value. Precision loss is acceptable for observability — alert on canonical
// source-of-truth values, not these gauges.
func decimalToFloat(d decimal.Decimal) float64 {
	f, _ := d.Float64()
	return f
}

// PrometheusMetrics implements core.Metrics on top of a *prometheus.Registry.
//
// All label sets must come from a bounded vocabulary. Free-form strings (UUIDs,
// user IDs, etc.) MUST NOT be passed in as labels — Prometheus stores one
// timeseries per unique label combination and high-cardinality label sets will
// blow up memory.
type PrometheusMetrics struct {
	registry *prometheus.Registry

	// Counters
	journalPosted         *prometheus.CounterVec
	journalFailed         *prometheus.CounterVec
	reserveCreated        prometheus.Counter
	reserveSettled        prometheus.Counter
	reserveReleased       prometheus.Counter
	rollupProcessed       prometheus.Counter
	reconcileCompleted    *prometheus.CounterVec
	idempotencyCollision  *prometheus.CounterVec
	templateFailed        *prometheus.CounterVec
	bookingTransitioned   *prometheus.CounterVec

	// Histograms
	journalLatency    prometheus.Histogram
	rollupLatency     prometheus.Histogram
	snapshotLatency   prometheus.Histogram
	journalEntryCount *prometheus.HistogramVec

	// Gauges
	pendingRollups     prometheus.Gauge
	activeReservations prometheus.Gauge
	checkpointAge      *prometheus.GaugeVec
	balanceDrift       *prometheus.GaugeVec
	reconcileGap       *prometheus.GaugeVec
	reservedAmount     *prometheus.GaugeVec
}

// NewPrometheusMetrics returns a Prometheus-backed core.Metrics implementation
// alongside the registry that holds its collectors. Callers wire the registry
// into an HTTP handler with Handler.
func NewPrometheusMetrics() *PrometheusMetrics {
	registry := prometheus.NewRegistry()
	const ns = "ledger"

	m := &PrometheusMetrics{
		registry: registry,

		journalPosted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "journals_posted_total",
			Help:      "Total journals successfully posted, labelled by journal type code.",
		}, []string{"journal_type"}),
		journalFailed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "journals_failed_total",
			Help:      "Total journal posting failures, labelled by journal type and reason.",
		}, []string{"journal_type", "reason"}),
		reserveCreated: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "reservations_created_total",
			Help:      "Total reservations created.",
		}),
		reserveSettled: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "reservations_settled_total",
			Help:      "Total reservations settled.",
		}),
		reserveReleased: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "reservations_released_total",
			Help:      "Total reservations released without settlement.",
		}),
		rollupProcessed: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "rollups_processed_total",
			Help:      "Total rollup queue items processed.",
		}),
		reconcileCompleted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "reconciliations_completed_total",
			Help:      "Total reconciliation runs, labelled by success.",
		}, []string{"success"}),
		idempotencyCollision: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "idempotency_collisions_total",
			Help:      "Total idempotency-key collisions detected, labelled by journal type.",
		}, []string{"journal_type"}),
		templateFailed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "template_failed_total",
			Help:      "Template execution failures, labelled by template code and reason.",
		}, []string{"template", "reason"}),
		bookingTransitioned: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "bookings_transitioned_total",
			Help:      "Booking state transitions, labelled by classification code and destination status.",
		}, []string{"class", "to_status"}),

		journalLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: ns,
			Name:      "journal_post_seconds",
			Help:      "Wall-clock latency of PostJournal.",
			Buckets:   prometheus.DefBuckets,
		}),
		rollupLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: ns,
			Name:      "rollup_seconds",
			Help:      "Wall-clock latency of a single rollup batch.",
			Buckets:   prometheus.DefBuckets,
		}),
		snapshotLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: ns,
			Name:      "snapshot_seconds",
			Help:      "Wall-clock latency of CreateDailySnapshot.",
			Buckets:   prometheus.DefBuckets,
		}),
		journalEntryCount: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: ns,
			Name:      "journal_entry_count",
			Help:      "Number of entries per journal, labelled by journal type.",
			Buckets:   []float64{2, 4, 8, 16, 32, 64, 128},
		}, []string{"journal_type"}),

		pendingRollups: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "rollups_pending",
			Help:      "Current depth of the rollup queue.",
		}),
		activeReservations: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "reservations_active",
			Help:      "Currently active (un-settled, un-released) reservations.",
		}),
		checkpointAge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "checkpoint_age_seconds",
			Help:      "Age of the oldest checkpoint, labelled by classification code.",
		}, []string{"class"}),
		balanceDrift: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "balance_drift_units",
			Help:      "Drift between expected and actual balance, labelled by class and currency.",
		}, []string{"class", "currency_id"}),
		reconcileGap: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "reconcile_gap_units",
			Help:      "Reconciliation gap, labelled by currency.",
		}, []string{"currency_id"}),
		reservedAmount: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "reserved_amount_units",
			Help:      "Total reserved amount per currency.",
		}, []string{"currency_id"}),
	}

	registry.MustRegister(
		m.journalPosted, m.journalFailed,
		m.reserveCreated, m.reserveSettled, m.reserveReleased,
		m.rollupProcessed, m.reconcileCompleted,
		m.idempotencyCollision, m.templateFailed, m.bookingTransitioned,
		m.journalLatency, m.rollupLatency, m.snapshotLatency, m.journalEntryCount,
		m.pendingRollups, m.activeReservations, m.checkpointAge,
		m.balanceDrift, m.reconcileGap, m.reservedAmount,
	)

	return m
}

// Registry returns the underlying Prometheus registry. Useful for adding
// process/Go runtime collectors or composing with other modules.
func (m *PrometheusMetrics) Registry() *prometheus.Registry { return m.registry }

// Handler returns an http.Handler that serves the Prometheus exposition
// format from this collector's registry. Mount it on /metrics or wherever
// your scrape config points.
func (m *PrometheusMetrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		// Continue on error so a single broken collector doesn't take down /metrics.
		ErrorHandling: promhttp.ContinueOnError,
	})
}

// Compile-time check.
var _ core.Metrics = (*PrometheusMetrics)(nil)

// --- core.Metrics implementation ---

// JournalPosted increments journal post counter for the given type.
func (m *PrometheusMetrics) JournalPosted(journalTypeCode string) {
	m.journalPosted.WithLabelValues(safeLabel(journalTypeCode)).Inc()
}

// JournalFailed increments journal failure counter.
func (m *PrometheusMetrics) JournalFailed(journalTypeCode, reason string) {
	m.journalFailed.WithLabelValues(safeLabel(journalTypeCode), safeLabel(reason)).Inc()
}

func (m *PrometheusMetrics) ReserveCreated()  { m.reserveCreated.Inc() }
func (m *PrometheusMetrics) ReserveSettled()  { m.reserveSettled.Inc() }
func (m *PrometheusMetrics) ReserveReleased() { m.reserveReleased.Inc() }

// RollupProcessed adds to the rollup counter (not labelled).
func (m *PrometheusMetrics) RollupProcessed(count int) {
	if count > 0 {
		m.rollupProcessed.Add(float64(count))
	}
}

// ReconcileCompleted increments reconciliation counter, labelled by success.
func (m *PrometheusMetrics) ReconcileCompleted(success bool) {
	label := "false"
	if success {
		label = "true"
	}
	m.reconcileCompleted.WithLabelValues(label).Inc()
}

func (m *PrometheusMetrics) IdempotencyCollision(journalTypeCode string) {
	m.idempotencyCollision.WithLabelValues(safeLabel(journalTypeCode)).Inc()
}

func (m *PrometheusMetrics) TemplateFailed(templateCode, reason string) {
	m.templateFailed.WithLabelValues(safeLabel(templateCode), safeLabel(reason)).Inc()
}

// BookingTransitioned records a booking state transition.
func (m *PrometheusMetrics) BookingTransitioned(classCode, toStatus string) {
	m.bookingTransitioned.WithLabelValues(safeLabel(classCode), safeLabel(toStatus)).Inc()
}

// Histograms.
func (m *PrometheusMetrics) JournalLatency(d time.Duration) { m.journalLatency.Observe(d.Seconds()) }
func (m *PrometheusMetrics) RollupLatency(d time.Duration)  { m.rollupLatency.Observe(d.Seconds()) }
func (m *PrometheusMetrics) SnapshotLatency(d time.Duration) {
	m.snapshotLatency.Observe(d.Seconds())
}
func (m *PrometheusMetrics) JournalEntryCount(journalTypeCode string, count int) {
	m.journalEntryCount.WithLabelValues(safeLabel(journalTypeCode)).Observe(float64(count))
}

// Gauges.
func (m *PrometheusMetrics) PendingRollups(count int64)     { m.pendingRollups.Set(float64(count)) }
func (m *PrometheusMetrics) ActiveReservations(count int64) { m.activeReservations.Set(float64(count)) }
func (m *PrometheusMetrics) CheckpointAge(classCode string, age time.Duration) {
	m.checkpointAge.WithLabelValues(safeLabel(classCode)).Set(age.Seconds())
}

// BalanceDrift records the latest drift for a (class, currency) pair.
// We deliberately downcast the decimal to a float here — observability values
// don't need 30 digits of precision; if precision matters, alert on the source.
func (m *PrometheusMetrics) BalanceDrift(classCode string, currencyID int64, delta decimal.Decimal) {
	m.balanceDrift.WithLabelValues(safeLabel(classCode), int64Label(currencyID)).Set(decimalToFloat(delta))
}

func (m *PrometheusMetrics) ReconcileGap(currencyID int64, gap decimal.Decimal) {
	m.reconcileGap.WithLabelValues(int64Label(currencyID)).Set(decimalToFloat(gap))
}

func (m *PrometheusMetrics) ReservedAmount(currencyID int64, amount decimal.Decimal) {
	m.reservedAmount.WithLabelValues(int64Label(currencyID)).Set(decimalToFloat(amount))
}
