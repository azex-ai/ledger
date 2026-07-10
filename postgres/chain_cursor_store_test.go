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
