package postgres_test

// Pin tests for the RunInTx signing gap fix
// (docs/plans/2026-08-21-tamper-evident-ledger-design.md §7.5, board
// #12/#13): PostJournal's tx-mode branch never called the Attestor,
// regardless of whether one was configured -- silently, since "no
// Attestor configured" and "posted through a path with no safe point to
// sign" were byte-for-byte indistinguishable (auth_digest/signature/key_id
// all empty either way). Authorize + PostAuthorized close that gap for a
// specific posting; auth_status (migration 051) makes the "why" a
// queryable fact.

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

func fetchAuthStatus(t testing.TB, pool *pgxpool.Pool, ctx context.Context, journalUID string) string {
	t.Helper()
	var status string
	require.NoError(t, pool.QueryRow(ctx, "SELECT auth_status FROM journals WHERE uid=$1", journalUID).Scan(&status))
	return status
}

// TestAuthorize_RejectsOnTransactionBoundStore pins the guard that stops a
// caller from calling Authorize on a store already bound to a transaction
// (postgres.LedgerStore.WithDB, e.g. the clone RunInTx hands to its
// callback): calling out to an Attestor from inside an open transaction is
// exactly what financial.md forbids, and exactly what Authorize exists to
// let callers avoid by running before RunInTx opens.
func TestAuthorize_RejectsOnTransactionBoundStore(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAuthFixture(t, pool, ctx)

	attestor, _ := newTestAttestor(t, "ed25519-authz-guard")
	store := postgres.NewLedgerStore(pool).WithAuth(attestor)

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	txStore := store.WithDB(tx)
	input := f.journalInput(8101, postgrestest.UniqueKey("authz-guard"), decimal.NewFromInt(10))
	_, err = txStore.Authorize(ctx, input)
	require.Error(t, err, "Authorize must refuse to run on a transaction-bound store")
	require.ErrorIs(t, err, core.ErrInvalidInput)
}

// TestPostAuthorized_RejectsEmptyStatus pins that a hand-built
// core.AuthorizedJournal (zero value, Status == "") is rejected rather than
// silently treated as any of the three real auth_status states -- it must
// come from Authorize.
func TestPostAuthorized_RejectsEmptyStatus(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	setupAuthFixture(t, pool, ctx)

	store := postgres.NewLedgerStore(pool)
	_, err := store.PostAuthorized(ctx, core.AuthorizedJournal{})
	require.Error(t, err)
	require.ErrorIs(t, err, core.ErrInvalidInput)
}

// TestPostAuthorized_SignsFromTxMode is the direct pin for the fix itself:
// Authorize runs on the pool-mode (top-level) store -- outside any
// transaction -- then PostAuthorized posts the result through a
// caller-owned transaction (mirroring exactly what RunInTx hands its
// callback). Before this fix, the only way to post a journal inside a
// caller-owned transaction was PostJournal's tx-mode branch, which never
// signed at all (see the contrasting assertion in
// TestPostJournal_TxMode_NeverSignsEvenWithAttestor below).
func TestPostAuthorized_SignsFromTxMode(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAuthFixture(t, pool, ctx)

	attestor, verifier := newTestAttestor(t, "ed25519-postauth-1")
	store := postgres.NewLedgerStore(pool).WithAuth(attestor)

	input := f.journalInput(8102, postgrestest.UniqueKey("postauth-txmode"), decimal.NewFromInt(30))

	// Authorize outside any transaction (pool mode).
	authorized, err := store.Authorize(ctx, input)
	require.NoError(t, err)
	require.Equal(t, core.AuthStatusSigned, authorized.Status)
	require.NotEmpty(t, authorized.Signature)

	// Post through a transaction the CALLER owns -- exactly the RunInTx
	// shape (postgres.LedgerStore.WithDB is what (*ledger.Service).RunInTx
	// uses internally to bind its clone to the open pgx.Tx).
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	txStore := store.WithDB(tx)
	journal, err := txStore.PostAuthorized(ctx, authorized)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	status := fetchAuthStatus(t, pool, ctx, journal.UID)
	assert.Equal(t, string(core.AuthStatusSigned), status, "a journal posted via PostAuthorized inside a caller-owned transaction must be signed")

	digest, signature, keyID, effectiveAt := fetchAuthColumns(t, pool, ctx, journal.UID)
	require.NoError(t, core.VerifyJournalAuth(ctx, verifier, input, effectiveAt, digest, signature, keyID))
}

// TestPostJournal_TxMode_NeverSignsEvenWithAttestor pins the OTHER half of
// the contract: PostJournal's tx-mode branch is UNCHANGED by this fix --
// callers that keep using it directly (instead of Authorize +
// PostAuthorized) still get an unsigned journal, now explicitly labeled
// unsigned_tx_mode rather than being indistinguishable from "no Attestor
// configured". This is the exact shape of the bug §7.5 fixes: an Attestor
// IS configured here, yet nothing gets signed, because this call path has
// no safe point to do so.
func TestPostJournal_TxMode_NeverSignsEvenWithAttestor(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAuthFixture(t, pool, ctx)

	attestor, _ := newTestAttestor(t, "ed25519-txmode-gap")
	store := postgres.NewLedgerStore(pool).WithAuth(attestor)

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	txStore := store.WithDB(tx)

	input := f.journalInput(8103, postgrestest.UniqueKey("txmode-gap"), decimal.NewFromInt(15))
	journal, err := txStore.PostJournal(ctx, input)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	digest, signature, keyID, _ := fetchAuthColumns(t, pool, ctx, journal.UID)
	assert.Empty(t, digest, "PostJournal's tx-mode branch must not sign even with an Attestor configured -- callers must use Authorize+PostAuthorized")
	assert.Empty(t, signature)
	assert.Empty(t, keyID)

	status := fetchAuthStatus(t, pool, ctx, journal.UID)
	assert.Equal(t, string(core.AuthStatusUnsignedTxMode), status, "must be labeled unsigned_tx_mode, not confused with unsigned_no_attestor -- an Attestor IS configured here")
}

// TestPostJournal_PoolMode_AuthStatusMatchesAttestorConfiguration pins the
// two non-tx-mode auth_status outcomes: unsigned_no_attestor when no
// Attestor is configured, signed when one is.
func TestPostJournal_PoolMode_AuthStatusMatchesAttestorConfiguration(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAuthFixture(t, pool, ctx)

	unsignedStore := postgres.NewLedgerStore(pool)
	j1, err := unsignedStore.PostJournal(ctx, f.journalInput(8104, postgrestest.UniqueKey("poolmode-noattestor"), decimal.NewFromInt(5)))
	require.NoError(t, err)
	assert.Equal(t, string(core.AuthStatusUnsignedNoAttestor), fetchAuthStatus(t, pool, ctx, j1.UID))

	attestor, _ := newTestAttestor(t, "ed25519-poolmode-signed")
	signedStore := postgres.NewLedgerStore(pool).WithAuth(attestor)
	j2, err := signedStore.PostJournal(ctx, f.journalInput(8105, postgrestest.UniqueKey("poolmode-signed"), decimal.NewFromInt(5)))
	require.NoError(t, err)
	assert.Equal(t, string(core.AuthStatusSigned), fetchAuthStatus(t, pool, ctx, j2.UID))
}

// TestAuthStatus_NewColumnRejectsUnknownValue pins the CHECK constraint
// (migration 051): auth_status is not a free-form string, direct SQL
// cannot write anything outside the three known values -- a second,
// narrower backstop alongside the Go-level guard in postJournalWithQueries.
func TestAuthStatus_NewColumnRejectsUnknownValue(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAuthFixture(t, pool, ctx)

	_, err := pool.Exec(ctx, `
		INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit, metadata, actor_id, source, effective_at, uid, auth_status)
		VALUES ($1, $2, 10, 10, '{}'::jsonb, 0, 'test', now(), gen_random_uuid(), 'not_a_real_status')
	`, f.journalTypeID, postgrestest.UniqueKey("auth-status-check"))
	require.Error(t, err, "auth_status CHECK constraint must reject values outside signed/unsigned_no_attestor/unsigned_tx_mode")
}
