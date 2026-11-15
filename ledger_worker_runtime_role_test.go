package ledger_test

import (
	"context"
	"fmt"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/service"
)

// TestServiceWorker_RuntimeRoleWarning_RealDatabase is the root-package half
// of the pin missing from the original W5-readme delivery (team-lead
// review, 2026-09-04): service.TestWorker_SetRuntimeRoleWarning_AppearsInStartupReport
// covers the StartupReport wiring in isolation, with a hand-built error; this
// one drives the actual facade call (*ledger.Service).Worker makes against a
// real database, on both sides of the role boundary AssertRuntimeRole (and
// now Worker) checks -- the same F-C1 gap the consumer audit found (README's
// Quick Start made the runtime connection = the migration superuser
// connection, and nothing warned).
func TestServiceWorker_RuntimeRoleWarning_RealDatabase(t *testing.T) {
	pool := postgrestest.SetupDB(t)

	t.Run("superuser pool -> Worker's StartupReport carries the warning", func(t *testing.T) {
		svc, err := ledger.New(pool)
		require.NoError(t, err)

		worker, err := svc.Worker(service.DefaultWorkerConfig())
		require.NoError(t, err)

		report := worker.StartupReport()
		require.NotEmpty(t, report.RuntimeRoleWarning,
			"a Worker built on a connection that does not authenticate as ledger_app must say so -- "+
				"this is the one degraded mode that used to report nothing at all (F-C1)")
		assert.Contains(t, report.RuntimeRoleWarning, "ledger_app",
			"the warning must name the role the operator should be using")

		var found bool
		for _, w := range report.Warnings {
			if w == "worker: "+report.RuntimeRoleWarning {
				found = true
			}
		}
		assert.True(t, found, "the warning must reach Warnings the same way every other degraded mode does: %v", report.Warnings)
	})

	t.Run("ledger_app pool -> Worker's StartupReport carries no warning", func(t *testing.T) {
		appPool := newRootAppPool(t, pool) //nolint:gosec
		svc, err := ledger.New(appPool)
		require.NoError(t, err)

		worker, err := svc.Worker(service.DefaultWorkerConfig())
		require.NoError(t, err)

		report := worker.StartupReport()
		assert.Empty(t, report.RuntimeRoleWarning)
		for _, w := range report.Warnings {
			assert.NotContains(t, w, "role check",
				"no role-mismatch warning should appear on a correctly-scoped connection: %v", report.Warnings)
		}
	})
}

// newRootAppPool mirrors postgres_test's newAppPool (postgres/roles_test.go)
// -- that helper lives in a _test.go file of a different package and is
// invisible outside it, so this is the minimal reimplementation this
// package's single ACL-boundary test needs. No cluster-lock guard (compare
// postgres/roles_test.go's holdACLGuard): no test elevates the shared
// ledger_app's attributes any more (the hardening test uses private role
// names), and the password it sets is postgrestest.SharedRolePassword -- the
// one every test sets on the shared roles -- so concurrent processes on the
// same server cannot invalidate each other's credentials.
func newRootAppPool(t *testing.T, pool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	password := postgrestest.SharedRolePassword
	ctx := context.Background()

	err := postgrestest.SetSharedRolePassword(ctx, pool, "ledger_app")
	require.NoError(t, err)

	cfg := pool.Config().ConnConfig
	u, err := url.Parse(fmt.Sprintf("postgres://placeholder:placeholder@%s:%d/%s?sslmode=disable", cfg.Host, cfg.Port, cfg.Database))
	require.NoError(t, err)
	u.User = url.UserPassword("ledger_app", password)

	appPool, err := pgxpool.New(ctx, u.String())
	require.NoError(t, err)
	t.Cleanup(appPool.Close)
	require.NoError(t, appPool.Ping(ctx))
	return appPool
}
