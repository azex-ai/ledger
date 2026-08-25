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
	// FullReconcileInterval controls how often the full
	// reconciliation suite runs. Only takes effect when a FullReconciler has
	// been registered via SetFullReconciler — nil by default (skip job),
	// mirroring the EventDeliverer pattern. default: 1h
	FullReconcileInterval time.Duration
	// PartitionInterval controls how often the journal_entries monthly
	// partition horizon is extended. Only takes effect when a
	// PartitionService has been registered via SetPartitionService.
	// default: 12h
	PartitionInterval time.Duration
	// PartitionMonthsAhead is how many future months of partitions to keep
	// pre-created. default: 3
	PartitionMonthsAhead int
	// AttestInterval controls how often the P6 batch attestation job runs.
	// Only takes effect when an AttestationService has been registered via
	// SetAttestor -- nil by default (skip job), mirroring the
	// FullReconciler pattern. default: 60s
	AttestInterval time.Duration
	// AttestBatchSize is the max entries one attestation batch covers.
	// default: 1000
	AttestBatchSize int32
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
		FullReconcileInterval:  time.Hour,
		PartitionInterval:      12 * time.Hour,
		PartitionMonthsAhead:   3,
		AttestInterval:         60 * time.Second,
		AttestBatchSize:        1000,
	}
}

// EventBatchProcessor processes a batch of pending events.
// Implemented by delivery.WebhookDeliverer and delivery.LocalDispatcher.
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
	eventDeliverer EventBatchProcessor // nil = skip webhook delivery (library mode)
	localDeliverer *delivery.LocalDispatcher
	// localPoller is held separately from localDeliverer so that wiring a
	// poller does not by itself start the callback loop. The loop must exist
	// only when someone has actually subscribed: LocalDispatcher marks every
	// event it polls as delivered, and with no handlers registered that is a
	// silent drain of the delivery queue.
	localPoller delivery.EventPoller
	fullReconcile  core.FullReconciler // nil = skip the full reconciliation suite job
	partition      *PartitionService   // nil = skip partition management
	attestation    *AttestationService // nil = skip the P6 batch attestation job
	pool           *pgxpool.Pool       // nil = no advisory locks (single-replica mode)
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

// SetFullReconciler registers a core.FullReconciler to run the full
// reconciliation suite on FullReconcileInterval. If not set, the job
// is skipped entirely — the lightweight CheckAccountingEquation job (see
// ReconcileInterval) still runs regardless. Typically built via
// (*ledger.Service).FullReconciler and wired in by the service-mode entry
// point, mirroring SetEventDeliverer.
func (w *Worker) SetFullReconciler(fr core.FullReconciler) {
	w.fullReconcile = fr
}

// SetPartitionService registers the journal_entries partition manager so the
// worker keeps the monthly partition horizon ahead of the clock
// (PartitionInterval / PartitionMonthsAhead). If not set, the job is
// skipped — sensible for tests and for deployments that manage partitions
// externally.
func (w *Worker) SetPartitionService(p *PartitionService) {
	w.partition = p
}

// SetAttestor registers the P6 batch attestation service to run on
// AttestInterval. If not set, the job is skipped entirely -- mirroring
// SetFullReconciler; unlike PostJournal's per-call nil-Attestor tolerance,
// there is no partial mode here because the whole job either runs or
// doesn't.
func (w *Worker) SetAttestor(a *AttestationService) {
	w.attestation = a
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
// Subscribe wires a delivery.LocalDispatcher the first time it is called,
// using the poller already set by SetLocalPoller. ledger.Service.Worker sets
// one, so a library consumer does not have to.
func (w *Worker) Subscribe(handler func(context.Context, core.Event) error) {
	if w.localDeliverer == nil {
		w.localDeliverer = delivery.NewLocalDispatcher(w.localPoller, w.logger)
	}
	w.localDeliverer.OnEvent(handler)
}

// SetLocalPoller wires the EventPoller that backs the in-process event
// subscription loop. It deliberately does NOT create the dispatcher: doing so
// would start the callback loop for a worker nobody subscribed to, and that
// loop marks every event it polls as delivered, which would silently drain
// the queue webhook delivery reads from. The dispatcher is created by
// Subscribe, which is the point at which a handler exists to receive events.
func (w *Worker) SetLocalPoller(poller delivery.EventPoller) {
	w.localPoller = poller
	if w.localDeliverer != nil {
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
			if err := reconcileJob.Run(ctx); err != nil {
				w.logger.Error("worker: reconcile job failed", "error", err)
			}
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
			if err := sysRollupJob.Run(ctx); err != nil {
				w.logger.Error("worker: system rollup job failed", "error", err)
			}
		})
	})

	if w.fullReconcile != nil {
		// Advisory-locked so only one replica runs the fleet-wide scan per tick.
		fullReconcileJob := NewLockedJob("full_reconcile", func(ctx context.Context) error {
			_, err := w.fullReconcile.RunFullReconciliation(ctx)
			return err
		}, w.pool, w.logger)
		g.Go(func() error {
			return w.runLoop(ctx, "full_reconcile", w.config.FullReconcileInterval, func(ctx context.Context) {
				if err := fullReconcileJob.Run(ctx); err != nil {
					w.logger.Error("worker: full reconcile job failed", "error", err)
				}
			})
		})
	}

	if w.partition != nil {
		// Advisory-locked: partition DDL must run on a single replica.
		partitionJob := NewLockedJob("partition", func(ctx context.Context) error {
			return w.partition.EnsureUpcoming(ctx, time.Now(), w.config.PartitionMonthsAhead)
		}, w.pool, w.logger)
		g.Go(func() error {
			return w.runLoop(ctx, "partition", w.config.PartitionInterval, func(ctx context.Context) {
				if err := partitionJob.Run(ctx); err != nil {
					w.logger.Error("worker: partition job failed", "error", err)
				}
			})
		})
	}

	if w.attestation != nil {
		// Advisory-locked so only one replica extends the attestation
		// chain per tick -- concurrent runs would race on "what is the
		// next seq" (see AttestationService.RunAttestBatch's doc comment).
		attestJob := NewLockedJob("attestation", func(ctx context.Context) error {
			_, _, err := w.attestation.RunAttestBatch(ctx, w.config.AttestBatchSize)
			return err
		}, w.pool, w.logger)
		g.Go(func() error {
			return w.runLoop(ctx, "attestation", w.config.AttestInterval, func(ctx context.Context) {
				if err := attestJob.Run(ctx); err != nil {
					w.logger.Error("worker: attestation job failed", "error", err)
				}
			})
		})
	}

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
//
// A non-positive interval would crash time.NewTicker; defending here means a
// caller that bypassed the facade (which fills defaults via mergeWorkerConfig)
// only loses the loop, not the whole worker.
func (w *Worker) runLoop(ctx context.Context, name string, interval time.Duration, fn func(context.Context)) error {
	if interval <= 0 {
		w.logger.Warn("worker: skipping job: interval is non-positive", "job", name, "interval", interval.String())
		<-ctx.Done()
		return nil
	}

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
