package service_test

// Pin tests for concurrency.md's Major: the expiration job used to run
// unconditionally on every replica -- a bare runLoop, unlike its five
// siblings (reconcile, system_rollup, full_reconcile, partition,
// attestation), which all go through service.NewLockedJob. These drive the
// real Worker.Run wiring (not just the already-covered NewLockedJob
// mechanism itself; see locked_job_test.go / locked_job_integration_test.go)
// against a real Postgres advisory lock, so falsifying the fix -- reverting
// Worker.Run's "expiration" branch back to a bare runLoop -- reproduces
// TestWorker_Expiration_SkipsWhenAnotherReplicaHoldsTheLock's failure here.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

// countingExpiredReservationFinder counts GetExpiredReservations calls,
// standing in for ExpirationService's real store dependency so the test can
// observe whether the expiration batch ran on a given tick without needing
// any seeded expired reservations.
type countingExpiredReservationFinder struct{ calls atomic.Int64 }

func (c *countingExpiredReservationFinder) GetExpiredReservations(_ context.Context, _ int) ([]core.Reservation, error) {
	c.calls.Add(1)
	return nil, nil
}

type noopReservationReleaser struct{}

func (noopReservationReleaser) Release(_ context.Context, _ core.ReleaseInput) error { return nil }

// buildTestWorker assembles a Worker whose non-expiration jobs are backed by
// a real (harmless -- the DB is empty and the intervals below never fire
// them during the test) postgres.RollupAdapter, mirroring
// ledger.Service.Worker's production wiring (ledger.go, not this task's file
// -- reconstructed here independently from exported constructors only) so
// the test exercises the real Worker.Run rather than a stripped-down stand-in.
func buildTestWorker(pool *pgxpool.Pool, finder *countingExpiredReservationFinder) *service.Worker {
	engine := core.NewEngine()
	adapter := postgres.NewRollupAdapter(pool)

	rollupSvc := service.NewRollupService(adapter, adapter, adapter, adapter, engine)
	expirationSvc := service.NewExpirationService(finder, noopReservationReleaser{}, nil, nil, nil, engine)
	reconcileSvc := service.NewReconciliationService(adapter, adapter, adapter, adapter, engine)
	snapshotSvc := service.NewSnapshotService(adapter, adapter, engine)
	systemRollupSvc := service.NewSystemRollupService(adapter, adapter, engine)

	config := service.WorkerConfig{
		RollupInterval:       time.Hour,
		ExpirationInterval:   10 * time.Millisecond,
		ExpirationBatchSize:  10,
		ReconcileInterval:    time.Hour,
		SnapshotInterval:     time.Hour,
		SystemRollupInterval: time.Hour,
	}

	worker := service.NewWorker(rollupSvc, expirationSvc, reconcileSvc, snapshotSvc, systemRollupSvc, config, engine)
	// AllowSilent: this test builds a Worker with core.NewEngine's default
	// (no-op) logger and asserts on behaviour rather than log lines, so the
	// silence is deliberate -- Worker.Run otherwise refuses to start under it.
	worker.AllowSilent()
	worker.SetPool(pool)
	return worker
}

// TestWorker_Expiration_SkipsWhenAnotherReplicaHoldsTheLock holds the EXACT
// advisory lock key Worker.Run's "expiration" branch now takes (via
// NewLockedJob, same derivation as its siblings) on a separate real Postgres
// session before starting the worker -- simulating another replica already
// running this tick's batch -- and asserts GetExpiredReservations is never
// called.
func TestWorker_Expiration_SkipsWhenAnotherReplicaHoldsTheLock(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	lockKey := service.AdvisoryLockKeyForTest("job:expiration")
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	var acquired bool
	require.NoError(t, conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", lockKey).Scan(&acquired))
	require.True(t, acquired, "test setup: must hold the lock before starting the worker")
	defer func() {
		var released bool
		_ = conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", lockKey).Scan(&released)
	}()

	finder := &countingExpiredReservationFinder{}
	worker := buildTestWorker(pool, finder)

	runCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	_ = worker.Run(runCtx)

	require.Equal(t, int64(0), finder.calls.Load(),
		"expiration must skip entirely when another replica already holds the job lock")
}

// TestWorker_Expiration_RunsWhenLockIsFree is the positive contrast: with no
// competing holder, the expiration job must still run at its configured
// interval -- proving the lock wrap does not silently disable the job
// (working-agreements §3: a "leader election" that never elects a leader is
// the same failure mode as no election at all).
func TestWorker_Expiration_RunsWhenLockIsFree(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	finder := &countingExpiredReservationFinder{}
	worker := buildTestWorker(pool, finder)

	runCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	_ = worker.Run(runCtx)

	require.Greater(t, finder.calls.Load(), int64(0), "expiration must still run when no other replica holds the lock")
}
