package service

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
)

// lockAcquirer abstracts pg_try_advisory_lock so tests can substitute a fake.
//
// Session advisory locks live on one Postgres session: the unlock MUST run on
// the same connection that acquired the lock. A pooled QueryRow pair (one for
// lock, one for unlock) has no connection affinity — the unlock lands on a
// different connection, returns false, and the lock stays stuck on an idle
// pooled connection until it is recycled (~MaxConnLifetime), silently
// stalling the job fleet-wide. Hence acquire returns a release func bound to
// the acquiring session.
type lockAcquirer interface {
	// tryAdvisoryLock attempts the lock. acquired=false with nil err means
	// another holder owns it. On acquired=true, release must be called
	// exactly once to free the lock and its underlying resources.
	tryAdvisoryLock(ctx context.Context, key int64) (release func(ctx context.Context) error, acquired bool, err error)
}

// pgPoolLockAcquirer implements lockAcquirer by pinning a dedicated pool
// connection for the lifetime of the lock. Shared by LockedJob and
// SnapshotService.
type pgPoolLockAcquirer struct{ pool *pgxpool.Pool }

func (p *pgPoolLockAcquirer) tryAdvisoryLock(ctx context.Context, key int64) (func(context.Context) error, bool, error) {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("service: advisory lock: acquire conn: %w", err)
	}

	var acquired bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("service: advisory lock: pg_try_advisory_lock: %w", err)
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}

	release := func(ctx context.Context) error {
		var released bool
		if err := conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", key).Scan(&released); err != nil {
			// Unlock failed on the owning connection — do NOT return the
			// connection to the pool still holding the lock. Hijack and close
			// the session so Postgres drops every advisory lock it held.
			_ = conn.Hijack().Close(ctx)
			return fmt.Errorf("service: advisory lock: pg_advisory_unlock: %w", err)
		}
		conn.Release()
		if !released {
			return fmt.Errorf("service: advisory lock: pg_advisory_unlock returned false for key %d", key)
		}
		return nil
	}
	return release, true, nil
}

// LockedJob wraps a background job function with pg_try_advisory_lock semantics:
// if the lock cannot be acquired (another replica holds it), Run logs and returns
// immediately — only one pod runs the wrapped fn per tick.
//
// Lock keys are derived via advisoryLockKey("job:<name>") (FNV-64a).
type LockedJob struct {
	name    string
	lockKey int64
	fn      func(ctx context.Context) error
	locker  lockAcquirer // nil → skip locking (no pool configured)
	logger  core.Logger
	// metrics is core.NopMetrics() unless NewLockedJob is given a non-nil
	// one (I-M10). Exactly one of JobTickCompleted/JobTickFailed/
	// JobTickSkippedLocked is emitted per Run call.
	metrics core.Metrics
}

// NewLockedJob creates a LockedJob. When pool is nil, locking is skipped and fn
// runs unconditionally — suitable for single-instance deployments or tests.
// metrics may be nil, in which case Run's per-tick outcome is not reported
// (core.NopMetrics()).
func NewLockedJob(name string, fn func(ctx context.Context) error, pool *pgxpool.Pool, logger core.Logger, metrics core.Metrics) *LockedJob {
	if metrics == nil {
		metrics = core.NopMetrics()
	}
	lj := &LockedJob{
		name:    name,
		lockKey: advisoryLockKey("job:" + name),
		fn:      fn,
		logger:  logger,
		metrics: metrics,
	}
	if pool != nil {
		lj.locker = &pgPoolLockAcquirer{pool: pool}
	}
	return lj
}

// Run acquires the advisory lock, executes fn, then releases the lock. Lock
// acquisition errors fail closed: globally single-flight work must never run
// without the coordination primitive that makes it safe.
//
// Exactly one of JobTickCompleted / JobTickFailed / JobTickSkippedLocked is
// emitted per call (I-M10) -- a caller wrapping Run in its own scheduler must
// not ALSO account for completed/failed from Run's return value, or the same
// tick would be double-counted (see service/worker.go's runLoop, which
// leaves accounting to Run for every locked job and only adds its own
// panic-recovery layer around the call).
func (lj *LockedJob) Run(ctx context.Context) error {
	if lj.locker != nil {
		release, acquired, err := lj.locker.tryAdvisoryLock(ctx, lj.lockKey)
		if err != nil {
			lj.logger.Error("service: locked_job: advisory lock failed, skipping",
				"job", lj.name,
				"error", err,
			)
			lj.metrics.JobTickFailed(lj.name)
			return fmt.Errorf("service: locked_job %s: acquire advisory lock: %w", lj.name, err)
		} else if !acquired {
			lj.logger.Info("service: locked_job: advisory lock held by another replica, skipping",
				"job", lj.name,
			)
			lj.metrics.JobTickSkippedLocked(lj.name)
			return nil
		} else {
			defer func() {
				// ctx may already be cancelled (e.g. shutdown mid-job) —
				// release on a detached, short-lived context so the lock
				// doesn't leak until the session holding it disconnects.
				cleanupCtx, cancel := cleanupContext(ctx)
				defer cancel()
				if err := release(cleanupCtx); err != nil {
					lj.logger.Error("service: locked_job: release advisory lock failed",
						"job", lj.name,
						"error", err,
					)
				}
			}()
		}
	}

	if err := lj.fn(ctx); err != nil {
		lj.logger.Error("service: locked_job: fn failed", "job", lj.name, "error", err)
		lj.metrics.JobTickFailed(lj.name)
		return fmt.Errorf("service: locked_job %s: run: %w", lj.name, err)
	}
	lj.metrics.JobTickCompleted(lj.name)
	return nil
}
