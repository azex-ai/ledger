package service

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
)

// lockAcquirer abstracts pg_try_advisory_lock so tests can substitute a fake.
type lockAcquirer interface {
	tryAdvisoryLock(ctx context.Context, key int64) (bool, error)
	releaseAdvisoryLock(ctx context.Context, key int64) error
}

// pgPoolLockAcquirer implements lockAcquirer using a *pgxpool.Pool.
// This is separate from snapshot's pgAdvisoryLock to keep each file self-contained.
type pgPoolLockAcquirer struct{ pool *pgxpool.Pool }

func (p *pgPoolLockAcquirer) tryAdvisoryLock(ctx context.Context, key int64) (bool, error) {
	var acquired bool
	if err := p.pool.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		return false, fmt.Errorf("service: locked_job: pg_try_advisory_lock: %w", err)
	}
	return acquired, nil
}

func (p *pgPoolLockAcquirer) releaseAdvisoryLock(ctx context.Context, key int64) error {
	var released bool
	if err := p.pool.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", key).Scan(&released); err != nil {
		return fmt.Errorf("service: locked_job: pg_advisory_unlock: %w", err)
	}
	if !released {
		return fmt.Errorf("service: locked_job: pg_advisory_unlock returned false for key %d", key)
	}
	return nil
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
}

// NewLockedJob creates a LockedJob. When pool is nil, locking is skipped and fn
// runs unconditionally — suitable for single-instance deployments or tests.
func NewLockedJob(name string, fn func(ctx context.Context) error, pool *pgxpool.Pool, logger core.Logger) *LockedJob {
	lj := &LockedJob{
		name:    name,
		lockKey: advisoryLockKey("job:" + name),
		fn:      fn,
		logger:  logger,
	}
	if pool != nil {
		lj.locker = &pgPoolLockAcquirer{pool: pool}
	}
	return lj
}

// Run acquires the advisory lock, executes fn, then releases the lock.
// If the lock is already held, it logs and returns nil immediately.
func (lj *LockedJob) Run(ctx context.Context) {
	if lj.locker != nil {
		acquired, err := lj.locker.tryAdvisoryLock(ctx, lj.lockKey)
		if err != nil {
			lj.logger.Error("service: locked_job: advisory lock failed, proceeding without lock",
				"job", lj.name,
				"error", err,
			)
			// Fall through — run anyway rather than silently skip on transient errors.
		} else if !acquired {
			lj.logger.Info("service: locked_job: advisory lock held by another replica, skipping",
				"job", lj.name,
			)
			return
		} else {
			defer func() {
				if err := lj.locker.releaseAdvisoryLock(ctx, lj.lockKey); err != nil {
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
	}
}
