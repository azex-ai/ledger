package postgres_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// TestWebhookNonce_WorksAsLedgerApp pins the combination nothing exercised
// before: the replay cache driven by the role the application actually
// connects as.
//
// The nonce prune is a DELETE, and 001_baseline's grant loop gives ledger_app
// no DELETE on anything. TryRecordNonce ran the prune first and returned its
// error, and the webhook handler turns that into an HTTP failure — so every
// inbound webhook failed on a permission error in exactly the deployments that
// use the role separation the baseline installs. The security feature and the
// ingestion feature were mutually exclusive.
//
// It went unnoticed because the two halves were never run together: every
// other test connects as the container superuser, for which the DELETE
// succeeds and the whole path looks healthy.
//
// Migration 002 grants the one DELETE the baseline's own comment already
// called "the one sanctioned DELETE in this schema".
func TestWebhookNonce_WorksAsLedgerApp(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	const appPassword = "webhook-nonce-test-app-password-not-a-real-secret" //nolint:gosec // test-only credential
	_, err := pool.Exec(ctx, fmt.Sprintf("ALTER ROLE ledger_app WITH PASSWORD '%s'", appPassword))
	require.NoError(t, err)

	appPool, err := pgxpool.New(ctx, withRole(t, roleURLFromPool(pool), "ledger_app", appPassword))
	require.NoError(t, err)
	t.Cleanup(appPool.Close)
	require.NoError(t, appPool.Ping(ctx))

	store := postgres.NewWebhookSubscriberStore(appPool)

	fresh, err := store.TryRecordNonce(ctx, "nonce-as-ledger-app-1")
	require.NoError(t, err, "an inbound webhook must not fail because the cache prune needs a privilege")
	assert.True(t, fresh, "a nonce seen for the first time is fresh")

	// The replay check still has to work — tolerating the prune must not have
	// turned the cache off.
	replay, err := store.TryRecordNonce(ctx, "nonce-as-ledger-app-1")
	require.NoError(t, err)
	assert.False(t, replay, "the same nonce a second time is a replay and must be reported as stale")

	// And the prune must actually run, or the cache never shrinks. Plant an
	// expired row through the superuser pool, then drive one more call as
	// ledger_app and confirm it was collected.
	_, err = pool.Exec(ctx,
		`INSERT INTO webhook_nonces (nonce, seen_at) VALUES ('expired-nonce', now() - interval '1 hour')`)
	require.NoError(t, err)

	_, err = store.TryRecordNonce(ctx, "nonce-as-ledger-app-2")
	require.NoError(t, err)

	var remaining int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_nonces WHERE nonce = 'expired-nonce'`).Scan(&remaining))
	assert.Equal(t, 0, remaining,
		"the expired nonce must have been pruned by the ledger_app call -- if it survives, migration 002's grant is missing and the cache grows without bound")
}

// TestWebhookNonce_SurvivesRevokedPrunePrivilege pins the degradation, not the
// grant: with the DELETE revoked, recording a nonce still works and replay
// detection still works. Only the pruning stops.
//
// This is the half that keeps a database without migration 002 -- or one where
// an operator revoked the grant -- serving traffic instead of refusing it. It
// is deliberately not a substitute for the grant: the cost of landing here is
// a cache that only grows, which is why 002 exists.
func TestWebhookNonce_SurvivesRevokedPrunePrivilege(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	const appPassword = "webhook-nonce-revoked-test-password-not-a-real-secret" //nolint:gosec // test-only credential
	_, err := pool.Exec(ctx, fmt.Sprintf("ALTER ROLE ledger_app WITH PASSWORD '%s'", appPassword))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "REVOKE DELETE ON public.webhook_nonces FROM ledger_app")
	require.NoError(t, err)

	appPool, err := pgxpool.New(ctx, withRole(t, roleURLFromPool(pool), "ledger_app", appPassword))
	require.NoError(t, err)
	t.Cleanup(appPool.Close)

	store := postgres.NewWebhookSubscriberStore(appPool)

	fresh, err := store.TryRecordNonce(ctx, "nonce-without-delete-1")
	require.NoError(t, err, "a refused prune must not fail the request -- rejecting replays is this call's job, bounding the cache is not")
	assert.True(t, fresh)

	replay, err := store.TryRecordNonce(ctx, "nonce-without-delete-1")
	require.NoError(t, err)
	assert.False(t, replay, "replay detection must survive the degraded prune -- that is the part that must never be traded away")

	// The expired row stays, which is the accepted cost of degrading rather
	// than breaking. Asserting it keeps the trade honest and visible.
	_, err = pool.Exec(ctx,
		`INSERT INTO webhook_nonces (nonce, seen_at) VALUES ('stays-expired', now() - interval '1 hour')`)
	require.NoError(t, err)
	_, err = store.TryRecordNonce(ctx, "nonce-without-delete-2")
	require.NoError(t, err)

	var remaining int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_nonces WHERE nonce = 'stays-expired'`).Scan(&remaining))
	assert.Equal(t, 1, remaining, "without the privilege the cache stops shrinking -- this is the documented cost, not a second bug")
}
