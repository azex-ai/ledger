package service

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
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
	// mu guards localDeliverer, which Subscribe writes and Run reads. running
	// being atomic never protected the field it exists to talk about: the
	// documented "Subscribe before Run" ordering makes a violation a genuine
	// data race under -race, not merely a subscription that quietly does
	// nothing (consumer-surface handoff to concurrency, 2026-09-02).
	mu             sync.Mutex
	localDeliverer *delivery.LocalDispatcher
	// localPoller is held separately from localDeliverer so that wiring a
	// poller does not by itself start the callback loop. The loop must exist
	// only when someone has actually subscribed: LocalDispatcher marks every
	// event it polls as delivered, and with no handlers registered that is a
	// silent drain of the delivery queue.
	localPoller   delivery.EventPoller
	fullReconcile core.FullReconciler // nil = skip the full reconciliation suite job
	partition     *PartitionService   // nil = skip partition management
	attestation   *AttestationService // nil = skip the P6 batch attestation job
	pool          *pgxpool.Pool       // nil = no advisory locks (single-replica mode)
	config        WorkerConfig
	logger        core.Logger
	metrics       core.Metrics
	// allowSilent opts out of Run's refusal to start under core.NopLogger --
	// see AllowSilent.
	allowSilent bool
	// running is set by Run and never cleared: Run reads localDeliverer once
	// at startup to decide whether the event_callback loop exists at all, so
	// a first Subscribe after Run registers a handler nothing will ever
	// invoke. Subscribe uses this to make that mistake loud instead of
	// silent.
	running atomic.Bool
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
		metrics:      engine.Metrics(),
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
//
// ledger.Service.Worker calls this automatically (with a nil Anchor) when
// the Service was constructed WithAttestor, so most callers never need to
// call it directly. Call it yourself only to override that default -- e.g.
// to supply a real core.Anchor for external publication -- in which case
// your call after Worker() returns simply replaces the auto-wired one.
func (w *Worker) SetAttestor(a *AttestationService) {
	w.attestation = a
}

// SetPool attaches a *pgxpool.Pool used for pg_try_advisory_lock-based leader
// election on every LockedJob this Worker runs: expiration, reconcile,
// system_rollup, full_reconcile, partition and attestation (six, not the two
// this comment used to name). When nil (the default), NewLockedJob leaves its
// locker nil and all six run on every pod — safe for single-replica
// deployments, silently fail-open for everything else, which is why
// StartupReport surfaces it as LeaderElection.
func (w *Worker) SetPool(pool *pgxpool.Pool) {
	w.pool = pool
}

// AllowSilent opts this Worker out of Run's refusal to start when its logger
// is core.NopLogger (the library default). Only call it when the absence of
// every worker log line is a deliberate choice — a test that asserts on
// something other than logs, or a deployment that reads the StartupReport
// programmatically instead.
//
// ledger.WithSilentWorker is the facade-level way to reach this.
func (w *Worker) AllowSilent() {
	w.allowSilent = true
}

// Subscribe registers an in-process handler that receives every emitted event.
// Handlers are invoked from a background poll loop ("event_callback"), with
// the same at-least-once semantics as webhook delivery
// (delivery.WebhookDeliverer): if a handler returns an error the event is
// logged and scheduled for retry (bounded by max_attempts, after which it is
// marked 'dead') rather than marked delivered — blocking the queue on a
// buggy handler is worse than a delayed retry, but it also means a handler
// that errors after doing partial work WILL be invoked again for the same
// event. Handlers must therefore be idempotent per event UID; do not write a
// handler that assumes "returns an error" means "had no effect".
//
// Subscribe wires a delivery.LocalDispatcher the first time it is called,
// using the poller already set by SetLocalPoller. ledger.Service.Worker sets
// one, so a library consumer does not have to.
//
// ORDERING: the first Subscribe must happen BEFORE Run, and Subscribe
// returns an error when it does not. Run reads localDeliverer once at startup
// to decide whether the event_callback loop exists at all — a first Subscribe
// after Run registers a handler that nothing will ever invoke, and events
// simply stay pending. This used to be an Error log line only; under the
// library's default core.NopLogger that line went nowhere, so the wiring bug
// it was meant to report was invisible (working-agreements.md §3: a
// diagnostic delivered over a channel that is off by default is not a
// diagnostic). The handler is not registered when this returns an error.
func (w *Worker) Subscribe(handler func(context.Context, core.Event) error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.localDeliverer == nil {
		if w.running.Load() {
			return fmt.Errorf("service: worker: Subscribe called after Run: the event_callback loop was never started, so this handler would never be invoked and events would stay pending forever -- subscribe before starting the worker: %w", core.ErrInvalidInput)
		}
		w.localDeliverer = delivery.NewLocalDispatcher(w.localPoller, w.logger, w.metrics)
	}
	w.localDeliverer.OnEvent(handler)
	return nil
}

// SetLocalPoller wires the EventPoller that backs the in-process event
// subscription loop. It deliberately does NOT create the dispatcher: doing so
// would start the callback loop for a worker nobody subscribed to, and that
// loop marks every event it polls as delivered, which would silently drain
// the queue webhook delivery reads from. The dispatcher is created by
// Subscribe, which is the point at which a handler exists to receive events.
func (w *Worker) SetLocalPoller(poller delivery.EventPoller) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.localPoller = poller
	if w.localDeliverer != nil {
		w.localDeliverer.SetPoller(poller)
	}
}

// StartupReport is the machine-readable form of what Run logs at startup:
// which optional jobs this Worker will actually run, and which
// deliberately-permitted degraded modes it is in. Callers can read it before
// (or instead of) Run, so "is this job on?" and "is the attestation chain
// externally witnessed?" are answerable without a logger — the log line alone
// used to be the only answer, and under the library's default core.NopLogger
// there was no answer at all (working-agreements.md §3).
type StartupReport struct {
	FullReconcile              bool `json:"full_reconcile"`
	EventDeliveryWebhook       bool `json:"event_delivery_webhook"`
	EventDeliveryLocalCallback bool `json:"event_delivery_local_callback"`
	Attestation                bool `json:"attestation"`
	// AttestationAnchor is false when the batch attestation job runs without
	// an external anchor: the chain still advances and every batch is still
	// signed, but VerifyLedger cannot detect a wholesale history rewrite.
	// Attestation && !AttestationAnchor is the shape ledger.Service.Worker
	// auto-wires by default.
	AttestationAnchor bool `json:"attestation_anchor"`
	// AttestationAnchorType is the Go type of the configured anchor
	// ("*anchordev.LocalFileAnchor", "*r2.Anchor", ...), or "" when there is
	// none. The boolean above cannot distinguish a production carrier from
	// anchordev's local file, and that file is on the same host as the
	// database it exists to be independent of -- so "anchored" was reported
	// identically for a real external witness and for no witness at all
	// (2026-09-02 audit, tamper-evident.md m-1). Reported as a type name
	// rather than a category so it stays true for carriers this library has
	// never heard of.
	AttestationAnchorType string `json:"attestation_anchor_type"`
	Partition             bool   `json:"partition"`
	// LeaderElection reports whether a *pgxpool.Pool was attached (SetPool),
	// which is what makes every LockedJob single-runner across replicas.
	// False means all six locked jobs run on every replica each tick.
	LeaderElection bool `json:"leader_election"`
	// Warnings lists degraded-but-permitted states in the same words Run
	// logs them. Empty means nothing to report.
	Warnings []string `json:"warnings"`
}

// StartupReport describes what Run will do with this Worker's current wiring.
// Safe to call before Run; Run calls it itself and logs the result.
func (w *Worker) StartupReport() StartupReport {
	w.mu.Lock()
	localDeliverer := w.localDeliverer
	w.mu.Unlock()

	r := StartupReport{
		FullReconcile:              w.fullReconcile != nil,
		EventDeliveryWebhook:       w.eventDeliverer != nil,
		EventDeliveryLocalCallback: localDeliverer != nil,
		Attestation:                w.attestation != nil,
		AttestationAnchor:          w.attestation != nil && w.attestation.anchor != nil,
		AttestationAnchorType:      attestationAnchorTypeName(w),
		Partition:                  w.partition != nil,
		LeaderElection:             w.pool != nil,
	}
	if r.Attestation && !r.AttestationAnchor {
		r.Warnings = append(r.Warnings, anchorlessAttestationWarning)
	}
	if !r.LeaderElection {
		r.Warnings = append(r.Warnings, noLeaderElectionWarning)
	}
	if strings.HasPrefix(r.AttestationAnchorType, devAnchorTypePrefix) {
		r.Warnings = append(r.Warnings, developmentAnchorWarning+r.AttestationAnchorType)
	}
	return r
}

// attestationAnchorTypeName reports the configured anchor's Go type, or ""
// when no anchor is configured.
func attestationAnchorTypeName(w *Worker) string {
	if w.attestation == nil || w.attestation.anchor == nil {
		return ""
	}
	return fmt.Sprintf("%T", w.attestation.anchor)
}

// devAnchorTypePrefix recognises this library's own dev-only anchor by type
// NAME rather than by importing it: service is a domain package and must not
// depend on a dev adapter (abstractions.md -- the dependency would also be
// backwards, since anchordev is one of core.Anchor's implementations). A
// string prefix is a weak coupling, and deliberately so: if anchordev is ever
// renamed this check silently stops matching, which costs a warning, whereas
// the import would cost the layering.
const devAnchorTypePrefix = "*anchordev."

const developmentAnchorWarning = "worker: the configured attestation anchor is this library's DEV-ONLY " +
	"local-file anchor -- it lives on the same host as the ledger's own database, so it cannot " +
	"witness a history rewrite performed by whoever holds that database (design doc §8.3 point 1). " +
	"Production needs a carrier the database credentials cannot reach. Anchor type: "

const anchorlessAttestationWarning = "worker: batch attestation is running with no anchor configured -- " +
	"the batch chain will advance and every batch will be signed, but VerifyLedger " +
	"cannot detect a wholesale history rewrite until an anchor is wired in via " +
	"Worker.SetAttestor (see service/attestation.go's anchor field comment)"

const noLeaderElectionWarning = "worker: no connection pool attached (Worker.SetPool was never called) -- " +
	"every advisory-locked job (expiration, reconcile, system_rollup, full_reconcile, " +
	"partition, attestation) will run on every replica each tick, which is safe only " +
	"for a single-replica deployment"

// Run starts all background jobs and blocks until ctx is cancelled.
// Returns nil when all goroutines exit cleanly after context cancellation.
//
// Returns an error WITHOUT starting anything when this Worker's logger is
// core.NopLogger (what ledger.New installs unless the consumer passes
// ledger.WithLogger) and AllowSilent was not called. Everything below --
// the startup report, the anchorless-attestation warning, every job's
// per-tick failure -- reaches the operator over that logger and nowhere
// else, so booting under the silent default produces a worker that is
// indistinguishable, from outside, from one that never started. Refusing at
// startup is the fail-closed reading of working-agreements.md §3; opt out
// with ledger.WithSilentWorker / Worker.AllowSilent when silence is a
// deliberate choice.
//
// Four jobs -- full reconciliation suite, webhook event delivery, local
// event-callback delivery, and P6 batch attestation -- only start when the
// corresponding Set* method was called first; Run logs, once, which of them
// are enabled so "this job never ran" is a line a consumer can grep for at
// startup instead of an absence they have to notice on their own
// (working-agreements.md §3: a skipped optional job and a running one must
// never look the same from the outside).
//
// m-7 (2026-08-26 independent review, third pass): "attestation": true only
// ever meant "the batch chain will advance and every batch will be signed"
// -- it says nothing about whether an anchor is wired in, and
// Service.Worker's auto-wiring (ledger.go) always leaves anchor nil unless
// the caller overrides it with their own SetAttestor afterward. An anchor is
// the only thing that lets VerifyLedger detect a wholesale DB-level history
// rewrite (service/attestation.go's own field comment); without one, the
// chain is still internally self-consistent but has no external witness. A
// deployment running anchorless for months would see "attestation": true at
// every restart and have no log-level signal that the one property anchoring
// exists to provide was never actually in force -- degraded looking
// identical to full is the same class of gap this doc comment's own
// parenthetical warns against. "attestation_anchor" makes that distinction
// visible, and the Warn (mirroring DevCreditEnabled's treatment of another
// deliberately-permitted-but-worth-flagging state) makes it noisy at every
// startup rather than something only a careful reader of the Info line would
// catch. VerifyLedger itself is unaffected and already correctly fail-closed
// (service/attest_verify.go: nil anchor -> VerifyStatusNotRun) -- this only
// changes what the operator sees before anything goes wrong.
func (w *Worker) Run(ctx context.Context) error {
	report := w.StartupReport()
	if core.IsNopLogger(w.logger) && !w.allowSilent {
		return fmt.Errorf("service: worker: refusing to start with the default silent logger: "+
			"the startup report (%+v), the anchorless-attestation warning and every job's "+
			"per-tick failure are only ever reported through core.Logger, so this worker would "+
			"run with no way to tell it apart from one that never started -- pass "+
			"ledger.WithLogger(...) at ledger.New, or opt into silence explicitly with "+
			"ledger.WithSilentWorker() / Worker.AllowSilent(): %w", report, core.ErrInvalidInput)
	}

	w.running.Store(true)
	w.logger.Info("worker: starting",
		"full_reconcile", report.FullReconcile,
		"event_delivery_webhook", report.EventDeliveryWebhook,
		"event_delivery_local_callback", report.EventDeliveryLocalCallback,
		"attestation", report.Attestation,
		"attestation_anchor", report.AttestationAnchor,
		"attestation_anchor_type", report.AttestationAnchorType,
		"partition", report.Partition,
		"leader_election", report.LeaderElection,
	)
	for _, warning := range report.Warnings {
		w.logger.Warn(warning)
	}

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		return w.runLoop(ctx, "rollup", w.config.RollupInterval, func(ctx context.Context) {
			if _, err := w.rollup.ProcessBatch(ctx, w.config.RollupBatchSize); err != nil {
				w.logger.Error("worker: rollup batch failed", "error", err)
				w.metrics.JobTickFailed("rollup")
				return
			}
			w.metrics.JobTickCompleted("rollup")
		})
	})

	// expiration — advisory-locked so only one replica runs per tick
	// (concurrency.md Major: this was the one background job among
	// {reconcile, system_rollup, full_reconcile, partition, attestation,
	// expiration} that ran unconditionally on every replica. Without
	// leader election, K replicas racing GetExpiredReservations/
	// ListExpiredBookings on the same tick all read the same expired batch
	// and each call Release/Transition on it; the row lock inside
	// Release/Transition serializes the writes so nothing corrupts, but
	// K-1 of every K calls fail with ErrInvalidTransition and get logged as
	// errors, drowning out genuine failures in noise. Wrapping in
	// NewLockedJob, like its five siblings, means only one replica ever
	// runs the batch per tick).
	expirationJob := NewLockedJob("expiration", func(ctx context.Context) error {
		// Errors from either sweep are logged here, individually, rather than
		// returned: the two operate on independent domains (reservations vs
		// bookings) and one failing must not skip the other. LockedJob.Run
		// therefore always sees nil from this closure and always counts the
		// tick as JobTickCompleted -- each individual Release/FinalizeSettlement
		// this sweep drives is still separately counted (ReserveReleased /
		// ReserveSettled), because those are emitted by postgres.ReserverStore
		// itself (I-M1), not by this closure.
		if _, err := w.expiration.ExpireStaleReservations(ctx, w.config.ExpirationBatchSize); err != nil {
			w.logger.Error("worker: expire reservations failed", "error", err)
		}
		if _, err := w.expiration.ExpireStaleBookings(ctx, w.config.ExpirationBatchSize); err != nil {
			w.logger.Error("worker: expire bookings failed", "error", err)
		}
		return nil
	}, w.pool, w.logger, w.metrics)
	g.Go(func() error {
		return w.runLoop(ctx, "expiration", w.config.ExpirationInterval, func(ctx context.Context) {
			if err := expirationJob.Run(ctx); err != nil {
				w.logger.Error("worker: expiration job failed", "error", err)
			}
		})
	})

	// reconcile — advisory-locked so only one replica runs per tick.
	reconcileJob := NewLockedJob("reconcile", func(ctx context.Context) error {
		_, err := w.reconcile.CheckAccountingEquation(ctx)
		return err
	}, w.pool, w.logger, w.metrics)
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
				w.metrics.JobTickFailed("snapshot")
				return
			}
			w.metrics.JobTickCompleted("snapshot")
		})
	})

	// system_rollup — advisory-locked so only one replica runs per tick.
	sysRollupJob := NewLockedJob("system_rollup", func(ctx context.Context) error {
		return w.systemRollup.RefreshSystemRollups(ctx)
	}, w.pool, w.logger, w.metrics)
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
		}, w.pool, w.logger, w.metrics)
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
		}, w.pool, w.logger, w.metrics)
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
		}, w.pool, w.logger, w.metrics)
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
					w.metrics.JobTickFailed("event_delivery")
					return
				}
				w.metrics.JobTickCompleted("event_delivery")
			})
		})
	}

	if report.EventDeliveryLocalCallback {
		// Read once, under the same lock Subscribe writes through, and close
		// over the value: the loop must not race with a (now rejected, but
		// still possible from a Worker built by hand) later Subscribe.
		w.mu.Lock()
		localDeliverer := w.localDeliverer
		w.mu.Unlock()
		g.Go(func() error {
			return w.runLoop(ctx, "event_callback", w.config.EventDeliveryInterval, func(ctx context.Context) {
				if _, err := localDeliverer.ProcessBatch(ctx, w.config.EventDeliveryBatchSize); err != nil {
					w.logger.Error("worker: event callback delivery failed", "error", err)
					w.metrics.JobTickFailed("event_callback")
					return
				}
				w.metrics.JobTickCompleted("event_callback")
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
			w.safeRun(ctx, name, fn)
		}
	}
}

// safeRun executes fn, recovering any panic so a single job's bug cannot take
// down the whole Worker -- or, since Run is typically launched with `go
// worker.Run(ctx)`, the whole process (I-M9). Before this, a panicking
// Subscribe handler or job function propagated straight through errgroup.Wait
// and out of Run.
//
// Completed/failed accounting for the tick itself is each job closure's own
// responsibility (see the Run's doc comment on LockedJob for why this cannot
// be centralized here without double-counting); safeRun only ever adds the
// JobPanicked signal, which nothing else emits.
func (w *Worker) safeRun(ctx context.Context, name string, fn func(context.Context)) {
	defer func() {
		if r := recover(); r != nil {
			w.logger.Error("worker: job panicked",
				"job", name,
				"panic", fmt.Sprintf("%v", r),
				"stack", string(debug.Stack()),
			)
			w.metrics.JobPanicked(name)
		}
	}()
	fn(ctx)
}
