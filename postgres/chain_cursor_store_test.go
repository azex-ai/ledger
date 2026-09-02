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

func TestChainCursorStore_GetCursor_NotFoundWhenNeverScanned(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	store := postgres.NewChainCursorStore(pool)

	_, err := store.GetCursor(ctx, 999_001)
	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestChainCursorStore_SetCursor_UpsertAdvances(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	store := postgres.NewChainCursorStore(pool)

	const chainID = int64(999_002)

	require.NoError(t, store.SetCursor(ctx, chainID, 100))
	cur, err := store.GetCursor(ctx, chainID)
	require.NoError(t, err)
	assert.Equal(t, int64(100), cur.LastScannedBlock)

	require.NoError(t, store.SetCursor(ctx, chainID, 250))
	cur, err = store.GetCursor(ctx, chainID)
	require.NoError(t, err)
	assert.Equal(t, int64(250), cur.LastScannedBlock)
}

// TestChainCursorStore_SetCursor_IsMonotonic pins B-m7's storage half
// (docs/audits/2026-09-02-deep-audit/concurrency.md Minor): the query used to
// document monotonicity as "an orchestration-layer invariant (service/)" that
// service/ did not implement. Two replicas each write back the block THEY
// reached, so a lagging one could drag the cursor backwards -- and the
// fail-closed cursor semantics I-52 now relies on (a window is only marked
// scanned once every sighting in it is ingested or dead-lettered) are undone
// by a backwards write.
//
// Deleting the WHERE clause from SetChainCursor makes this test red.
func TestChainCursorStore_SetCursor_IsMonotonic(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	store := postgres.NewChainCursorStore(pool)

	const chainID = int64(999_003)

	require.NoError(t, store.SetCursor(ctx, chainID, 300))
	require.NoError(t, store.SetCursor(ctx, chainID, 200), "a backwards write is a no-op, not an error")

	cur, err := store.GetCursor(ctx, chainID)
	require.NoError(t, err)
	assert.Equal(t, int64(300), cur.LastScannedBlock,
		"the cursor must never move backwards, whoever writes it")

	// Forward progress still applies, and equal values are a no-op.
	require.NoError(t, store.SetCursor(ctx, chainID, 300))
	require.NoError(t, store.SetCursor(ctx, chainID, 301))
	cur, err = store.GetCursor(ctx, chainID)
	require.NoError(t, err)
	assert.Equal(t, int64(301), cur.LastScannedBlock)
}
