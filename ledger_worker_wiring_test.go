package ledger_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/service"
)

// ---------------------------------------------------------------------------
// Test doubles: a logger and a metrics sink that record what they were given.
// ---------------------------------------------------------------------------

type recordingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *recordingLogger) record(level, msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf("%s %s %v", level, msg, args))
}

func (l *recordingLogger) Info(msg string, args ...any)  { l.record("INFO", msg, args...) }
func (l *recordingLogger) Warn(msg string, args ...any)  { l.record("WARN", msg, args...) }
func (l *recordingLogger) Error(msg string, args ...any) { l.record("ERROR", msg, args...) }

// contains reports whether any recorded line contains every one of subs.
func (l *recordingLogger) contains(subs ...string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		all := true
		for _, sub := range subs {
			if !strings.Contains(line, sub) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func (l *recordingLogger) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// recordingMetrics counts the one signal the full reconciliation suite emits
// per check. Embedding core.NoopMetrics is the documented way to implement a
// slice of the interface (core/metrics.go).
type recordingMetrics struct {
	core.NoopMetrics
	mu     sync.Mutex
	checks []string
}

func (m *recordingMetrics) ReconcileCheckResult(name string, _ bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checks = append(m.checks, name)
}

func (m *recordingMetrics) checkCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.checks)
}

// ---------------------------------------------------------------------------
// E-M1 / I-M11 / C-R3 — the default silent channel
// ---------------------------------------------------------------------------

// TestServiceWorker_RefusesToRunUnderTheDefaultSilentLogger pins the fix to
// the finding four territories reached independently: every worker signal
// this library produces travels over core.Logger, and ledger.New installs
// core.NopLogger by default, so a consumer following README's Quick Start
// verbatim got a process with literally zero output -- the startup report
// naming which optional jobs were skipped, the "attestation is running with
// no anchor" warning, and every job's per-tick failure all written into
// /dev/null. The previous round's remedy for "svc.Worker silently disables
// jobs" was that startup report; it was delivered over the channel that is
// off by default (working-agreements.md §3: a diagnostic nobody can receive
// is not a diagnostic).
//
// The wiring under test is README's Quick Start, character for character:
// ledger.New(pool) with no options, then svc.Worker(DefaultWorkerConfig()),
// then Run. It must now fail loudly and say what is missing.
func TestServiceWorker_RefusesToRunUnderTheDefaultSilentLogger(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	// README Quick Start, verbatim.
	svc, err := ledger.New(pool)
	require.NoError(t, err)
	worker, err := svc.Worker(service.DefaultWorkerConfig())
	require.NoError(t, err)

	runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	runErr := worker.Run(runCtx)

	require.Error(t, runErr,
		"a Worker built from ledger.New(pool) with no WithLogger must refuse to start: "+
			"running it produces a process indistinguishable from one that never started")
	require.ErrorIs(t, runErr, core.ErrInvalidInput)
	require.Contains(t, runErr.Error(), "ledger.WithLogger",
		"the error must name the option that fixes it")
	require.Contains(t, runErr.Error(), "WithSilentWorker",
		"the error must name the opt-out for consumers who mean it")
}

// TestServiceWorker_StartsAndReportsWhenALoggerIsInjected is the control for
// the test above -- without it, "Run returns an error" could be satisfied by
// a Worker that never starts under any configuration. It also asserts the
// content of the startup report, which is the line the previous round added
// and nothing ever checked was reachable.
func TestServiceWorker_StartsAndReportsWhenALoggerIsInjected(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	logger := &recordingLogger{}
	svc, err := ledger.New(pool, ledger.WithLogger(logger))
	require.NoError(t, err)

	cfg := service.DefaultWorkerConfig()
	worker, err := svc.Worker(cfg)
	require.NoError(t, err)

	runCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	require.NoError(t, worker.Run(runCtx))

	require.True(t, logger.contains("worker: starting", "event_delivery_webhook", "false"),
		"the startup report must reach the injected logger and say which optional jobs are OFF; got %v", logger.snapshot())
	require.True(t, logger.contains("worker: starting", "full_reconcile", "true"),
		"and which are ON; got %v", logger.snapshot())
	require.True(t, logger.contains("worker: starting", "leader_election", "true"),
		"leader election is a job-correctness property, not an implementation detail: it must be in the report; got %v", logger.snapshot())
}

// TestServiceWorker_StartupReportIsReadableWithoutALogger pins the other half
// of the fix (C-R3, and the "expose it in the report, not only in a log
// line" requirement): svc.Worker always wires the P6 attestation job with a
// nil anchor, so a WithAttestor deployment runs anchorless by default -- the
// chain advances and every batch is signed, but VerifyLedger cannot detect a
// wholesale history rewrite. That was reported by exactly one Warn line,
// discarded by the default logger. StartupReport answers it programmatically,
// with no logger involved at all.
func TestServiceWorker_StartupReportIsReadableWithoutALogger(t *testing.T) {
	pool := postgrestest.SetupDB(t)

	attestor, verifier := newTestAttestor(t, "startup-report-key")
	svc, err := ledger.New(pool, ledger.WithAttestor(attestor, verifier))
	require.NoError(t, err)

	worker, err := svc.Worker(service.DefaultWorkerConfig())
	require.NoError(t, err)

	report := worker.StartupReport()
	require.True(t, report.Attestation, "WithAttestor must auto-wire the batch attestation job")
	require.False(t, report.AttestationAnchor,
		"the facade wires the job with a nil anchor; the report has to say so rather than leave it to a Warn")
	require.True(t, report.LeaderElection)
	require.True(t, report.FullReconcile)
	require.True(t, report.Partition)

	var anchorWarning string
	for _, w := range report.Warnings {
		if strings.Contains(w, "no anchor configured") {
			anchorWarning = w
		}
	}
	require.NotEmpty(t, anchorWarning,
		"running anchorless is permitted but degraded; the report must carry the warning as data, not only as a log line: %v", report.Warnings)
}

// ---------------------------------------------------------------------------
// F-M1 — the two Worker() wiring lines that had no pin at all
// ---------------------------------------------------------------------------

// TestServiceWorker_WiresTheAdvisoryLockPool pins ledger.go's `w.SetPool(s.pool)`.
// Deleting that line used to leave the whole suite green while turning every
// one of the six LockedJobs (expiration, reconcile, system_rollup,
// full_reconcile, partition, attestation) into an unlocked job that runs on
// every replica each tick -- a silent fail-open, since NewLockedJob's contract
// with a nil pool is to skip locking rather than complain.
//
// service/locked_job_integration_test.go cannot catch it: it hands
// NewLockedJob a pool itself, testing whether locking works rather than
// whether the facade turned it on.
func TestServiceWorker_WiresTheAdvisoryLockPool(t *testing.T) {
	pool := postgrestest.SetupDB(t)

	svc, err := ledger.New(pool)
	require.NoError(t, err)
	worker, err := svc.Worker(service.DefaultWorkerConfig())
	require.NoError(t, err)

	report := worker.StartupReport()
	require.True(t, report.LeaderElection,
		"the Worker the facade builds must carry the connection pool: without it every advisory-locked "+
			"job silently degrades to running on every replica, with no error and no log difference")
	require.NotContains(t, strings.Join(report.Warnings, "\n"), "Worker.SetPool was never called")
}

// TestServiceWorker_ExtendsThePartitionHorizon pins ledger.go's
// `w.SetPartitionService(...)`. Deleting that line also used to leave the
// suite green: I-13's three pins all call PartitionStore.EnsureMonthlyPartitions
// directly, so both the service layer and the facade wiring could be removed
// without a single failure -- and the consequence is journal entries landing
// in journal_entries_default once the horizon runs out.
//
// This drives it end to end: a horizon far beyond the one migration created,
// a worker started through nothing but the facade, and an assertion against
// pg_class that the partition for that month now exists.
func TestServiceWorker_ExtendsThePartitionHorizon(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	const monthsAhead = 14 // migration's horizon is far shorter
	target := time.Now().UTC().AddDate(0, monthsAhead, 0)
	partition := fmt.Sprintf("journal_entries_y%04dm%02d", target.Year(), int(target.Month()))

	var existsBefore bool
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1)", partition,
	).Scan(&existsBefore))
	require.False(t, existsBefore,
		"control: %s must not exist yet, otherwise this test cannot tell the worker created it", partition)

	logger := &recordingLogger{}
	svc, err := ledger.New(pool, ledger.WithLogger(logger))
	require.NoError(t, err)

	cfg := service.DefaultWorkerConfig()
	cfg.PartitionInterval = 50 * time.Millisecond
	cfg.PartitionMonthsAhead = monthsAhead
	worker, err := svc.Worker(cfg)
	require.NoError(t, err)

	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker.Run(workerCtx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	require.Eventually(t, func() bool {
		var exists bool
		if err := pool.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1)", partition,
		).Scan(&exists); err != nil {
			return false
		}
		return exists
	}, 10*time.Second, 100*time.Millisecond,
		"the partition horizon was never extended to %s: ledger.Service.Worker is no longer wiring the "+
			"PartitionService, so journal entries will fall into journal_entries_default once the "+
			"migration-created horizon runs out. Logs: %v", partition, logger.snapshot())
}

// TestServiceWorker_WiresTheFullReconciler pins the auto-wiring this round
// adds. DefaultWorkerConfig has always set FullReconcileInterval, so the
// fifteen-check suite -- unauthorized_journals, checkpoint_balance,
// journal_dr_cr among them -- looked configured while nothing ever registered
// a reconciler for it to run: the whole suite was off for every library
// consumer, and the one line that would have said so went to the default
// no-op logger.
//
// The assertion is the suite's own per-check metric, not a wiring flag, so it
// fails if the reconciler is registered but never actually runs.
func TestServiceWorker_WiresTheFullReconciler(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	metrics := &recordingMetrics{}
	logger := &recordingLogger{}
	svc, err := ledger.New(pool, ledger.WithLogger(logger), ledger.WithMetrics(metrics))
	require.NoError(t, err)

	cfg := service.DefaultWorkerConfig()
	cfg.FullReconcileInterval = 50 * time.Millisecond
	worker, err := svc.Worker(cfg)
	require.NoError(t, err)
	require.True(t, worker.StartupReport().FullReconcile)

	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker.Run(workerCtx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	require.Eventually(t, func() bool { return metrics.checkCount() > 0 }, 10*time.Second, 100*time.Millisecond,
		"the full reconciliation suite never reported a single check result: ledger.Service.Worker is not "+
			"wiring SetFullReconciler, so every check it runs -- including unauthorized_journals -- is off "+
			"by default while DefaultWorkerConfig advertises an interval for them. Logs: %v", logger.snapshot())
}

// TestServiceWorker_SubscribeAfterRunIsAnError pins the upgrade of the
// "Subscribe must precede Run" rule from an Error log line to a returned
// error. The log line was unreachable under the default logger, so violating
// a documented ordering constraint produced a handler that was never invoked
// and events that stayed pending forever, with nothing anywhere to notice.
func TestServiceWorker_SubscribeAfterRunIsAnError(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	logger := &recordingLogger{}
	svc, err := ledger.New(pool, ledger.WithLogger(logger))
	require.NoError(t, err)

	worker, err := svc.Worker(service.DefaultWorkerConfig())
	require.NoError(t, err)

	// Control: before Run, subscribing is fine.
	require.NoError(t, worker.Subscribe(func(context.Context, core.Event) error { return nil }))

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// A second Worker, so the "after Run" case is not masked by the
	// dispatcher the control call above already created.
	late, err := svc.Worker(service.DefaultWorkerConfig())
	require.NoError(t, err)
	lateCtx, cancelLate := context.WithCancel(ctx)
	lateDone := make(chan error, 1)
	go func() { lateDone <- late.Run(lateCtx) }()
	t.Cleanup(func() {
		cancelLate()
		<-lateDone
	})

	require.Eventually(t, func() bool {
		return late.Subscribe(func(context.Context, core.Event) error { return nil }) != nil
	}, 5*time.Second, 50*time.Millisecond,
		"Subscribe after Run must return an error: the event_callback loop was never started, so the "+
			"handler would never be invoked and the events would stay pending forever")
}
