package core

import (
	"time"

	"github.com/shopspring/decimal"
)

// Metrics is the observability interface for counters, histograms, and gauges.
// Inject Prometheus, OpenTelemetry, or DataDog implementation. Default: nopMetrics (silent).
// NOTE: reason/code parameters must be constrained enums, not free-form strings (Prometheus cardinality).
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
	// ReconcileCheckResult is emitted once per check in the full 10-check
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
}

type nopMetrics struct{}

func (nopMetrics) JournalPosted(string)                        {}
func (nopMetrics) JournalFailed(string, string)                {}
func (nopMetrics) ReserveCreated()                             {}
func (nopMetrics) ReserveSettled()                             {}
func (nopMetrics) ReserveReleased()                            {}
func (nopMetrics) RollupProcessed(int)                         {}
func (nopMetrics) ReconcileCompleted(bool)                     {}
func (nopMetrics) IdempotencyCollision(string)                 {}
func (nopMetrics) TemplateFailed(string, string)               {}
func (nopMetrics) BookingTransitioned(string, string)          {}
func (nopMetrics) EventDelivered()                             {}
func (nopMetrics) EventDeliveryFailed()                        {}
func (nopMetrics) EventDead()                                  {}
func (nopMetrics) RollupItemFailed()                           {}
func (nopMetrics) ReconcileCheckResult(string, bool)           {}
func (nopMetrics) JournalLatency(time.Duration)                {}
func (nopMetrics) RollupLatency(time.Duration)                 {}
func (nopMetrics) SnapshotLatency(time.Duration)               {}
func (nopMetrics) JournalEntryCount(string, int)               {}
func (nopMetrics) PendingRollups(int64)                        {}
func (nopMetrics) ActiveReservations(int64)                    {}
func (nopMetrics) CheckpointAge(string, time.Duration)         {}
func (nopMetrics) BalanceDrift(string, int64, decimal.Decimal) {}
func (nopMetrics) ReconcileGap(int64, decimal.Decimal)         {}
func (nopMetrics) ReservedAmount(int64, decimal.Decimal)       {}

func (nopMetrics) ChainCursorLag(int64, int64) {}
func (nopMetrics) DepositReorgDetected(int64)  {}
func (nopMetrics) SweepUnattributed(int64)     {}

// NopMetrics returns a no-op metrics collector.
func NopMetrics() Metrics { return nopMetrics{} }
