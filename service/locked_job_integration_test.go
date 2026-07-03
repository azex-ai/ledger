package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/service"
)

// Pins the advisory-lock connection affinity fix: pg advisory locks are
// session-scoped, so the unlock must run on the same pooled connection that
// acquired the lock. The old implementation issued lock and unlock as two
// independent pool.QueryRow calls — the unlock usually landed on a different
// connection, returned false, and the lock stayed stuck on an idle pooled
// connection, making every later Run skip with "held by another replica" and
// silently stalling the job fleet-wide until the connection was recycled.
func TestLockedJob_RealPool_LockReleasedAcrossRuns(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	engine := core.NewEngine()

	ran := 0
	lj := service.NewLockedJob("affinity_regression", func(ctx context.Context) error {
		ran++
		return nil
	}, pool, engine.Logger())

	for i := 0; i < 5; i++ {
		lj.Run(ctx)
	}
	require.Equal(t, 5, ran, "every Run must execute fn — a skipped run means the previous release leaked the lock")

	var held int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT count(*) FROM pg_locks WHERE locktype = 'advisory'").Scan(&held))
	assert.Zero(t, held, "no advisory lock may survive after the runs complete")
}
