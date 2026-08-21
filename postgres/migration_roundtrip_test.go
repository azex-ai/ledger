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
// deployment.md requires every migration to carry a down script and every
// release to be rollback-capable, and each migration in the 2026-08-21
// integrity wave was reviewed individually against that rule -- but nothing
// had ever executed the down chain end to end. Individual down scripts passing
// review is not evidence that they compose: 049 hands table ownership to
// ledger_owner and 042's down drops that role, 045 converts journals.event_id
// between a sentinel and a nullable FK, 048 and 051 add columns under an
// append-only guard that has to be disabled and re-enabled. Any of those can
// be correct alone and wrong in sequence.
//
// Reapplying afterwards matters as much as the teardown: a down chain that
// leaves a stray role, sequence, or trigger behind will not fail here, it will
// fail the next time someone migrates up.
func TestMigrations_FullDownChainAndReapply(t *testing.T) {
	connStr := postgrestest.SetupRawDB(t)
	migrateURL := strings.Replace(connStr, "postgres://", "pgx5://", 1)
	ctx := context.Background()

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
}
