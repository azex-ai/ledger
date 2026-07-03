package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// ---------------------------------------------------------------------------
// Fake lockAcquirer implementations
// ---------------------------------------------------------------------------

// alwaysAcquireLock simulates acquiring the lock every time.
type alwaysAcquireLock struct {
	acquired int64
	released int64
	// lastReleaseCtxErr captures ctx.Err() AT CALL TIME. Storing the ctx
	// object itself would be misleading: cleanupContext's own `defer cancel()`
	// fires immediately after this call returns (still before the assertion
	// runs), which would retroactively cancel a stored reference even though
	// the call itself ran on a live, uncancelled context.
	lastReleaseCtxErr error
}

func (l *alwaysAcquireLock) tryAdvisoryLock(_ context.Context, _ int64) (func(context.Context) error, bool, error) {
	atomic.AddInt64(&l.acquired, 1)
	release := func(ctx context.Context) error {
		atomic.AddInt64(&l.released, 1)
		l.lastReleaseCtxErr = ctx.Err()
		return nil
	}
	return release, true, nil
}

// neverAcquireLock simulates the lock being held by another replica.
type neverAcquireLock struct {
	attempts int64
}

func (l *neverAcquireLock) tryAdvisoryLock(_ context.Context, _ int64) (func(context.Context) error, bool, error) {
	atomic.AddInt64(&l.attempts, 1)
	return nil, false, nil // lock is held
}

// errorLock returns an error from tryAdvisoryLock.
type errorLock struct{}

func (l *errorLock) tryAdvisoryLock(_ context.Context, _ int64) (func(context.Context) error, bool, error) {
	return nil, false, errors.New("pg: connection refused")
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestLockedJob_RunsWhenLockAcquired verifies that fn is called when
// pg_try_advisory_lock returns true.
func TestLockedJob_RunsWhenLockAcquired(t *testing.T) {
	var called atomic.Int64
	locker := &alwaysAcquireLock{}
	engine := core.NewEngine()

	lj := &LockedJob{
		name:    "test_job",
		lockKey: advisoryLockKey("job:test_job"),
		fn: func(_ context.Context) error {
			called.Add(1)
			return nil
		},
		locker: locker,
		logger: engine.Logger(),
	}

	lj.Run(context.Background())

	assert.Equal(t, int64(1), called.Load(), "fn should run once")
	assert.Equal(t, int64(1), atomic.LoadInt64(&locker.acquired))
	assert.Equal(t, int64(1), atomic.LoadInt64(&locker.released), "lock must be released after fn")
}

// TestLockedJob_SkipsWhenLockHeld verifies that fn is NOT called when
// pg_try_advisory_lock returns false (another replica holds the lock).
func TestLockedJob_SkipsWhenLockHeld(t *testing.T) {
	var called atomic.Int64
	locker := &neverAcquireLock{}
	engine := core.NewEngine()

	lj := &LockedJob{
		name:    "test_job",
		lockKey: advisoryLockKey("job:test_job"),
		fn: func(_ context.Context) error {
			called.Add(1)
			return nil
		},
		locker: locker,
		logger: engine.Logger(),
	}

	lj.Run(context.Background())

	assert.Equal(t, int64(0), called.Load(), "fn must not run when lock is held by another replica")
	assert.Equal(t, int64(1), atomic.LoadInt64(&locker.attempts))
}

// TestLockedJob_NilLocker_RunsUnconditionally verifies single-instance mode
// (no pool configured) — fn always runs.
func TestLockedJob_NilLocker_RunsUnconditionally(t *testing.T) {
	var called atomic.Int64
	engine := core.NewEngine()

	lj := &LockedJob{
		name:    "test_job",
		lockKey: advisoryLockKey("job:test_job"),
		fn: func(_ context.Context) error {
			called.Add(1)
			return nil
		},
		locker: nil,
		logger: engine.Logger(),
	}

	lj.Run(context.Background())
	lj.Run(context.Background())

	assert.Equal(t, int64(2), called.Load(), "fn should run on every tick when locker is nil")
}

// TestLockedJob_LockErrorFallsThrough verifies that when pg_try_advisory_lock
// returns an error, the job falls through and runs anyway (prefer execution
// over silent skipping on transient DB errors).
func TestLockedJob_LockErrorFallsThrough(t *testing.T) {
	var called atomic.Int64
	engine := core.NewEngine()

	lj := &LockedJob{
		name:    "test_job",
		lockKey: advisoryLockKey("job:test_job"),
		fn: func(_ context.Context) error {
			called.Add(1)
			return nil
		},
		locker: &errorLock{},
		logger: engine.Logger(),
	}

	lj.Run(context.Background())

	assert.Equal(t, int64(1), called.Load(), "fn should still run when lock acquisition errors")
}

// TestLockedJob_ReleasesLockAfterCtxCancelledDuringFn verifies that the
// advisory lock is still released — on a context that is NOT cancelled —
// even when fn cancels (or observes cancellation of) the ctx it was given.
// This simulates a worker shutdown racing a running job: the release must not
// be handed the already-cancelled ctx, or it fails immediately and the lock
// leaks until the DB session holding it disconnects.
func TestLockedJob_ReleasesLockAfterCtxCancelledDuringFn(t *testing.T) {
	locker := &alwaysAcquireLock{}
	engine := core.NewEngine()

	ctx, cancel := context.WithCancel(context.Background())
	lj := &LockedJob{
		name:    "test_job",
		lockKey: advisoryLockKey("job:test_job"),
		fn: func(_ context.Context) error {
			cancel() // simulate shutdown firing while the job is running
			return nil
		},
		locker: locker,
		logger: engine.Logger(),
	}

	lj.Run(ctx)

	assert.Equal(t, int64(1), atomic.LoadInt64(&locker.released), "lock must still be released")
	assert.NoError(t, locker.lastReleaseCtxErr, "release must run on a detached ctx, not the cancelled parent")
}

// TestLockedJob_DoubleConcurrentRun simulates two sequential "pod" calls in
// the same tick — only the one that wins the lock should run fn.  We use a
// CAS-based fake locker to deterministically grant the lock to exactly one
// caller.  The two jobs run sequentially (not truly concurrent) so that we
// can assert precisely on run counts without race conditions inside the test.
func TestLockedJob_DoubleConcurrentRun(t *testing.T) {
	var runCount atomic.Int64
	var lockHolder atomic.Bool

	tryFn := func(_ context.Context, _ int64) (func(context.Context) error, bool, error) {
		// CAS: first caller wins, second sees lock held.
		if !lockHolder.CompareAndSwap(false, true) {
			return nil, false, nil
		}
		release := func(_ context.Context) error {
			lockHolder.Store(false)
			return nil
		}
		return release, true, nil
	}

	engine := core.NewEngine()
	makeLJ := func() *LockedJob {
		return &LockedJob{
			name:    "race_job",
			lockKey: advisoryLockKey("job:race_job"),
			fn: func(_ context.Context) error {
				runCount.Add(1)
				return nil
			},
			locker: &mockLockAcquirer{tryFn: tryFn},
			logger: engine.Logger(),
		}
	}

	// Run sequentially to keep the test deterministic: lj1 acquires, lj2 is skipped.
	lj1 := makeLJ()
	lj2 := makeLJ()

	lj1.Run(context.Background())
	// At this point lj1 has released the lock, but we want to test the
	// "another replica holds it" case.  Re-acquire manually before lj2 runs.
	lockHolder.Store(true)
	lj2.Run(context.Background())

	assert.Equal(t, int64(1), runCount.Load(), "only lj1 should have run fn; lj2 sees lock held")
}

// mockLockAcquirer is a flexible lockAcquirer backed by function fields.
type mockLockAcquirer struct {
	tryFn func(ctx context.Context, key int64) (func(context.Context) error, bool, error)
}

func (m *mockLockAcquirer) tryAdvisoryLock(ctx context.Context, key int64) (func(context.Context) error, bool, error) {
	return m.tryFn(ctx, key)
}

// TestAdvisoryLockKey_Deterministic verifies the key derivation is stable.
func TestAdvisoryLockKey_Deterministic(t *testing.T) {
	k1 := advisoryLockKey("job:reconcile")
	k2 := advisoryLockKey("job:reconcile")
	require.Equal(t, k1, k2, "key must be deterministic for the same input")

	k3 := advisoryLockKey("job:system_rollup")
	assert.NotEqual(t, k1, k3, "different job names must produce different keys")
}
