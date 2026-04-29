package service

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/service/delivery"
)

// WorkerConfig holds configuration for the background Worker.
type WorkerConfig struct {
	RollupInterval         time.Duration // default: 5s
	RollupBatchSize        int           // default: 100
	RollupClaimLease       time.Duration // default: 2m
	ExpirationInterval     time.Duration // default: 30s
	ExpirationBatchSize    int           // default: 50
	ReconcileInterval      time.Duration // default: 6h
	SnapshotInterval       time.Duration // default: 24h
	SystemRollupInterval   time.Duration // default: 1m
	EventDeliveryInterval  time.Duration // default: 5s
	EventDeliveryBatchSize int           // default: 100
	EventClaimLease        time.Duration // default: 2m
}

// DefaultWorkerConfig returns the default WorkerConfig.
func DefaultWorkerConfig() WorkerConfig {
	return WorkerConfig{
		RollupInterval:         5 * time.Second,
		RollupBatchSize:        100,
		RollupClaimLease:       2 * time.Minute,
		ExpirationInterval:     30 * time.Second,
		ExpirationBatchSize:    50,
		ReconcileInterval:      6 * time.Hour,
		SnapshotInterval:       24 * time.Hour,
		SystemRollupInterval:   time.Minute,
		EventDeliveryInterval:  5 * time.Second,
		EventDeliveryBatchSize: 100,
		EventClaimLease:        2 * time.Minute,
	}
}

// EventBatchProcessor processes a batch of pending events.
// Implemented by delivery.WebhookDeliverer and delivery.LocalDispatcher.
type EventBatchProcessor interface {
	ProcessBatch(ctx context.Context, batchSize int) (int, error)
}

// Worker runs background jobs on configurable intervals.
type Worker struct {
	rollup                 *RollupService
	expiration             *ExpirationService
	reconcile              *ReconciliationService
	snapshot               *SnapshotService
	systemRollup           *SystemRollupService
	eventDeliverer         EventBatchProcessor // nil = skip webhook delivery (library mode)
	localDeliverer         *delivery.LocalDispatcher
	pool                   *pgxpool.Pool // nil = no advisory locks (single-replica mode)
	config                 WorkerConfig
	logger                 core.Logger
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

// SetPool attaches a *pgxpool.Pool used for pg_try_advisory_lock-based leader
// election on the reconcile and system_rollup jobs.  When nil (the default),
// those jobs run on every pod — safe for single-replica deployments.
func (w *Worker) SetPool(pool *pgxpool.Pool) {
	w.pool = pool
}

// Subscribe registers an in-process handler that receives every emitted event.
// Handlers are invoked from a background poll loop ("event_callback").  If a
// handler returns an error the event is logged and still marked delivered —
// blocking the queue on a buggy handler is worse than a missed notification.
//
// Subscribe wires a delivery.LocalDispatcher the first time it is called.
// localPoller must be non-nil when Subscribe is used; pass it via
// SetLocalPoller before calling Run.
func (w *Worker) Subscribe(handler func(context.Context, core.Event) error) {
	if w.localDeliverer == nil {
		// Lazily create — the poller will be set when SetLocalPoller is called.
		// If the caller never sets a poller, ProcessBatch will return an error
		// on the first tick, which the worker will log but not crash on.
		w.localDeliverer = delivery.NewLocalDispatcher(nil, w.logger)
	}
	w.localDeliverer.OnEvent(handler)
}

// SetLocalPoller wires the EventPoller that backs the in-process event
// subscription loop.  Must be called before Run() when Subscribe() is used.
func (w *Worker) SetLocalPoller(poller delivery.EventPoller) {
	if w.localDeliverer == nil {
		w.localDeliverer = delivery.NewLocalDispatcher(poller, w.logger)
	} else {
		w.localDeliverer.SetPoller(poller)
	}
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
			if _, err := w.expiration.ExpireStaleBookings(ctx, w.config.ExpirationBatchSize); err != nil {
				w.logger.Error("worker: expire bookings failed", "error", err)
			}
		})
	})

	// reconcile — advisory-locked so only one replica runs per tick.
	reconcileJob := NewLockedJob("reconcile", func(ctx context.Context) error {
		_, err := w.reconcile.CheckAccountingEquation(ctx)
		return err
	}, w.pool, w.logger)
	g.Go(func() error {
		return w.runLoop(ctx, "reconcile", w.config.ReconcileInterval, func(ctx context.Context) {
			reconcileJob.Run(ctx)
		})
	})

	// snapshot — advisory lock is handled inside CreateDailySnapshot via WithPool.
	g.Go(func() error {
		return w.runLoop(ctx, "snapshot", w.config.SnapshotInterval, func(ctx context.Context) {
			yesterday := time.Now().UTC().AddDate(0, 0, -1)
			if err := w.snapshot.CreateDailySnapshot(ctx, yesterday); err != nil {
				w.logger.Error("worker: snapshot failed", "error", err)
			}
		})
	})

	// system_rollup — advisory-locked so only one replica runs per tick.
	sysRollupJob := NewLockedJob("system_rollup", func(ctx context.Context) error {
		return w.systemRollup.RefreshSystemRollups(ctx)
	}, w.pool, w.logger)
	g.Go(func() error {
		return w.runLoop(ctx, "system_rollup", w.config.SystemRollupInterval, func(ctx context.Context) {
			sysRollupJob.Run(ctx)
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

	if w.localDeliverer != nil {
		g.Go(func() error {
			return w.runLoop(ctx, "event_callback", w.config.EventDeliveryInterval, func(ctx context.Context) {
				if _, err := w.localDeliverer.ProcessBatch(ctx, w.config.EventDeliveryBatchSize); err != nil {
					w.logger.Error("worker: event callback delivery failed", "error", err)
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
