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
	DepositConfirmed(channelName string)
	WithdrawConfirmed(channelName string)

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
}

type nopMetrics struct{}

func (nopMetrics) JournalPosted(string)                              {}
func (nopMetrics) JournalFailed(string, string)                      {}
func (nopMetrics) ReserveCreated()                                   {}
func (nopMetrics) ReserveSettled()                                   {}
func (nopMetrics) ReserveReleased()                                  {}
func (nopMetrics) RollupProcessed(int)                               {}
func (nopMetrics) ReconcileCompleted(bool)                           {}
func (nopMetrics) IdempotencyCollision(string)                       {}
func (nopMetrics) TemplateFailed(string, string)                     {}
func (nopMetrics) DepositConfirmed(string)                           {}
func (nopMetrics) WithdrawConfirmed(string)                          {}
func (nopMetrics) JournalLatency(time.Duration)                      {}
func (nopMetrics) RollupLatency(time.Duration)                       {}
func (nopMetrics) SnapshotLatency(time.Duration)                     {}
func (nopMetrics) JournalEntryCount(string, int)                     {}
func (nopMetrics) PendingRollups(int64)                              {}
func (nopMetrics) ActiveReservations(int64)                          {}
func (nopMetrics) CheckpointAge(string, time.Duration)               {}
func (nopMetrics) BalanceDrift(string, int64, decimal.Decimal)       {}
func (nopMetrics) ReconcileGap(int64, decimal.Decimal)               {}
func (nopMetrics) ReservedAmount(int64, decimal.Decimal)             {}

// NopMetrics returns a no-op metrics collector.
func NopMetrics() Metrics { return nopMetrics{} }
