package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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

	require.NoError(t, store.AdvanceRegistrationRescan(ctx, claimed[0].UID, 3234, false, claimed[0].Attempts))
	claimed, err = store.ClaimRegistrationRescans(ctx, 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, int64(3234), claimed[0].NextBlock)

	require.NoError(t, store.AdvanceRegistrationRescan(ctx, claimed[0].UID, 4000, true, claimed[0].Attempts))
	claimed, err = store.ClaimRegistrationRescans(ctx, 10, time.Minute)
	require.NoError(t, err)
	require.Empty(t, claimed)
}

// TestRegistrationRescanStore_AdvanceRejectsStaleClaim pins the claim-token
// guard (concurrency.md Major, board #30): a worker whose lease outlived its
// own processing must not be able to write progress after another worker
// re-claimed the same row. Constructs the real timing sequence, not just a
// unit assertion on the guard's SQL:
//  1. Worker A claims with a short lease and observes attempts=1.
//  2. A's lease genuinely expires (real sleep past the lease duration).
//  3. Worker B re-claims the same row -- ClaimRegistrationRescans bumps
//     attempts to 2, exactly like a real second replica's poll loop would.
//  4. A "finishes" late and tries to advance using the STALE attempts=1 it
//     captured at its own claim time -- this must be rejected.
//  5. B's live claim (claimed_until still in the future) and the row's real
//     next_block must be completely unaffected by A's rejected write --
//     this is the concrete failure concurrency.md describes: a stale
//     worker's write "抹掉了 B 正在持有的活 claim" if unguarded.
//
// Falsification: removing "AND attempts = $4" from AdvanceRegistrationRescan
// (postgres/sql/queries -- this store hand-writes SQL rather than using a
// sqlc query file, see registration_rescan_store.go's package doc) makes
// step 4's AdvanceRegistrationRescan succeed instead of erroring, which
// turns this test red (verified: see this task's bus checkpoint for the
// git-stash-revert run).
func TestRegistrationRescanStore_AdvanceRejectsStaleClaim(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	store := postgres.NewRegistrationRescanStore(pool)
	ctx := context.Background()

	jobs := []core.RegistrationRescan{{ChainID: 77, Address: "0xstaleclaim", NextBlock: 100}}
	require.NoError(t, store.EnqueueRegistrationRescans(ctx, jobs))

	// Worker A claims with a short lease.
	claimedA, err := store.ClaimRegistrationRescans(ctx, 10, 50*time.Millisecond)
	require.NoError(t, err)
	require.Len(t, claimedA, 1)
	staleAttempts := claimedA[0].Attempts
	require.Equal(t, int32(1), staleAttempts)

	// A hangs (simulating FetchDeposits stuck on a slow RPC) -- let the
	// lease genuinely expire in real time.
	time.Sleep(100 * time.Millisecond)

	// Worker B re-claims the same row (lease expired): bumps attempts.
	claimedB, err := store.ClaimRegistrationRescans(ctx, 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, claimedB, 1)
	require.Equal(t, claimedA[0].UID, claimedB[0].UID)
	require.Equal(t, int32(2), claimedB[0].Attempts, "re-claim after lease expiry must bump attempts")

	// A finally "finishes" and tries to write back using the STALE attempts
	// value it observed at its own claim time -- must be rejected.
	err = store.AdvanceRegistrationRescan(ctx, claimedA[0].UID, 999, false, staleAttempts)
	require.Error(t, err, "a worker whose claim was stolen by a re-claim must not be able to advance progress")
	require.ErrorIs(t, err, pgx.ErrNoRows)

	// B's active claim (1 minute lease, still far from expiry) must still
	// be held -- A's rejected write must not have cleared claimed_until.
	claimedAgain, err := store.ClaimRegistrationRescans(ctx, 10, time.Minute)
	require.NoError(t, err)
	require.Empty(t, claimedAgain, "B's active claim must still be held -- A's rejected write must not have cleared it")

	// The row's real progress must be untouched by A's rejected write.
	var nextBlock int64
	require.NoError(t, pool.QueryRow(ctx, "SELECT next_block FROM registration_rescans WHERE uid=$1", claimedA[0].UID).Scan(&nextBlock))
	require.Equal(t, int64(100), nextBlock, "A's stale write must not have changed next_block")
}

// TestRegistrationRescanStore_RetryRejectsStaleClaim is
// TestRegistrationRescanStore_AdvanceRejectsStaleClaim's counterpart for the
// failure path: a stale worker's FetchDeposits eventually errors (rather
// than eventually succeeding) after its claim was stolen by a re-claim.
// Falsification: removing "AND attempts = $4" from RetryRegistrationRescan
// makes A's retry succeed instead of erroring, clearing B's live claim
// (claimed_until = NULL) out from under it -- turns this test red the same
// way as the Advance case above.
func TestRegistrationRescanStore_RetryRejectsStaleClaim(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	store := postgres.NewRegistrationRescanStore(pool)
	ctx := context.Background()

	jobs := []core.RegistrationRescan{{ChainID: 78, Address: "0xstaleretry", NextBlock: 100}}
	require.NoError(t, store.EnqueueRegistrationRescans(ctx, jobs))

	claimedA, err := store.ClaimRegistrationRescans(ctx, 10, 50*time.Millisecond)
	require.NoError(t, err)
	require.Len(t, claimedA, 1)
	staleAttempts := claimedA[0].Attempts

	time.Sleep(100 * time.Millisecond)

	claimedB, err := store.ClaimRegistrationRescans(ctx, 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, claimedB, 1)
	require.Equal(t, int32(2), claimedB[0].Attempts)

	// A's own attempt finally errors out (e.g. FetchDeposits failed) and it
	// tries to record the retry using its stale attempts -- must be rejected.
	err = store.RetryRegistrationRescan(ctx, claimedA[0].UID, "rpc timeout", time.Now().Add(time.Second), staleAttempts)
	require.Error(t, err, "a worker whose claim was stolen by a re-claim must not be able to record a retry")
	require.ErrorIs(t, err, pgx.ErrNoRows)

	// B's active claim must still be held.
	claimedAgain, err := store.ClaimRegistrationRescans(ctx, 10, time.Minute)
	require.NoError(t, err)
	require.Empty(t, claimedAgain, "B's active claim must still be held -- A's rejected retry must not have cleared it")
}
