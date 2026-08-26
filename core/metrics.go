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
	BalanceDrift(classCode string, currencyID int64, delta decimal.Decimal)
	ReconcileGap(currencyID int64, gap decimal.Decimal)
	ReservedAmount(currencyID int64, amount decimal.Decimal)

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
}

// NoopMetrics is a Metrics implementation where every method is a no-op.
// Exported (unlike its predecessor, an unexported nopMetrics) so consumers
// can embed it as a base and override only the handful of methods they
// actually wire up -- see the Metrics doc comment for the embedding pattern.
// NopMetrics() below returns this same type behind the Metrics interface for
// callers that just want a working default with no customization.
type NoopMetrics struct{}

func (NoopMetrics) JournalPosted(string)                        {}
func (NoopMetrics) JournalFailed(string, string)                {}
func (NoopMetrics) ReserveCreated()                             {}
func (NoopMetrics) ReserveSettled()                             {}
func (NoopMetrics) ReserveReleased()                            {}
func (NoopMetrics) RollupProcessed(int)                         {}
func (NoopMetrics) ReconcileCompleted(bool)                     {}
func (NoopMetrics) IdempotencyCollision(string)                 {}
func (NoopMetrics) TemplateFailed(string, string)               {}
func (NoopMetrics) BookingTransitioned(string, string)          {}
func (NoopMetrics) EventDelivered()                             {}
func (NoopMetrics) EventDeliveryFailed()                        {}
func (NoopMetrics) EventDead()                                  {}
func (NoopMetrics) RollupItemFailed()                           {}
func (NoopMetrics) ReconcileCheckResult(string, bool)           {}
func (NoopMetrics) JournalLatency(time.Duration)                {}
func (NoopMetrics) RollupLatency(time.Duration)                 {}
func (NoopMetrics) SnapshotLatency(time.Duration)               {}
func (NoopMetrics) JournalEntryCount(string, int)               {}
func (NoopMetrics) PendingRollups(int64)                        {}
func (NoopMetrics) ActiveReservations(int64)                    {}
func (NoopMetrics) CheckpointAge(string, time.Duration)         {}
func (NoopMetrics) BalanceDrift(string, int64, decimal.Decimal) {}
func (NoopMetrics) ReconcileGap(int64, decimal.Decimal)         {}
func (NoopMetrics) ReservedAmount(int64, decimal.Decimal)       {}

func (NoopMetrics) ChainCursorLag(int64, int64)         {}
func (NoopMetrics) DepositReorgDetected(int64)          {}
func (NoopMetrics) SweepUnattributed(int64)             {}
func (NoopMetrics) RegistrationRescanFailed(int64)      {}
func (NoopMetrics) DepositReviewRequired(int64, string) {}

// NopMetrics returns a no-op metrics collector.
func NopMetrics() Metrics { return NoopMetrics{} }
