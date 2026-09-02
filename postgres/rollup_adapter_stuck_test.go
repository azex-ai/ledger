package postgres_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// TestRollupAdapter_StuckRollups pins B-m10: a rollup_queue item that
// exhausted its retry budget (failed_attempts >= 10) is excluded from
// CountPendingRollups (so the "backlog is draining" gauge does not stay
// pinned above zero forever), counted separately by CountStuckRollups, and
// recoverable via ResetRollupClaim -- the only write path back into
// DequeueRollupBatch's eligible set for such a row.
func TestRollupAdapter_StuckRollups(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	adapter := postgres.NewRollupAdapter(pool)

	var stuckID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO rollup_queue (account_holder, currency_id, classification_id, failed_attempts)
		VALUES (1, 1, 1, 10)
		RETURNING id
	`).Scan(&stuckID))

	var healthyID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO rollup_queue (account_holder, currency_id, classification_id, failed_attempts)
		VALUES (2, 1, 1, 3)
		RETURNING id
	`).Scan(&healthyID))

	pending, err := adapter.CountPendingRollups(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), pending, "the stuck item must not count toward pending backlog")

	stuck, err := adapter.CountStuckRollups(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), stuck)

	// Resetting the healthy item's id must not touch the stuck row's count,
	// and resetting an unprocessed-but-not-stuck row is a legitimate no-op
	// success (there is nothing wrong with it, but the operator asked).
	require.NoError(t, adapter.ResetRollupClaim(ctx, healthyID))

	// Reset the actually-stuck row: it must move from "stuck" back to
	// "pending" atomically -- never double-counted, never lost.
	require.NoError(t, adapter.ResetRollupClaim(ctx, stuckID))

	pending, err = adapter.CountPendingRollups(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), pending, "both items are pending again after reset")

	stuck, err = adapter.CountStuckRollups(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), stuck)

	// A non-existent / already-processed row is reported, not silently
	// swallowed -- an operator resetting the wrong id must find out.
	err = adapter.ResetRollupClaim(ctx, 9_999_999)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrNotFound)
}
