package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

func TestRegistrationRescanStore_DurableLeaseAndProgress(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	store := postgres.NewRegistrationRescanStore(pool)
	ctx := context.Background()

	jobs := []core.RegistrationRescan{{ChainID: 1, Address: "0xabc", NextBlock: 1234}}
	require.NoError(t, store.EnqueueRegistrationRescans(ctx, jobs))
	require.NoError(t, store.EnqueueRegistrationRescans(ctx, jobs), "enqueue must be idempotent")

	claimed, err := store.ClaimRegistrationRescans(ctx, 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, int64(1234), claimed[0].NextBlock)
	require.Equal(t, int32(1), claimed[0].Attempts)

	again, err := store.ClaimRegistrationRescans(ctx, 10, time.Minute)
	require.NoError(t, err)
	require.Empty(t, again, "an active lease must prevent a second replica from claiming")

	require.NoError(t, store.AdvanceRegistrationRescan(ctx, claimed[0].UID, 3234, false))
	claimed, err = store.ClaimRegistrationRescans(ctx, 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, int64(3234), claimed[0].NextBlock)

	require.NoError(t, store.AdvanceRegistrationRescan(ctx, claimed[0].UID, 4000, true))
	claimed, err = store.ClaimRegistrationRescans(ctx, 10, time.Minute)
	require.NoError(t, err)
	require.Empty(t, claimed)
}
