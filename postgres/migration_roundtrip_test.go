package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/postgrestest"
)

// TestMigrations_FullDownChainAndReapply drives the whole migration set up,
// all the way back down, and up again.
//
// The baseline transfers table ownership to ledger_owner and its down script
// drops cluster-wide roles. Later migrations also disable and restore guards
// while changing protected tables. Testing each migration in isolation cannot
// establish that the complete down chain composes.
//
// It runs on the same server as every other test. Roles are cluster-wide, so
// the migrations are served through postgrestest's role-rewriting source:
// this test's install creates, and its down chain drops, private roles
// ledger_test_<pid>_<n>_{owner,app,ro}, never the shared ledger_owner /
// ledger_app / ledger_ro. docs/TESTING.md describes the arrangement.
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

	isolated := postgrestest.SetupRoleIsolatedRawDB(t)
	connStr := isolated.ConnString
	migrateURL := strings.Replace(connStr, "postgres://", "pgx5://", 1)
	privateRoles := []string{isolated.Roles.Owner, isolated.Roles.App, isolated.Roles.RO}
	countPrivateRoles := func() int {
		var n int
		require.NoError(t, ordinary.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname = ANY($1)`, privateRoles).Scan(&n))
		return n
	}
	require.Zero(t, countPrivateRoles(), "private roles must not exist before the install")

	newMigrator := func() *migrate.Migrate {
		src, err := postgrestest.NewRoleRewritingMigrationSource(isolated.Prefix)
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
	require.Equal(t, 3, countPrivateRoles(), "the install must create this test's private roles")
	assertOwnedByPrivateRoles(t, connStr, isolated.Roles.Owner, ownerBefore)

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
	require.Zero(t, countPrivateRoles(), "the full down chain must drop the roles its install created")

	reup := newMigrator()
	require.NoError(t, reup.Up(), "re-applying after a full rollback must succeed -- a down chain that leaks state fails here, not during teardown")
	reVersion, dirty, err := reup.Version()
	require.NoError(t, err)
	require.False(t, dirty)
	require.Equal(t, upVersion, reVersion, "re-applied schema must land on the same version")
	srcErr, dbErr = reup.Close()
	require.NoError(t, srcErr)
	require.NoError(t, dbErr)
	require.Equal(t, 3, countPrivateRoles(), "re-applying must recreate the private roles")
	assertOwnedByPrivateRoles(t, connStr, isolated.Roles.Owner, ownerBefore)

	var ownerAfter, tableOwnerAfter uint32
	var canaryName string
	require.NoError(t, ordinary.QueryRow(ctx, `SELECT oid FROM pg_roles WHERE rolname = 'ledger_owner'`).Scan(&ownerAfter))
	require.NoError(t, ordinary.QueryRow(ctx, `SELECT relowner FROM pg_class WHERE oid = 'public.currencies'::regclass`).Scan(&tableOwnerAfter))
	require.NoError(t, ordinary.QueryRow(ctx, `SELECT name FROM currencies WHERE uid = $1::uuid`, canaryUID).Scan(&canaryName))
	require.Equal(t, ownerBefore, ownerAfter, "roundtrip must not drop or recreate the shared ledger_owner role")
	require.Equal(t, tableOwnerBefore, tableOwnerAfter, "ordinary table ownership must remain intact")
	require.Equal(t, "Unaffected ordinary database", canaryName)
}

// assertOwnedByPrivateRoles proves the role rewrite took effect: every
// relation in the roundtrip database's public schema is owned by the private
// owner role, none by the shared ledger_owner (sharedOwner), and no ACL in it
// names a shared ledger role.
func assertOwnedByPrivateRoles(t *testing.T, connStr, privateOwner string, sharedOwner uint32) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, connStr)
	require.NoError(t, err)
	defer pool.Close()
	var privateOwned, sharedOwned, sharedACL int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE pg_get_userbyid(c.relowner) = $1),
		       count(*) FILTER (WHERE c.relowner = $2),
		       count(*) FILTER (WHERE c.relacl::text ~ '\mledger_(owner|app|ro)\M')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relname <> 'schema_migrations'
	`, privateOwner, sharedOwner).Scan(&privateOwned, &sharedOwned, &sharedACL))
	require.Positive(t, privateOwned, "the rewritten install must own its objects through %s", privateOwner)
	require.Zero(t, sharedOwned, "no roundtrip object may be owned by the shared ledger_owner")
	require.Zero(t, sharedACL, "no roundtrip object may grant to a shared ledger role")
}
