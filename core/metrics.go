package core

import (
	"time"

	"github.com/shopspring/decimal"
)

// Metrics is the observability interface for counters, histograms, and gauges.
// Inject Prometheus, OpenTelemetry, or DataDog implementation. Default:
// NoopMetrics (silent), via NopMetrics().
// NOTE: reason/code parameters must be constrained enums, not free-form strings (Prometheus cardinality).
//
// This interface is intentionally wide (one method per emitted signal, not
// grouped into a handful of generic Counter/Gauge/Histogram calls) so each
// call site names what it means rather than a raw metric name string. A
// consumer implementing only a few of these -- e.g. wiring just
// JournalPosted into an existing internal dashboard -- should embed
// NoopMetrics and override the handful it cares about, rather than writing
// 30 empty method bodies by hand:
//
//	type myMetrics struct{ core.NoopMetrics }
//	func (m *myMetrics) JournalPosted(code string) { ... }
type Metrics interface {
	// Counters
	JournalPosted(journalTypeCode string)
	JournalFailed(journalTypeCode string, reason string)
	ReserveCreated()
	ReserveSettled()
	ReserveReleased()
	RollupProcessed(count int)
	ReconcileCompleted(success bool)
	IdempotencyCollision(journalTypeCode string)
	TemplateFailed(templateCode string, reason string)
	// BookingTransitioned is emitted whenever a booking moves to a new lifecycle
	// state. classCode is the classification code (e.g. "deposit"), toStatus is
	// the destination state (e.g. "confirmed"). Both should come from a bounded
	// set of values to keep Prometheus cardinality in check.
	BookingTransitioned(classCode string, toStatus string)
	// EventDelivered is emitted whenever an outbound event is successfully
	// delivered to all matched webhook subscribers (or has no subscribers to
	// deliver to).
	EventDelivered()
	// EventDeliveryFailed is emitted whenever at least one webhook subscriber
	// delivery attempt fails and the event is scheduled for retry.
	EventDeliveryFailed()
	// EventDead is emitted when an event exhausts its retry budget and is
	// permanently parked (delivery_status = 'dead').
	EventDead()
	// RollupItemFailed is emitted whenever a rollup queue item's claim is
	// released after a failed processing attempt (failed_attempts is bumped).
	RollupItemFailed()
	// ReconcileCheckResult is emitted once per check in the full
	// reconciliation suite. checkName must come from the fixed set of check
	// names (e.g. "orphan_entries") to keep cardinality bounded.
	ReconcileCheckResult(checkName string, passed bool)

	// Histograms
	JournalLatency(d time.Duration)
	RollupLatency(d time.Duration)
	SnapshotLatency(d time.Duration)
	JournalEntryCount(journalTypeCode string, count int)

	// Gauges
	PendingRollups(count int64)
	ActiveReservations(count int64)
	CheckpointAge(classCode string, age time.Duration)

	// Financial

	// BalanceDrift reports the most recent drift-from-zero reading for a
	// (class, currency) label. Do not alert on this alone -- see
	// NegativeBalanceDetected's doc for why a healthy item can mask a real,
	// still-open violation under the same label. Fine for dashboards.
	//
	// currencyUID (not the internal currencies.id) -- H-M9: core.Metrics is a
	// port every consumer's exporter/dashboard config keys off of, and an
	// internal BIGSERIAL id has no meaning outside this database and is not
	// stable across a restore-from-backup (api-contract.md: uid is the only
	// identifier that crosses a boundary). Every other currency-labelled
	// metric on this interface follows the same rule.
	BalanceDrift(classCode string, currencyUID string, delta decimal.Decimal)
	// NegativeBalanceDetected is a monotonic counter incremented every time a
	// rollup item's recomputed balance is found negative on a debit-normal
	// classification -- the same trigger condition BalanceDrift's non-zero
	// readings report. It exists because BalanceDrift is a Gauge labelled
	// (class, currency) WITHOUT holder (deliberately, to keep cardinality
	// bounded -- see the call site in service/rollup.go), so a healthy item
	// for one holder can overwrite a genuinely still-negative reading left by
	// a different holder sharing the same label: the Gauge alone cannot tell
	// "the fleet recovered" from "an unrelated holder in the same bucket
	// happened to be processed last". A Counter cannot be un-incremented by
	// anything, so `increase(negative_balance_detected_total[window]) > 0`
	// stays a reliable alert even while BalanceDrift's own reading bounces
	// back to zero in between (working-agreements §3; I-41 point 3).
	NegativeBalanceDetected(classCode string, currencyUID string)
	ReconcileGap(currencyUID string, gap decimal.Decimal)
	ReservedAmount(currencyUID string, amount decimal.Decimal)

	// Onchain (crypto deposit + sweep, design doc §6)

	// ChainCursorLag reports how many blocks behind the chain tip the
	// deposit watcher's cursor currently is, labelled by chain. A stalled
	// (non-decreasing) lag is the alerting signal for a stuck watcher.
	ChainCursorLag(chainID int64, lagBlocks int64)
	// DepositReorgDetected is emitted whenever a previously-confirmed
	// deposit's transaction is found to have disappeared from the canonical
	// chain (deep reorg), regardless of ReorgPolicy.
	DepositReorgDetected(chainID int64)
	// SweepUnattributed is emitted whenever a sweep batch collects a token
	// that is not in the chain's CreditTokens allowlist -- value moved to
	// treasury with no corresponding user ledger balance, requiring manual
	// reconciliation (design doc §4).
	SweepUnattributed(chainID int64)
	// SweepAddressUnreadable is emitted whenever ChainScanner.ScanBalances
	// could not read one or more addresses' balance this round (count is
	// how many). Those addresses are excluded from the round's sweep-eligible
	// set rather than defaulted to zero (I-41 point 4), and simply retry next
	// cycle -- this is the observability half of that fail-closed-per-address
	// contract (m-10, `.local/independent-review-2026-08-26.md`): "proceeded
	// with what was readable" must be visible, not just quietly true.
	SweepAddressUnreadable(chainID int64, count int)
	// RegistrationRescanFailed is emitted whenever EnsureDepositAddress's
	// background historical rescan of one chain fails (design doc §5-2b):
	// the "deposit sent before registration" gap this rescan exists to close
	// stays open for that address/chain until a retry succeeds, so a failure
	// here must be visible to alerting, not just a log line.
	RegistrationRescanFailed(chainID int64)
	// DepositReviewRequired is emitted whenever a deposit that reached its
	// confirmation threshold is routed to human review instead of
	// auto-crediting (design doc §9: M3 compensating controls), labelled by
	// chain and reason ("over_ceiling" | "reconcile_mismatch" -- a bounded
	// set, safe for Prometheus cardinality).
	DepositReviewRequired(chainID int64, reason string)

	// Background jobs (Worker.runLoop / LockedJob, I-M10)
	//
	// job is the fixed, bounded job name already used for logging (e.g.
	// "rollup", "expiration", "reconcile", "snapshot", "system_rollup",
	// "partition", "attest", or an onchain job's name) -- never a free-form
	// string.

	// JobTickCompleted is emitted after a scheduled job's tick function
	// returns without error, regardless of whether it did any work.
	JobTickCompleted(job string)
	// JobTickFailed is emitted whenever a scheduled job's tick function
	// returns an error. Use increase(ledger_job_tick_completed_total{job=...})
	// == 0 to alert on a stalled job -- see docs/RUNBOOK.md.
	JobTickFailed(job string)
	// JobTickSkippedLocked is emitted whenever a LockedJob's tick is skipped
	// because another replica currently holds the advisory lock. A healthy
	// fleet emits this frequently; use it to distinguish "another replica is
	// doing the work" from "nobody is" -- JobTickCompleted staying flat to
	// zero across every replica, with JobTickSkippedLocked also flat, is the
	// single-replica-stuck signal, not either counter alone.
	JobTickSkippedLocked(job string)
	// JobPanicked is emitted whenever a job's tick function panics. The
	// panic is always recovered (a job bug must not take down the process),
	// but a panic is a stronger signal than an ordinary JobTickFailed and is
	// worth alerting on separately.
	JobPanicked(job string)

	// StuckRollups reports rollup_queue items that have exhausted their
	// retry budget (failed_attempts >= the worker's max) and will never be
	// dequeued again without manual intervention (see
	// docs/RUNBOOK.md "stuck rollup items"). Distinct from PendingRollups,
	// which reports items still being retried -- conflating the two turns a
	// permanently-stuck item into a gauge that never clears, i.e. an alarm
	// nailed to ON that looks identical to ordinary backlog (B-m10).
	StuckRollups(count int64)

	// PendingEvents reports the current size of the outbound-event delivery
	// queue (events with delivery_status = 'pending' or 'retry'), sampled
	// once per delivery job tick. Complements EventDelivered/
	// EventDeliveryFailed/EventDead, which are edge-triggered counters that
	// cannot by themselves show a growing backlog.
	PendingEvents(count int64)

	// Tamper-evidence chain (design doc 2026-08-21 §7/§8, P5/P6)

	// AttestationBatchResult is emitted after every RunAttestBatch attempt,
	// success or failure -- the P6 batch-signing job has otherwise had zero
	// metrics coverage since it was introduced.
	AttestationBatchResult(ok bool)
	// AnchorPublishResult is emitted after every attempt to publish the
	// latest attestation batch to an external core.Anchor, success or
	// failure. A consumer with no Anchor configured never calls this.
	AnchorPublishResult(ok bool)
	// AnchorLagSeqs reports how many attestation-chain sequence numbers the
	// last-published anchor head is behind the latest locally-signed batch.
	// A monotonically growing lag with AnchorPublishResult(false) readings
	// is the alerting signal for "the tamper-evidence chain's external
	// witness has stopped advancing".
	AnchorLagSeqs(lag int64)
}

// NoopMetrics is a Metrics implementation where every method is a no-op.
// Exported (unlike its predecessor, an unexported nopMetrics) so consumers
// can embed it as a base and override only the handful of methods they
// actually wire up -- see the Metrics doc comment for the embedding pattern.
// NopMetrics() below returns this same type behind the Metrics interface for
// callers that just want a working default with no customization.
type NoopMetrics struct{}

func (NoopMetrics) JournalPosted(string)                         {}
func (NoopMetrics) JournalFailed(string, string)                 {}
func (NoopMetrics) ReserveCreated()                              {}
func (NoopMetrics) ReserveSettled()                              {}
func (NoopMetrics) ReserveReleased()                             {}
func (NoopMetrics) RollupProcessed(int)                          {}
func (NoopMetrics) ReconcileCompleted(bool)                      {}
func (NoopMetrics) IdempotencyCollision(string)                  {}
func (NoopMetrics) TemplateFailed(string, string)                {}
func (NoopMetrics) BookingTransitioned(string, string)           {}
func (NoopMetrics) EventDelivered()                              {}
func (NoopMetrics) EventDeliveryFailed()                         {}
func (NoopMetrics) EventDead()                                   {}
func (NoopMetrics) RollupItemFailed()                            {}
func (NoopMetrics) ReconcileCheckResult(string, bool)            {}
func (NoopMetrics) JournalLatency(time.Duration)                 {}
func (NoopMetrics) RollupLatency(time.Duration)                  {}
func (NoopMetrics) SnapshotLatency(time.Duration)                {}
func (NoopMetrics) JournalEntryCount(string, int)                {}
func (NoopMetrics) PendingRollups(int64)                         {}
func (NoopMetrics) ActiveReservations(int64)                     {}
func (NoopMetrics) CheckpointAge(string, time.Duration)          {}
func (NoopMetrics) BalanceDrift(string, string, decimal.Decimal) {}
func (NoopMetrics) NegativeBalanceDetected(string, string)       {}
func (NoopMetrics) ReconcileGap(string, decimal.Decimal)         {}
func (NoopMetrics) ReservedAmount(string, decimal.Decimal)       {}

func (NoopMetrics) ChainCursorLag(int64, int64)         {}
func (NoopMetrics) DepositReorgDetected(int64)          {}
func (NoopMetrics) SweepUnattributed(int64)             {}
func (NoopMetrics) SweepAddressUnreadable(int64, int)   {}
func (NoopMetrics) RegistrationRescanFailed(int64)      {}
func (NoopMetrics) DepositReviewRequired(int64, string) {}

func (NoopMetrics) JobTickCompleted(string)     {}
func (NoopMetrics) JobTickFailed(string)        {}
func (NoopMetrics) JobTickSkippedLocked(string) {}
func (NoopMetrics) JobPanicked(string)          {}
func (NoopMetrics) StuckRollups(int64)          {}
func (NoopMetrics) PendingEvents(int64)         {}

func (NoopMetrics) AttestationBatchResult(bool) {}
func (NoopMetrics) AnchorPublishResult(bool)    {}
func (NoopMetrics) AnchorLagSeqs(int64)         {}

// NopMetrics returns a no-op metrics collector.
func NopMetrics() Metrics { return NoopMetrics{} }
