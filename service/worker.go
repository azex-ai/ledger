package service

import (
	"context"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/azex-ai/ledger/core"
)

// WorkerConfig holds configuration for the background Worker.
type WorkerConfig struct {
	RollupInterval         time.Duration // default: 5s
	RollupBatchSize        int           // default: 100
	ExpirationInterval     time.Duration // default: 30s
	ExpirationBatchSize    int           // default: 50
	ReconcileInterval      time.Duration // default: 6h
	SnapshotInterval       time.Duration // default: 24h (run at 02:00)
	SystemRollupInterval   time.Duration // default: 1m
	EventDeliveryInterval  time.Duration // default: 5s
	EventDeliveryBatchSize int           // default: 100
}

// DefaultWorkerConfig returns the default WorkerConfig.
func DefaultWorkerConfig() WorkerConfig {
	return WorkerConfig{
		RollupInterval:         5 * time.Second,
		RollupBatchSize:        100,
		ExpirationInterval:     30 * time.Second,
		ExpirationBatchSize:    50,
		ReconcileInterval:      6 * time.Hour,
		SnapshotInterval:       24 * time.Hour,
		SystemRollupInterval:   time.Minute,
		EventDeliveryInterval:  5 * time.Second,
		EventDeliveryBatchSize: 100,
	}
}

// EventBatchProcessor processes a batch of pending events.
// Implemented by delivery.WebhookDeliverer.
type EventBatchProcessor interface {
	ProcessBatch(ctx context.Context, batchSize int) (int, error)
}

// Worker runs background jobs on configurable intervals.
type Worker struct {
	rollup         *RollupService
	expiration     *ExpirationService
	reconcile      *ReconciliationService
	snapshot       *SnapshotService
	systemRollup   *SystemRollupService
	eventDeliverer EventBatchProcessor // nil = skip event delivery (library mode)
	config         WorkerConfig
	logger         core.Logger
}

// NewWorker creates a new Worker.
func NewWorker(
	rollup *RollupService,
	expiration *ExpirationService,
	reconcile *ReconciliationService,
	snapshot *SnapshotService,
	systemRollup *SystemRollupService,
	config WorkerConfig,
	engine *core.Engine,
) *Worker {
	return &Worker{
		rollup:       rollup,
		expiration:   expiration,
		reconcile:    reconcile,
		snapshot:     snapshot,
		systemRollup: systemRollup,
		config:       config,
		logger:       engine.Logger(),
	}
}

// SetEventDeliverer sets an optional event batch processor for webhook delivery.
// If not set, event delivery is skipped (library mode uses sync callbacks instead).
func (w *Worker) SetEventDeliverer(d EventBatchProcessor) {
	w.eventDeliverer = d
}

// Run starts all background jobs and blocks until ctx is cancelled.
// Returns nil when all goroutines exit cleanly after context cancellation.
func (w *Worker) Run(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		return w.runLoop(ctx, "rollup", w.config.RollupInterval, func(ctx context.Context) {
			if _, err := w.rollup.ProcessBatch(ctx, w.config.RollupBatchSize); err != nil {
				w.logger.Error("worker: rollup batch failed", "error", err)
			}
		})
	})

	g.Go(func() error {
		return w.runLoop(ctx, "expiration", w.config.ExpirationInterval, func(ctx context.Context) {
			if _, err := w.expiration.ExpireStaleReservations(ctx, w.config.ExpirationBatchSize); err != nil {
				w.logger.Error("worker: expire reservations failed", "error", err)
			}
			if _, err := w.expiration.ExpireStaleOperations(ctx, w.config.ExpirationBatchSize); err != nil {
				w.logger.Error("worker: expire operations failed", "error", err)
			}
		})
	})

	g.Go(func() error {
		return w.runLoop(ctx, "reconcile", w.config.ReconcileInterval, func(ctx context.Context) {
			if _, err := w.reconcile.CheckAccountingEquation(ctx); err != nil {
				w.logger.Error("worker: reconcile failed", "error", err)
			}
		})
	})

	g.Go(func() error {
		return w.runLoop(ctx, "snapshot", w.config.SnapshotInterval, func(ctx context.Context) {
			yesterday := time.Now().AddDate(0, 0, -1)
			if err := w.snapshot.CreateDailySnapshot(ctx, yesterday); err != nil {
				w.logger.Error("worker: snapshot failed", "error", err)
			}
		})
	})

	g.Go(func() error {
		return w.runLoop(ctx, "system_rollup", w.config.SystemRollupInterval, func(ctx context.Context) {
			if err := w.systemRollup.RefreshSystemRollups(ctx); err != nil {
				w.logger.Error("worker: system rollup failed", "error", err)
			}
		})
	})

	if w.eventDeliverer != nil {
		g.Go(func() error {
			return w.runLoop(ctx, "event_delivery", w.config.EventDeliveryInterval, func(ctx context.Context) {
				if _, err := w.eventDeliverer.ProcessBatch(ctx, w.config.EventDeliveryBatchSize); err != nil {
					w.logger.Error("worker: event delivery failed", "error", err)
				}
			})
		})
	}

	return g.Wait()
}

// runLoop executes fn at the specified interval, exiting when ctx is done.
func (w *Worker) runLoop(ctx context.Context, name string, interval time.Duration, fn func(context.Context)) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	w.logger.Info("worker: started", "job", name, "interval", interval.String())

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("worker: stopped", "job", name)
			return nil
		case <-ticker.C:
			fn(ctx)
		}
	}
}
