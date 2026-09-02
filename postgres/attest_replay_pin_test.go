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

// P6 (W3 adversarial review of the gates, 2026-09-03): this pin used to
// assert only the OUTCOME of a replay -- no new row, stored status still
// `signed`, Authorize reports `signed`. Every one of those held BEFORE the
// m-6 fix too, because postJournalWithQueries' locked recheck short-circuits
// first, so the reviewer could delete `replay: true` from attestJournal AND
// neuter the insert path's fail-closed refusal with the whole postgres
// package staying green. The pin was bound to a property of one caller,
// which is precisely what m-6 was about.
//
// It is now three tests, one per mechanism, so removing either half of the
// fix goes red on its own:
//
//   - the outcome (this test, unchanged in what it asserts);
//   - attestJournal FLAGS the replay and borrows the stored row's status
//     (TestAttestJournal_ReplayIsFlaggedAndReportsTheStoredStatus);
//   - the insert path REFUSES a replay-flagged auth the locked recheck did
//     not corroborate (TestPostJournalWithQueries_RefusesReplayFlaggedAuth).

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

// TestAttestJournal_ReplayIsFlaggedAndReportsTheStoredStatus pins mechanism
// one: attestJournal must mark an already-posted key as a replay, and must
// READ the status off the stored row rather than invent one. Deleting
// `replay: true` (the reviewer's P6) turns this red; going back to a
// hardcoded core.AuthStatusUnsignedNoAttestor -- the verdict VerifyLedger
// reads as suspected forgery -- turns it red too.
func TestAttestJournal_ReplayIsFlaggedAndReportsTheStoredStatus(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAuthFixture(t, pool, ctx)

	attestor, _ := newTestAttestor(t, "ed25519-replay-mechanism")
	store := postgres.NewLedgerStore(pool).WithAuth(attestor)

	input := f.journalInput(8311, postgrestest.UniqueKey("replay-mechanism"), decimal.NewFromInt(31))

	// A key with nothing posted under it is not a replay, and gets signed.
	replay, status, err := store.AttestJournalReplayVerdictForTest(ctx, input)
	require.NoError(t, err)
	assert.False(t, replay, "an unposted idempotency key is not a replay")
	assert.Equal(t, core.AuthStatusSigned, status)

	posted, err := store.PostJournal(ctx, input)
	require.NoError(t, err)
	require.Equal(t, string(core.AuthStatusSigned), fetchAuthStatus(t, pool, ctx, posted.UID))

	replay, status, err = store.AttestJournalReplayVerdictForTest(ctx, input)
	require.NoError(t, err)
	assert.True(t, replay,
		"attestJournal must FLAG an already-posted key as a replay. Without the flag the insert path has nothing to fail closed on, "+
			"and the only thing keeping an unsigned row out is the locked recheck in one caller -- the property m-6 exists to stop relying on")
	assert.Equal(t, core.AuthStatusSigned, status,
		"a replay must report the status the stored row actually carries, not a placeholder")
}

// TestPostJournalWithQueries_RefusesReplayFlaggedAuth pins mechanism two: the
// insert path must refuse a replay-flagged auth whose journal the locked
// recheck did not find. That state means "signing was skipped because the row
// already exists" and "the row does not exist" at the same time -- there is no
// correct row to write, so none may be written. Changing that guard to
// `if false` (the reviewer's P6b) turns this red.
func TestPostJournalWithQueries_RefusesReplayFlaggedAuth(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAuthFixture(t, pool, ctx)

	attestor, _ := newTestAttestor(t, "ed25519-replay-failclosed")
	store := postgres.NewLedgerStore(pool).WithAuth(attestor)

	// Nothing is posted under this key: the flag is a lie, which is exactly
	// the caller bug the guard is for.
	input := f.journalInput(8321, postgrestest.UniqueKey("replay-failclosed"), decimal.NewFromInt(17))
	before := countJournals(t, pool, ctx)

	err := store.PostJournalWithReplayFlaggedAuthForTest(ctx, input, core.AuthStatusSigned)
	require.Error(t, err,
		"a replay-flagged auth that reaches the insert path must be refused: it would write a row whose signature was deliberately never computed")
	assert.ErrorIs(t, err, core.ErrConflict)
	assert.Equal(t, before, countJournals(t, pool, ctx),
		"the refusal must write nothing -- an unsigned row under a key that is supposed to resolve to a signed one is what VerifyLedger reports as forgery")
}
