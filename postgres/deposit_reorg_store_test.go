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

// TestDepositReorgStore_RecordIsIdempotentPerBookingAndKind pins the storage
// contract the reorg queue depends on (G-M8): re-detecting the same anomaly
// on every recheck tick must refresh last_seen_at, not append a row -- an
// operator's queue that grows one entry per tick is not a queue.
func TestDepositReorgStore_RecordIsIdempotentPerBookingAndKind(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	store := postgres.NewDepositReorgStore(pool)

	const bookingUID = "0192f000-0000-7000-8000-00000000ab01"
	require.NoError(t, store.RecordReorg(ctx, core.ReorgKindDeepReorg, bookingUID, 1, "0xtx", "0192f000-0000-7000-8000-00000000cd01"))
	first, err := store.ListOpenReorgs(ctx, 10)
	require.NoError(t, err)
	require.Len(t, first, 1)

	require.NoError(t, store.RecordReorg(ctx, core.ReorgKindDeepReorg, bookingUID, 1, "0xtx", "0192f000-0000-7000-8000-00000000cd01"))
	second, err := store.ListOpenReorgs(ctx, 10)
	require.NoError(t, err)
	require.Len(t, second, 1, "re-detection refreshes the row, it does not add one")
	assert.Equal(t, first[0].UID, second[0].UID)
	assert.False(t, second[0].LastSeenAt.Before(first[0].LastSeenAt))
	assert.True(t, second[0].IsOpen())
	// "First noticed at" and "still observable" are two separate facts, and
	// the age of the anomaly is what tells an operator how overdue it is --
	// so a re-detection may refresh last_seen_at and must NOT touch
	// detected_at. A row whose detected_at keeps moving is always freshly
	// discovered and therefore never overdue.
	assert.True(t, second[0].DetectedAt.Equal(first[0].DetectedAt),
		"re-detection must not reset when the anomaly was first noticed (want %s, got %s)",
		first[0].DetectedAt, second[0].DetectedAt)

	// A different KIND on the same booking is a different fact and gets its
	// own row.
	require.NoError(t, store.RecordReorg(ctx, core.ReorgKindShallowReorgFailed, bookingUID, 1, "0xtx", ""))
	both, err := store.ListOpenReorgs(ctx, 10)
	require.NoError(t, err)
	assert.Len(t, both, 2)
}

// TestDepositReorgStore_ResolveClosesOutExactlyOnce pins that closing out is
// the only way off the queue and that a second close-out cannot silently
// overwrite the first operator's record.
func TestDepositReorgStore_ResolveClosesOutExactlyOnce(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	store := postgres.NewDepositReorgStore(pool)

	const bookingUID = "0192f000-0000-7000-8000-00000000ab02"
	require.NoError(t, store.RecordReorg(ctx, core.ReorgKindDeepReorg, bookingUID, 1, "0xtx2", ""))

	require.NoError(t, store.ResolveReorg(ctx, core.ReorgKindDeepReorg, bookingUID, "reversed per RUNBOOK §12"))
	open, err := store.ListOpenReorgs(ctx, 10)
	require.NoError(t, err)
	assert.Empty(t, open)

	err = store.ResolveReorg(ctx, core.ReorgKindDeepReorg, bookingUID, "again")
	assert.ErrorIs(t, err, core.ErrNotFound, "a second close-out must report that there was nothing open, not succeed silently")

	// A later re-detection must NOT reopen it behind the operator's back.
	require.NoError(t, store.RecordReorg(ctx, core.ReorgKindDeepReorg, bookingUID, 1, "0xtx2", ""))
	stillClosed, err := store.ListOpenReorgs(ctx, 10)
	require.NoError(t, err)
	assert.Empty(t, stillClosed)
}

// TestDepositReorgStore_RejectsUnknownKind keeps the Go-side kind set and the
// table's CHECK constraint from drifting: a typo must fail as invalid input,
// not as a constraint violation from three layers down.
func TestDepositReorgStore_RejectsUnknownKind(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	store := postgres.NewDepositReorgStore(pool)

	err := store.RecordReorg(context.Background(), "reorg_maybe", "0192f000-0000-7000-8000-00000000ab03", 1, "0xtx3", "")
	assert.ErrorIs(t, err, core.ErrInvalidInput)
}
