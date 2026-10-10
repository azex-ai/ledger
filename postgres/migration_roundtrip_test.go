package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// TestMigrations_FullDownChainAndReapply drives the whole migration set up,
// all the way back down, and up again.
//
// The baseline transfers table ownership to ledger_owner and its down script
// drops cluster-wide roles. Later migrations also disable and restore guards
// while changing protected tables. Testing each migration in isolation cannot
// establish that the complete down chain composes. docs/TESTING.md describes
// why this destructive test needs a dedicated cluster, not just a database.
//
// Reapplying afterwards matters as much as the teardown: a down chain that
// leaves a stray role, sequence, or trigger behind will not fail here, it will
// fail the next time someone migrates up.
func TestMigrations_FullDownChainAndReapply(t *testing.T) {
	// Keep a normal fixture alive throughout the destructive roundtrip. Its
	// ledger_owner dependency makes a regression to SetupRawDB fail at DROP
	// ROLE deterministically, even when no other package happens to run.
	ordinary := postgrestest.SetupDB(t)
	canaryUID := postgrestest.SeedCurrency(t, ordinary, "CANARY", "Unaffected ordinary database")
	ctx := context.Background()
	var ownerBefore, tableOwnerBefore uint32
	require.NoError(t, ordinary.QueryRow(ctx, `SELECT oid FROM pg_roles WHERE rolname = 'ledger_owner'`).Scan(&ownerBefore))
	require.NoError(t, ordinary.QueryRow(ctx, `SELECT relowner FROM pg_class WHERE oid = 'public.currencies'::regclass`).Scan(&tableOwnerBefore))
	require.Equal(t, ownerBefore, tableOwnerBefore)

	connStr := postgrestest.SetupIsolatedRawDB(t)
	migrateURL := strings.Replace(connStr, "postgres://", "pgx5://", 1)

	newMigrator := func() *migrate.Migrate {
		src, err := postgres.NewMigrationSource()
		require.NoError(t, err)
		m, err := migrate.NewWithSourceInstance("iofs", src, migrateURL)
		require.NoError(t, err)
		return m
	}

	up := newMigrator()
	require.NoError(t, up.Up(), "initial migrate up")
	upVersion, dirty, err := up.Version()
	require.NoError(t, err)
	require.False(t, dirty, "schema must not be dirty after a clean up")
	srcErr, dbErr := up.Close()
	require.NoError(t, srcErr)
	require.NoError(t, dbErr)

	down := newMigrator()
	require.NoError(t, down.Down(), "full down chain must compose, not just pass review one file at a time")
	_, _, err = down.Version()
	require.ErrorIs(t, err, migrate.ErrNilVersion, "after a full Down the schema should have no version")
	srcErr, dbErr = down.Close()
	require.NoError(t, srcErr)
	require.NoError(t, dbErr)

	// Nothing of ours should survive the teardown.
	pool, err := pgxpool.New(ctx, connStr)
	require.NoError(t, err)
	var leftovers []string
	rows, err := pool.Query(ctx, `
		SELECT tablename FROM pg_tables
		WHERE schemaname = 'public' AND tablename <> 'schema_migrations'
		ORDER BY tablename
	`)
	require.NoError(t, err)
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		leftovers = append(leftovers, name)
	}
	rows.Close()
	require.NoError(t, rows.Err())
	pool.Close()
	require.Empty(t, leftovers, "tables left behind by the down chain")

	reup := newMigrator()
	require.NoError(t, reup.Up(), "re-applying after a full rollback must succeed -- a down chain that leaks state fails here, not during teardown")
	reVersion, dirty, err := reup.Version()
	require.NoError(t, err)
	require.False(t, dirty)
	require.Equal(t, upVersion, reVersion, "re-applied schema must land on the same version")
	srcErr, dbErr = reup.Close()
	require.NoError(t, srcErr)
	require.NoError(t, dbErr)

	var ownerAfter, tableOwnerAfter uint32
	var canaryName string
	require.NoError(t, ordinary.QueryRow(ctx, `SELECT oid FROM pg_roles WHERE rolname = 'ledger_owner'`).Scan(&ownerAfter))
	require.NoError(t, ordinary.QueryRow(ctx, `SELECT relowner FROM pg_class WHERE oid = 'public.currencies'::regclass`).Scan(&tableOwnerAfter))
	require.NoError(t, ordinary.QueryRow(ctx, `SELECT name FROM currencies WHERE uid = $1::uuid`, canaryUID).Scan(&canaryName))
	require.Equal(t, ownerBefore, ownerAfter, "roundtrip must not replace the ordinary cluster's role")
	require.Equal(t, tableOwnerBefore, tableOwnerAfter, "ordinary table ownership must remain intact")
	require.Equal(t, "Unaffected ordinary database", canaryName)
}
