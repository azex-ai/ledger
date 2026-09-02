package postgres_test

// Pin for tamper-evident.md's m-6 (2026-09-02): attestJournal labelled an
// IDEMPOTENT REPLAY as core.AuthStatusUnsignedNoAttestor.
//
// That value is not a neutral placeholder. service/attest_verify.go reads it
// as "this journal was forged, or was posted before the signing key was
// wired" -- the single most alarming verdict VerifyLedger produces. The
// invariant that kept it harmless was a comment ("this value never reaches
// the DB, because the locked recheck short-circuits first"), i.e. a property
// of one caller, not of the value. Any future write path that does not do
// that recheck would have labelled a perfectly signed journal as suspected
// forgery.
//
// The fix makes the replay case explicit (journalAuth.replay) instead of
// borrowing a verdict that means something else, and fails closed if a
// replay-flagged auth ever reaches the insert path.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// TestPostJournal_IdempotentReplayNeverInsertsUnsignedRow asserts both halves:
// the replay itself is inert (no new row, the stored row is still signed), and
// -- the part that was red before the fix -- the authorization intent a replay
// produces is not labelled with the forged/pre-key verdict.
func TestPostJournal_IdempotentReplayNeverInsertsUnsignedRow(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAuthFixture(t, pool, ctx)

	attestor, _ := newTestAttestor(t, "ed25519-replay-label")
	store := postgres.NewLedgerStore(pool).WithAuth(attestor)

	input := f.journalInput(8301, postgrestest.UniqueKey("replay-label"), decimal.NewFromInt(25))

	first, err := store.PostJournal(ctx, input)
	require.NoError(t, err)
	require.Equal(t, string(core.AuthStatusSigned), fetchAuthStatus(t, pool, ctx, first.UID))

	countBefore := countJournals(t, pool, ctx)

	// Same key, same payload: the idempotent replay must return the original
	// journal and write nothing.
	replayed, err := store.PostJournal(ctx, input)
	require.NoError(t, err)
	assert.Equal(t, first.UID, replayed.UID)
	assert.Equal(t, countBefore, countJournals(t, pool, ctx), "an idempotent replay must not insert a row")
	assert.Equal(t, string(core.AuthStatusSigned), fetchAuthStatus(t, pool, ctx, first.UID),
		"the stored journal's auth_status must be untouched by a replay")

	// The label the replay path produces. Before the fix this came back as
	// unsigned_no_attestor -- the exact value VerifyLedger reports as
	// "forged, or posted before the key was wired" -- for a journal that is
	// demonstrably signed by the configured Attestor above.
	authorized, err := store.Authorize(ctx, input)
	require.NoError(t, err)
	assert.NotEqual(t, core.AuthStatusUnsignedNoAttestor, authorized.Status,
		"an idempotent replay of a SIGNED journal must never borrow the unsigned_no_attestor verdict; that value means 'forged or predates the key' to VerifyLedger")
	assert.Equal(t, core.AuthStatusSigned, authorized.Status,
		"the replay must report the status the already-stored journal actually carries")

	// And posting that intent still resolves to the original journal.
	viaAuthorized, err := store.PostAuthorized(ctx, authorized)
	require.NoError(t, err)
	assert.Equal(t, first.UID, viaAuthorized.UID)
	assert.Equal(t, countBefore, countJournals(t, pool, ctx))
}

func countJournals(t testing.TB, pool *pgxpool.Pool, ctx context.Context) int64 {
	t.Helper()
	var n int64
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM journals").Scan(&n))
	return n
}
