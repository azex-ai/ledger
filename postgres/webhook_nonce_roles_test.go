package postgres_test

import (
	"context"
	"fmt"
	"sync"
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

// TestWebhookNonce_PruneDegradationLeavesATrace pins D-m4 (2026-09-02 deep
// audit): the tolerated branch is tolerated, not silent.
//
// TryRecordNonce swallows insufficient_privilege from its prune on purpose,
// and its doc comment has always been honest about the cost ("a cache that
// stops shrinking"). What it could not say is that the cost is paid
// invisibly: a database missing migration 002's grant behaves identically to
// one that has it in every observable respect -- same responses, same latency,
// no log line, no metric -- until the replay cache fills the disk. That is the
// shape working-agreements §3 calls out: a degraded mode has to leave a trace.
//
// The assertion is deliberately two-sided. Tolerating the error is existing,
// correct behaviour (before migration 002 the fused version took down every
// inbound webhook), so the pin has to require the call to still SUCCEED as
// well as require the warning -- otherwise the obvious "fix" is to start
// failing again, which is the bug this branch exists to prevent.
func TestWebhookNonce_PruneDegradationLeavesATrace(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	const appPassword = "webhook-nonce-prune-warn-not-a-real-secret" //nolint:gosec // test-only credential
	_, err := pool.Exec(ctx, fmt.Sprintf("ALTER ROLE ledger_app WITH PASSWORD '%s'", appPassword))
	require.NoError(t, err)

	// Reproduce a database that never got migration 002's grant. Cluster-wide
	// role, per-database grant -- so this is scoped to this test's database
	// and does not leak into a concurrently running package.
	_, err = pool.Exec(ctx, "REVOKE DELETE ON public.webhook_nonces FROM ledger_app")
	require.NoError(t, err)

	appPool, err := pgxpool.New(ctx, withRole(t, roleURLFromPool(pool), "ledger_app", appPassword))
	require.NoError(t, err)
	t.Cleanup(appPool.Close)
	require.NoError(t, appPool.Ping(ctx))

	captured := &capturingLogger{}
	store := postgres.NewWebhookSubscriberStore(appPool).SetLogger(captured)

	fresh, err := store.TryRecordNonce(ctx, "nonce-prune-degraded-1")
	require.NoError(t, err, "a webhook must not start failing because the cache cannot be pruned")
	assert.True(t, fresh)

	require.Len(t, captured.warnings, 1, "the degradation must produce exactly one warning")
	assert.Contains(t, captured.warnings[0], "replay cache",
		"the message has to say what stopped working, not just that something failed")
	assert.Contains(t, captured.warnings[0], "002",
		"and it has to name the fix -- an operator reading this has no other pointer to it")

	// Once, not once per request: the condition is a missing GRANT, so it is
	// either always true or never true, and a line per inbound webhook would
	// bury the signal in its own noise.
	for i := 0; i < 5; i++ {
		_, err := store.TryRecordNonce(ctx, fmt.Sprintf("nonce-prune-degraded-%d", i+2))
		require.NoError(t, err)
	}
	assert.Len(t, captured.warnings, 1, "a permanently degraded prune must not warn per request")

	// And a healthy store says nothing at all, or the warning means nothing.
	quiet := &capturingLogger{}
	healthy := postgres.NewWebhookSubscriberStore(pool).SetLogger(quiet)
	_, err = healthy.TryRecordNonce(ctx, "nonce-prune-healthy-1")
	require.NoError(t, err)
	assert.Empty(t, quiet.warnings, "a database with the grant must produce no warning")
}

// capturingLogger is a core.Logger that records what it was told, so a test
// can assert on an observability claim rather than on the absence of an error.
type capturingLogger struct {
	mu       sync.Mutex
	warnings []string
}

func (l *capturingLogger) Debug(string, ...any) {}
func (l *capturingLogger) Info(string, ...any)  {}
func (l *capturingLogger) Error(string, ...any) {}
func (l *capturingLogger) Warn(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warnings = append(l.warnings, fmt.Sprint(append([]any{msg, " "}, args...)...))
}
