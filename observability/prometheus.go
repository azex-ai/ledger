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
	journalPosted        *prometheus.CounterVec
	journalFailed        *prometheus.CounterVec
	reserveCreated       prometheus.Counter
	reserveSettled       prometheus.Counter
	reserveReleased      prometheus.Counter
	rollupProcessed      prometheus.Counter
	reconcileCompleted   *prometheus.CounterVec
	idempotencyCollision *prometheus.CounterVec
	templateFailed       *prometheus.CounterVec
	bookingTransitioned  *prometheus.CounterVec
	eventDelivered       prometheus.Counter
	eventDeliveryFailed  prometheus.Counter
	eventDead            prometheus.Counter
	rollupItemFailed     prometheus.Counter
	reconcileCheckResult *prometheus.CounterVec

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
	chainCursorLag     *prometheus.GaugeVec

	// negativeBalanceDetected is a monotonic counterpart to the balanceDrift
	// Gauge above (M-3 fix, I-41 point 3): the Gauge's label omits holder to
	// keep cardinality bounded, so a healthy item can overwrite a different
	// holder's still-negative reading under the same (class, currency)
	// label. This Counter can only ever go up, so it stays a reliable
	// alerting signal (`increase(...) > 0`) regardless of how many other
	// holders' healthy items are interleaved.
	negativeBalanceDetected *prometheus.CounterVec

	// Onchain counters
	depositReorgDetected     *prometheus.CounterVec
	sweepUnattributed        *prometheus.CounterVec
	sweepAddressUnreadable   *prometheus.CounterVec
	registrationRescanFailed *prometheus.CounterVec
	depositReviewRequired    *prometheus.CounterVec

	// Background jobs (I-M10)
	jobTickCompleted     *prometheus.CounterVec
	jobTickFailed        *prometheus.CounterVec
	jobTickSkippedLocked *prometheus.CounterVec
	jobPanicked          *prometheus.CounterVec
	stuckRollups         prometheus.Gauge
	pendingEvents        prometheus.Gauge

	// Tamper-evidence chain (C-M9 / I-M8)
	attestationBatchResult *prometheus.CounterVec
	anchorPublishResult    *prometheus.CounterVec
	anchorLagSeqs          prometheus.Gauge
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
		eventDelivered: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "events_delivered_total",
			Help:      "Total outbound events successfully delivered to all matched webhook subscribers.",
		}),
		eventDeliveryFailed: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "events_delivery_failed_total",
			Help:      "Total outbound event delivery attempts where at least one subscriber failed.",
		}),
		eventDead: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "events_dead_total",
			Help:      "Total outbound events that exhausted their retry budget and were parked.",
		}),
		rollupItemFailed: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "rollup_items_failed_total",
			Help:      "Total rollup queue items whose claim was released after a failed processing attempt.",
		}),
		reconcileCheckResult: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "reconcile_check_results_total",
			Help:      "Full reconciliation suite check outcomes, labelled by check name and pass/fail.",
		}, []string{"check", "passed"}),

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
		}, []string{"class", "currency_uid"}),
		negativeBalanceDetected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "negative_balance_detected_total",
			Help:      "Total rollup items found with a negative balance on a debit-normal classification, labelled by class and currency. Monotonic -- unlike balance_drift_units, cannot be masked by a different holder's healthy item sharing the same label.",
		}, []string{"class", "currency_uid"}),
		reconcileGap: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "reconcile_gap_units",
			Help:      "Reconciliation gap, labelled by currency.",
		}, []string{"currency_uid"}),
		reservedAmount: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "reserved_amount_units",
			Help:      "Total reserved amount per currency.",
		}, []string{"currency_uid"}),
		chainCursorLag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "chain_cursor_lag_blocks",
			Help:      "Blocks behind the chain tip the deposit watcher's cursor currently is, labelled by chain.",
		}, []string{"chain_id"}),
		depositReorgDetected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "deposit_reorg_detected_total",
			Help:      "Total confirmed deposits found to have disappeared from the canonical chain (deep reorg), labelled by chain.",
		}, []string{"chain_id"}),
		sweepUnattributed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "sweep_unattributed_total",
			Help:      "Total sweep batches collecting a token with no ledger attribution, labelled by chain.",
		}, []string{"chain_id"}),
		sweepAddressUnreadable: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "sweep_address_unreadable_total",
			Help:      "Total addresses whose balance ChainScanner.ScanBalances could not read in a sweep round (excluded from that round, not treated as zero), labelled by chain.",
		}, []string{"chain_id"}),
		registrationRescanFailed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "registration_rescan_failed_total",
			Help:      "Total EnsureDepositAddress background historical rescan failures, labelled by chain.",
		}, []string{"chain_id"}),
		depositReviewRequired: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "deposit_review_required_total",
			Help:      "Total deposits routed to human review instead of auto-crediting, labelled by chain and reason.",
		}, []string{"chain_id", "reason"}),

		jobTickCompleted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "job_tick_completed_total",
			Help:      "Total background job ticks that returned without error, labelled by job name. Use increase(...)==0 to alert on a stalled job (see docs/RUNBOOK.md).",
		}, []string{"job"}),
		jobTickFailed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "job_tick_failed_total",
			Help:      "Total background job ticks that returned an error, labelled by job name.",
		}, []string{"job"}),
		jobTickSkippedLocked: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "job_tick_skipped_locked_total",
			Help:      "Total background job ticks skipped because another replica held the advisory lock, labelled by job name.",
		}, []string{"job"}),
		jobPanicked: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "job_panicked_total",
			Help:      "Total background job ticks that panicked (always recovered), labelled by job name.",
		}, []string{"job"}),
		stuckRollups: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "rollups_stuck",
			Help:      "Rollup queue items that exhausted their retry budget and require manual intervention (see docs/RUNBOOK.md).",
		}),
		pendingEvents: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "events_pending",
			Help:      "Current size of the outbound-event delivery queue.",
		}),

		attestationBatchResult: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "attestation_batch_result_total",
			Help:      "Total P6 attestation batch attempts, labelled by success.",
		}, []string{"success"}),
		anchorPublishResult: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns,
			Name:      "anchor_publish_result_total",
			Help:      "Total attempts to publish the attestation batch chain head to an external anchor, labelled by success.",
		}, []string{"success"}),
		anchorLagSeqs: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: ns,
			Name:      "anchor_lag_seqs",
			Help:      "Sequence numbers the last-published anchor head is behind the latest locally-signed attestation batch.",
		}),
	}

	registry.MustRegister(
		m.journalPosted, m.journalFailed,
		m.reserveCreated, m.reserveSettled, m.reserveReleased,
		m.rollupProcessed, m.reconcileCompleted,
		m.idempotencyCollision, m.templateFailed, m.bookingTransitioned,
		m.eventDelivered, m.eventDeliveryFailed, m.eventDead,
		m.rollupItemFailed, m.reconcileCheckResult,
		m.journalLatency, m.rollupLatency, m.snapshotLatency, m.journalEntryCount,
		m.pendingRollups, m.activeReservations, m.checkpointAge,
		m.balanceDrift, m.negativeBalanceDetected, m.reconcileGap, m.reservedAmount,
		m.chainCursorLag, m.depositReorgDetected, m.sweepUnattributed,
		m.sweepAddressUnreadable,
		m.registrationRescanFailed, m.depositReviewRequired,
		m.jobTickCompleted, m.jobTickFailed, m.jobTickSkippedLocked, m.jobPanicked,
		m.stuckRollups, m.pendingEvents,
		m.attestationBatchResult, m.anchorPublishResult, m.anchorLagSeqs,
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

// EventDelivered increments the successful outbound event delivery counter.
func (m *PrometheusMetrics) EventDelivered() { m.eventDelivered.Inc() }

// EventDeliveryFailed increments the failed outbound event delivery counter.
func (m *PrometheusMetrics) EventDeliveryFailed() { m.eventDeliveryFailed.Inc() }

// EventDead increments the counter of events parked after exhausting retries.
func (m *PrometheusMetrics) EventDead() { m.eventDead.Inc() }

// RollupItemFailed increments the counter of rollup queue items released
// after a failed processing attempt.
func (m *PrometheusMetrics) RollupItemFailed() { m.rollupItemFailed.Inc() }

// ReconcileCheckResult records one outcome of the full reconciliation suite.
func (m *PrometheusMetrics) ReconcileCheckResult(checkName string, passed bool) {
	label := "false"
	if passed {
		label = "true"
	}
	m.reconcileCheckResult.WithLabelValues(safeLabel(checkName), label).Inc()
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
//
// currencyUID, not the internal currencies.id (H-M9) -- see core.Metrics'
// doc comment on this method.
func (m *PrometheusMetrics) BalanceDrift(classCode string, currencyUID string, delta decimal.Decimal) {
	m.balanceDrift.WithLabelValues(safeLabel(classCode), safeLabel(currencyUID)).Set(decimalToFloat(delta))
}

// NegativeBalanceDetected increments the monotonic negative-balance counter.
// See the negativeBalanceDetected field doc for why this exists alongside
// BalanceDrift instead of replacing it.
func (m *PrometheusMetrics) NegativeBalanceDetected(classCode string, currencyUID string) {
	m.negativeBalanceDetected.WithLabelValues(safeLabel(classCode), safeLabel(currencyUID)).Inc()
}

func (m *PrometheusMetrics) ReconcileGap(currencyUID string, gap decimal.Decimal) {
	m.reconcileGap.WithLabelValues(safeLabel(currencyUID)).Set(decimalToFloat(gap))
}

func (m *PrometheusMetrics) ReservedAmount(currencyUID string, amount decimal.Decimal) {
	m.reservedAmount.WithLabelValues(safeLabel(currencyUID)).Set(decimalToFloat(amount))
}

// --- Onchain (crypto deposit + sweep) ---

// ChainCursorLag records the deposit watcher's current lag behind the chain tip.
func (m *PrometheusMetrics) ChainCursorLag(chainID int64, lagBlocks int64) {
	m.chainCursorLag.WithLabelValues(int64Label(chainID)).Set(float64(lagBlocks))
}

// DepositReorgDetected increments the deep-reorg detection counter.
func (m *PrometheusMetrics) DepositReorgDetected(chainID int64) {
	m.depositReorgDetected.WithLabelValues(int64Label(chainID)).Inc()
}

// SweepUnattributed increments the unattributed-sweep counter.
func (m *PrometheusMetrics) SweepUnattributed(chainID int64) {
	m.sweepUnattributed.WithLabelValues(int64Label(chainID)).Inc()
}

// SweepAddressUnreadable increments the unreadable-sweep-address counter by
// count.
func (m *PrometheusMetrics) SweepAddressUnreadable(chainID int64, count int) {
	m.sweepAddressUnreadable.WithLabelValues(int64Label(chainID)).Add(float64(count))
}

// RegistrationRescanFailed increments the registration-rescan failure counter.
func (m *PrometheusMetrics) RegistrationRescanFailed(chainID int64) {
	m.registrationRescanFailed.WithLabelValues(int64Label(chainID)).Inc()
}

// DepositReviewRequired increments the review-required counter.
func (m *PrometheusMetrics) DepositReviewRequired(chainID int64, reason string) {
	m.depositReviewRequired.WithLabelValues(int64Label(chainID), safeLabel(reason)).Inc()
}

// --- Background jobs (I-M10) ---

func (m *PrometheusMetrics) JobTickCompleted(job string) {
	m.jobTickCompleted.WithLabelValues(safeLabel(job)).Inc()
}

func (m *PrometheusMetrics) JobTickFailed(job string) {
	m.jobTickFailed.WithLabelValues(safeLabel(job)).Inc()
}

func (m *PrometheusMetrics) JobTickSkippedLocked(job string) {
	m.jobTickSkippedLocked.WithLabelValues(safeLabel(job)).Inc()
}

func (m *PrometheusMetrics) JobPanicked(job string) {
	m.jobPanicked.WithLabelValues(safeLabel(job)).Inc()
}

func (m *PrometheusMetrics) StuckRollups(count int64) { m.stuckRollups.Set(float64(count)) }

func (m *PrometheusMetrics) PendingEvents(count int64) { m.pendingEvents.Set(float64(count)) }

// --- Tamper-evidence chain (C-M9 / I-M8) ---

func (m *PrometheusMetrics) AttestationBatchResult(ok bool) {
	m.attestationBatchResult.WithLabelValues(strconv.FormatBool(ok)).Inc()
}

func (m *PrometheusMetrics) AnchorPublishResult(ok bool) {
	m.anchorPublishResult.WithLabelValues(strconv.FormatBool(ok)).Inc()
}

func (m *PrometheusMetrics) AnchorLagSeqs(lag int64) { m.anchorLagSeqs.Set(float64(lag)) }
