package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

// TestRollupQueue_ReDirtyPreventsLostEnqueue pins the DB-level closure of the
// enqueue-coalescing gap: a journal (EnqueueRollup) that arrives while a worker
// is mid-processing a dimension must re-dirty the claimed queue row, and the
// worker's MarkRollupProcessed claim guard must then refuse to mark it processed
// — so the row stays pending and the new entries are not lost.
func TestRollupQueue_ReDirtyPreventsLostEnqueue(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	adapter := postgres.NewRollupAdapter(pool)
	adapter.SetClaimLease(2 * time.Minute) // claim comfortably in the future

	const holder, currency, class = int64(100), int64(1), int64(10)

	// 1. A journal enqueues the dimension → one pending row.
	require.NoError(t, adapter.EnqueueRollup(ctx, holder, currency, class))

	// 2. Worker claims it (claimed_until set to now()+lease).
	items, err := adapter.DequeueRollupBatch(ctx, 10)
	require.NoError(t, err)
	require.Len(t, items, 1)
	id := items[0].ID

	// 3. A second journal lands WHILE the row is claimed (mid-processing). With
	//    DO NOTHING this would be silently coalesced away; with DO UPDATE it
	//    re-dirties the row (claimed_until → NULL).
	require.NoError(t, adapter.EnqueueRollup(ctx, holder, currency, class))

	// 4. Worker tries to finish: the claim guard must make this a no-op because
	//    the row was re-dirtied (claim token no longer matches).
	marked, err := adapter.MarkRollupProcessed(ctx, id, items[0].ClaimedUntil)
	require.NoError(t, err)
	assert.False(t, marked, "re-dirtied claim must not be markable")

	// 5. The row must still be pending — the enqueue was NOT lost.
	pending, err := adapter.CountPendingRollups(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), pending, "re-dirtied row must remain pending, not be lost")

	// 6. And it must be re-dequeuable so the next cycle materialises the entries.
	items2, err := adapter.DequeueRollupBatch(ctx, 10)
	require.NoError(t, err)
	require.Len(t, items2, 1, "re-dirtied row must be re-claimable")
	assert.Equal(t, id, items2[0].ID)
}

// TestRollupQueue_MarkProcessedSucceedsWithoutRedirty pins the other half: a
// clean processing pass (no enqueue arrives during it) must mark the row
// processed, so the queue does not churn forever.
func TestRollupQueue_MarkProcessedSucceedsWithoutRedirty(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	adapter := postgres.NewRollupAdapter(pool)
	adapter.SetClaimLease(2 * time.Minute)

	const holder, currency, class = int64(200), int64(1), int64(20)

	require.NoError(t, adapter.EnqueueRollup(ctx, holder, currency, class))

	items, err := adapter.DequeueRollupBatch(ctx, 10)
	require.NoError(t, err)
	require.Len(t, items, 1)

	marked, err := adapter.MarkRollupProcessed(ctx, items[0].ID, items[0].ClaimedUntil)
	require.NoError(t, err)
	assert.True(t, marked, "valid claim must mark the row processed")

	pending, err := adapter.CountPendingRollups(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), pending, "cleanly-processed row must be marked processed")
}

// TestRollupQueue_StaleWorkerCannotMarkOrReleaseReclaimedRow pins the claim-token
// handshake. After a re-dirty lets a second worker re-claim the row, the first
// (stale) worker must be able to neither mark it processed nor release/penalize
// it — only the current owner can. Before the claim token, MarkRollupProcessed /
// ReleaseRollupClaim only checked "some claim exists", letting a stale worker
// clobber the live owner's claim (and, via release, falsely bump failed_attempts).
func TestRollupQueue_StaleWorkerCannotMarkOrReleaseReclaimedRow(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	adapter := postgres.NewRollupAdapter(pool)

	const holder, currency, class = int64(400), int64(1), int64(40)

	require.NoError(t, adapter.EnqueueRollup(ctx, holder, currency, class))

	// Worker A claims with a 2-minute lease (tokenA).
	adapter.SetClaimLease(2 * time.Minute)
	itemsA, err := adapter.DequeueRollupBatch(ctx, 10)
	require.NoError(t, err)
	require.Len(t, itemsA, 1)
	tokenA := itemsA[0].ClaimedUntil

	// A journal re-dirties the claimed row (claimed_until → NULL).
	require.NoError(t, adapter.EnqueueRollup(ctx, holder, currency, class))

	// Worker B re-claims with a distinct 5-minute lease, so tokenB != tokenA.
	adapter.SetClaimLease(5 * time.Minute)
	itemsB, err := adapter.DequeueRollupBatch(ctx, 10)
	require.NoError(t, err)
	require.Len(t, itemsB, 1)
	require.Equal(t, itemsA[0].ID, itemsB[0].ID, "same row must be re-claimed")
	tokenB := itemsB[0].ClaimedUntil
	require.False(t, tokenA.Equal(tokenB), "claim tokens must differ between owners")

	// Stale worker A must NOT be able to mark B's row processed.
	marked, err := adapter.MarkRollupProcessed(ctx, itemsA[0].ID, tokenA)
	require.NoError(t, err)
	assert.False(t, marked, "stale worker must not mark a row it no longer owns")

	// Stale worker A's release must also no-op (must not clear B's claim).
	require.NoError(t, adapter.ReleaseRollupClaim(ctx, itemsA[0].ID, tokenA))

	// The current owner B can still finish its work.
	marked, err = adapter.MarkRollupProcessed(ctx, itemsB[0].ID, tokenB)
	require.NoError(t, err)
	assert.True(t, marked, "current owner must still be able to mark its claim processed")

	pending, err := adapter.CountPendingRollups(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), pending, "owner's mark must finalize the row")
}

// TestRollupQueue_CheckpointUpsertIsMonotonic pins the multi-worker safety guard:
// a writer carrying an OLDER snapshot (lower last_entry_id) must never regress a
// fresher checkpoint. This protects against two rollup workers processing the
// same dimension concurrently (possible once an enqueue re-dirties a claimed row).
func TestRollupQueue_CheckpointUpsertIsMonotonic(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	adapter := postgres.NewRollupAdapter(pool)

	const holder, currency, class = int64(300), int64(1), int64(30)

	// Fresh checkpoint at last_entry_id = 100.
	require.NoError(t, adapter.UpsertCheckpoint(ctx, service.BalanceCheckpoint{
		AccountHolder: holder, CurrencyID: currency, ClassificationID: class,
		Balance: decimal.NewFromInt(500), LastEntryID: 100,
	}))

	// Stale writer (older snapshot, last_entry_id = 50) must NOT regress it.
	require.NoError(t, adapter.UpsertCheckpoint(ctx, service.BalanceCheckpoint{
		AccountHolder: holder, CurrencyID: currency, ClassificationID: class,
		Balance: decimal.NewFromInt(250), LastEntryID: 50,
	}))

	cp, err := adapter.GetCheckpoint(ctx, holder, currency, class)
	require.NoError(t, err)
	require.NotNil(t, cp)
	assert.Equal(t, int64(100), cp.LastEntryID, "stale upsert must not regress last_entry_id")
	assert.True(t, cp.Balance.Equal(decimal.NewFromInt(500)), "stale upsert must not regress balance")

	// Fresher writer (last_entry_id = 150) advances it.
	require.NoError(t, adapter.UpsertCheckpoint(ctx, service.BalanceCheckpoint{
		AccountHolder: holder, CurrencyID: currency, ClassificationID: class,
		Balance: decimal.NewFromInt(750), LastEntryID: 150,
	}))
	cp, err = adapter.GetCheckpoint(ctx, holder, currency, class)
	require.NoError(t, err)
	require.NotNil(t, cp)
	assert.Equal(t, int64(150), cp.LastEntryID, "fresher upsert must advance the checkpoint")
	assert.True(t, cp.Balance.Equal(decimal.NewFromInt(750)), "fresher upsert must update balance")
}
