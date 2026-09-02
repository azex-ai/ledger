package postgres_test

// Pin for D-M2 (2026-09-02 deep audit): the credential docs/RUNBOOK.md tells
// operators to install with can actually install.
//
// RUNBOOK's "Database roles" section says the connection running 001_baseline
// "must be able to CREATE ROLE (superuser, or a role with the CREATEROLE
// attribute)", and then that "every migration after 001 runs as ledger_owner
// and needs no elevated privilege beyond what it already holds". The first
// sentence describes the standard managed-Postgres shape -- RDS's master,
// Cloud SQL, Neon, Supabase all withhold superuser. The second described a
// mechanism that did not exist: postgres.Migrate takes one URL and never
// switched roles.
//
// What actually happened on that credential, reproduced on postgres:17.10
// before this was fixed:
//
//   - migration 002 died at `GRANT DELETE ON public.webhook_nonces TO
//     ledger_app` with SQLSTATE 42501. 001's last act transfers every object
//     it created to ledger_owner, and the bootstrap credential holds SET but
//     not INHERIT on that role, so it stops passing Postgres's ownership check
//     the moment the sweep runs.
//   - had it survived that, 007's `ALTER ROLE ... NOSUPERUSER` would have
//     died too: Postgres gates each role attribute on the altering role
//     holding that same attribute, and it checks whether the clause was
//     written, not whether it changes anything. Measured as a CREATEROLE
//     CREATEDB role: NOSUPERUSER, NOREPLICATION and NOBYPASSRLS all refused;
//     as CREATEROLE alone, NOCREATEDB too.
//
// Either way golang-migrate marked the database dirty and every later
// migration silently never ran -- including 007's ledger_ro secret revoke,
// 008's journal_entries.id narrowing and 014's webhook_subscribers write
// narrowing. The deployments missing those are exactly the ones that followed
// the runbook.
//
// The fix is in two places and this test covers both: 007 now issues an ALTER
// only for an attribute a role actually holds, and postgres.Migrate takes
// ledger_owner's privileges for the span between 001 and the rest.
//
// It covers a third thing it did not originally set out to. Migration 018
// opens that same membership window inside itself (001's "Keepsake 2 of 2"
// idiom) and revokes it at the bottom of the file -- which, because the runner
// is the only role that can issue either grant, revokes Migrate's window too.
// Under one run-wide window this test went red at 020's `CREATE TRIGGER ... ON
// public.account_policies` ("permission denied for table account_policies",
// database dirty at 20, 021 never applied). Migrate now takes the membership
// per migration, so that coupling cannot exist; the assertions below (applied
// to the latest version, not dirty) are what notices if it comes back.
//
// Nothing else in the suite can notice any of this: every other test installs
// as the container's superuser, which takes the no-op branch of the elevation.

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

func TestMigrate_InstallsUnderNonSuperuserBootstrapCredential(t *testing.T) {
	ctx := context.Background()
	raw := postgrestest.SetupRawDB(t)

	admin, err := pgxpool.New(ctx, raw)
	require.NoError(t, err)
	defer admin.Close()

	var dbName string
	require.NoError(t, admin.QueryRow(ctx, "SELECT current_database()").Scan(&dbName))

	// Unique per run, because role names are cluster-wide while databases are
	// not: a deterministic name collides with a leftover from an interrupted
	// run, and with a concurrent copy of this test on a shared server.
	bootstrap := fmt.Sprintf("ledger_bootstrap_%d", time.Now().UnixNano())
	const bootstrapPassword = "bootstrap-role-test-not-a-real-secret" //nolint:gosec // test-only credential

	_, err = admin.Exec(ctx, fmt.Sprintf(
		"CREATE ROLE %s LOGIN CREATEROLE PASSWORD '%s'", pgxIdent(bootstrap), bootstrapPassword))
	require.NoError(t, err)
	t.Cleanup(func() {
		c, err := pgxpool.New(context.Background(), raw)
		if err != nil {
			return
		}
		defer c.Close()
		cctx := context.Background()
		// A role that owns anything, anywhere in the cluster, or that holds a
		// privilege on anything, cannot be dropped -- and this one is handed
		// the database below and then runs every migration. postgrestest's own
		// DROP DATABASE cleanup was registered first and so runs last, which is
		// why the database cannot be relied on to take these dependencies with
		// it. Leaving the role behind would fail the NEXT run of this test with
		// an error about a name, which is a long way from the cause.
		var self string
		if err := c.QueryRow(cctx, "SELECT current_user").Scan(&self); err != nil {
			return
		}
		_, _ = c.Exec(cctx, fmt.Sprintf("ALTER DATABASE %s OWNER TO %s", pgxIdent(dbName), pgxIdent(self)))
		_, _ = c.Exec(cctx, fmt.Sprintf("REASSIGN OWNED BY %s TO %s", pgxIdent(bootstrap), pgxIdent(self)))
		_, _ = c.Exec(cctx, fmt.Sprintf("DROP OWNED BY %s", pgxIdent(bootstrap)))
		_, _ = c.Exec(cctx, fmt.Sprintf("DROP ROLE IF EXISTS %s", pgxIdent(bootstrap)))
	})

	// In PostgreSQL 15+ schema `public` is owned by pg_database_owner, so
	// handing the database over is what gives the bootstrap credential CREATE
	// on it -- the ordinary shape on managed Postgres, where the account you
	// are given owns the database it provisioned for you.
	_, err = admin.Exec(ctx, fmt.Sprintf("ALTER DATABASE %s OWNER TO %s", pgxIdent(dbName), pgxIdent(bootstrap)))
	require.NoError(t, err)

	// Roles are cluster-wide and postgrestest shares one server across a test
	// binary, so ledger_owner/app/ro very likely already exist here, created
	// by another test's SetupDB as the superuser. A bootstrap credential
	// installing onto a cluster that already carries these names is the case
	// migration 007 section 1 was written for; it needs ADMIN OPTION on them,
	// which the credential that created them always has.
	//
	// `INHERIT FALSE, SET TRUE` matters and is not decoration. It is the shape
	// 001_baseline describes for the credential that creates these roles ("the
	// runner holds SET but deliberately not INHERIT on ledger_owner"), and it
	// is what makes this test exercise anything: a plain `GRANT ... WITH ADMIN
	// OPTION` takes its INHERIT default from the member's rolinherit, which is
	// true, so the bootstrap credential would arrive already inheriting
	// ledger_owner, Migrate's elevation would find nothing to do, and this test
	// would pass without the mechanism it exists to pin ever running.
	for _, role := range []string{"ledger_owner", "ledger_app", "ledger_ro"} {
		var exists bool
		require.NoError(t, admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", role).Scan(&exists))
		if exists {
			_, err = admin.Exec(ctx, fmt.Sprintf("GRANT %s TO %s WITH ADMIN TRUE, INHERIT FALSE, SET TRUE", role, pgxIdent(bootstrap)))
			require.NoError(t, err)
		}
	}

	// Scoped through pg_roles rather than naming ledger_owner as a literal:
	// pg_has_role raises 42704 on a role that does not exist, and on a fresh
	// server this test is the one that has to work when nothing has created
	// these roles yet -- `go test -run TestMigrate_Installs...` on a cold
	// container did exactly that, failing here for the one reason that is not
	// a finding about Migrate. Absent means "does not inherit", which is the
	// question being asked.
	var inheritsBefore bool
	require.NoError(t, admin.QueryRow(ctx, `
		SELECT COALESCE((SELECT pg_has_role($1, oid, 'USAGE') FROM pg_roles WHERE rolname = 'ledger_owner'), false)
	`, bootstrap).Scan(&inheritsBefore))
	require.False(t, inheritsBefore,
		"sanity: the credential must NOT already hold ledger_owner's privileges, or this test proves nothing about how Migrate gets them")

	u, err := url.Parse(raw)
	require.NoError(t, err)
	u.User = url.UserPassword(bootstrap, bootstrapPassword)

	require.NoError(t, postgres.Migrate(u.String()),
		"a CREATEROLE, non-superuser credential is what docs/RUNBOOK.md sanctions; if this fails, that document is describing an install nobody can perform")

	// Applied to the end and not dirty. "Dirty" is the specific failure this
	// pin is about: golang-migrate records the version it was attempting when a
	// statement raised, and every migration after it silently never runs.
	var version int
	var dirty bool
	require.NoError(t, admin.QueryRow(ctx, "SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty))
	assert.False(t, dirty, "the migration chain must not leave the database dirty on this credential")

	latest := latestMigrationVersion(t)
	assert.Equal(t, latest, version, "every migration must have run, not just the ones before the first privileged statement")

	// And the end state is the same one a superuser install produces -- the
	// point being that this credential does not get a quietly weaker schema.
	var unowned int
	require.NoError(t, admin.QueryRow(ctx, `
		SELECT count(*) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r','p','S','v','m')
		  AND pg_get_userbyid(c.relowner) <> 'ledger_owner'
	`).Scan(&unowned))
	assert.Zero(t, unowned, "I-57 must hold regardless of which credential ran the migrations")

	// The elevation is released. Leaving the bootstrap credential inheriting
	// ledger_owner would turn a bounded install-time window into a standing
	// one, which is the opposite of what 001 asks operators to do with this
	// credential (rotate or retire it).
	var stillInherits bool
	require.NoError(t, admin.QueryRow(ctx, "SELECT pg_has_role($1, 'ledger_owner', 'USAGE')", bootstrap).Scan(&stillInherits))
	assert.False(t, stillInherits, "Migrate must hand ledger_owner's privileges back when it returns")
}

// pgxIdent quotes a SQL identifier for interpolation into a statement that
// cannot take a bind parameter (CREATE ROLE, ALTER DATABASE, GRANT).
func pgxIdent(name string) string {
	return `"` + name + `"`
}

// latestMigrationVersion reads the highest migration number off the filesystem
// rather than hardcoding one, so adding a migration does not require editing
// this test -- and so "every migration ran" keeps meaning every migration,
// not every migration that existed when this was written.
func latestMigrationVersion(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("sql/migrations")
	require.NoError(t, err)

	latest := 0
	for _, e := range entries {
		m := migrationFileName.FindStringSubmatch(e.Name())
		if m == nil || m[3] != "up" {
			continue
		}
		n, err := strconv.Atoi(m[1])
		require.NoError(t, err)
		if n > latest {
			latest = n
		}
	}
	require.NotZero(t, latest, "sanity: sql/migrations must contain at least one up migration")
	return latest
}
