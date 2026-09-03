package postgres_test

// Pins for install-roles M1 and M2 (docs/audits/2026-09-03-independent-review/
// install-roles.md): installing a SECOND ledger database on a cluster that
// already carries the three roles, and the guard that decides whether the
// migration credential is safe to elevate.
//
// M1 -- 001_baseline acquires its SET membership on ledger_owner as a side
// effect of CREATING it. On a cluster where the roles already exist (Aaron's
// shared dev-postgres is exactly this; I-47's cluster lock exists because the
// shape is expected) `IF NOT EXISTS` skips the CREATE, and 001's closing
// `ALTER TABLE ... OWNER TO ledger_owner` dies with `must be able to SET ROLE
// "ledger_owner"` followed by an echo of the whole migration file, leaving
// `schema_migrations` at `(1, dirty=t)`. Migrate's preflight ran after
// applyBaseline and so had nothing to say. It now runs before.
//
// M2 -- the same-credential session guard excluded its own connections by
// `application_name`, a value any client sets for itself. One
// `SET application_name = 'azex-ledger-migrate'` on the application's pool
// turned a fail-closed refusal into a completed run. Ours are now identified
// by backend pid.

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// TestMigrate_SecondLedgerDatabaseOnACluster installs onto a database whose
// cluster already carries ledger_owner/app/ro, with the credential holding
// exactly what a first install leaves its installer: ADMIN OPTION on
// ledger_owner and nothing else (`admin=t, inherit=f, set=f`).
//
// TestMigrate_InstallsUnderNonSuperuserBootstrapCredential looks similar and
// is not this: it GRANTS `SET TRUE` by hand before installing, with a comment
// explaining that 001 needs it -- i.e. it pins the case where an operator
// already did what M1 is about nobody being told to do.
func TestMigrate_SecondLedgerDatabaseOnACluster(t *testing.T) {
	ctx := context.Background()

	// The first ledger database on this cluster, installed by the container's
	// superuser. This is what puts the three roles in the shared catalog --
	// asserted rather than assumed, because everything below is about what
	// happens when they are already there.
	_ = postgrestest.SetupDB(t)

	raw := postgrestest.SetupRawDB(t)
	admin, err := pgxpool.New(ctx, raw)
	require.NoError(t, err)
	defer admin.Close()

	for _, role := range []string{"ledger_owner", "ledger_app", "ledger_ro"} {
		var exists bool
		require.NoError(t, admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname=$1)", role).Scan(&exists))
		require.True(t, exists, "sanity: %s must already exist for this to be a SECOND install", role)
	}

	runner, runnerURL := newMigrationRunner(t, admin, raw, "ledger_second")
	// The residue a real first install leaves its own installer: admin option
	// and no way to switch roles. newMigrationRunner grants SET because most
	// tests need 001 to work; taking it back is what makes this the M1 case.
	stripLedgerOwnerSetOption(t, admin, runner)
	waitForNoSessionsAs(t, admin, runner)

	membersBefore := ledgerOwnerMembership(t, admin)

	require.NoError(t, postgres.Migrate(runnerURL),
		"a second ledger database on a cluster that already has the roles must install; before this, 001 died inside "+
			"its ownership sweep and left the database dirty")

	var version int
	var dirty bool
	require.NoError(t, admin.QueryRow(ctx, "SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty))
	assert.False(t, dirty, "the install must not leave the database dirty for a human to force")
	assert.Equal(t, latestMigrationVersion(t), version, "every migration must have run, not just the ones before 001's sweep")

	// I-57 holds here too: this credential does not get a quietly weaker schema.
	var unowned int
	require.NoError(t, admin.QueryRow(ctx, `
		SELECT count(*) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r','p','S','v','m')
		  AND pg_get_userbyid(c.relowner) <> 'ledger_owner'
	`).Scan(&unowned))
	assert.Zero(t, unowned)

	assert.Equal(t, membersBefore, ledgerOwnerMembership(t, admin),
		"the membership the install arranged for itself must be handed back -- including the one arranged before 001")
}

// TestMigrate_SecondInstallWithoutAdminOptionFailsBeforeTouchingAnything is
// the other half of M1: a credential that can neither switch to ledger_owner
// nor grant itself the membership cannot install, and must say so BEFORE
// applying anything.
//
// The failure being pinned is not "it errors" -- it errored before too. It is
// (a) the error names the statement that fixes it, and (b) the database is
// untouched, where previously golang-migrate had already written
// `(1, dirty=t)` and every later migration silently never ran.
func TestMigrate_SecondInstallWithoutAdminOptionFailsBeforeTouchingAnything(t *testing.T) {
	ctx := context.Background()

	_ = postgrestest.SetupDB(t) // puts the three roles on the cluster

	raw := postgrestest.SetupRawDB(t)
	admin, err := pgxpool.New(ctx, raw)
	require.NoError(t, err)
	defer admin.Close()

	var dbName string
	require.NoError(t, admin.QueryRow(ctx, "SELECT current_database()").Scan(&dbName))

	name := fmt.Sprintf("ledger_noadmin_%d", time.Now().UnixNano())
	const password = "no-admin-option-test-not-a-real-secret" //nolint:gosec // test-only credential
	_, err = admin.Exec(ctx, fmt.Sprintf("CREATE ROLE %s LOGIN CREATEROLE PASSWORD '%s'", pgxIdent(name), password))
	require.NoError(t, err)
	t.Cleanup(func() {
		c, err := pgxpool.New(context.Background(), raw)
		if err != nil {
			return
		}
		defer c.Close()
		cctx := context.Background()
		var self string
		if err := c.QueryRow(cctx, "SELECT current_user").Scan(&self); err != nil {
			return
		}
		_, _ = c.Exec(cctx, fmt.Sprintf("ALTER DATABASE %s OWNER TO %s", pgxIdent(dbName), pgxIdent(self)))
		_, _ = c.Exec(cctx, fmt.Sprintf("REASSIGN OWNED BY %s TO %s", pgxIdent(name), pgxIdent(self)))
		_, _ = c.Exec(cctx, fmt.Sprintf("DROP OWNED BY %s", pgxIdent(name)))
		_, _ = c.Exec(cctx, fmt.Sprintf("DROP ROLE IF EXISTS %s", pgxIdent(name)))
	})
	_, err = admin.Exec(ctx, fmt.Sprintf("ALTER DATABASE %s OWNER TO %s", pgxIdent(dbName), pgxIdent(name)))
	require.NoError(t, err)
	// Deliberately no GRANT on ledger_owner at all: this is the operator who
	// followed 001's header and had the first install's credential's rights
	// taken back, or a brand-new credential on a cluster somebody else
	// installed on.

	u, err := url.Parse(raw)
	require.NoError(t, err)
	u.User = url.UserPassword(name, password)

	err = postgres.Migrate(u.String())
	require.Error(t, err, "this credential cannot complete 001's ownership sweep and must not be allowed to start it")
	assert.Contains(t, err.Error(), "GRANT ledger_owner TO",
		"a refusal without the statement that fixes it is a deploy nobody can unblock")
	assert.Contains(t, err.Error(), "Nothing has been applied")
	assert.NotContains(t, err.Error(), "CREATE TABLE",
		"the old failure echoed the entire 1600-line migration file back at the operator")

	var installed bool
	require.NoError(t, admin.QueryRow(ctx, "SELECT to_regclass('public.schema_migrations') IS NOT NULL").Scan(&installed))
	assert.False(t, installed, "a refused install must not have created golang-migrate's bookkeeping table, let alone dirtied it")
}

// TestMigrate_RefusesASessionClaimingTheMigrationApplicationName is M2. The
// squatting session is a plain application connection that has said it is
// Migrate; the guard must count it anyway.
//
// Structured as a triple so a pass cannot be an accident: the same credential
// with an honestly-named foreign session is refused (the behaviour that
// always worked), with a squatting one is refused (the behaviour that did
// not), and with no foreign session at all migrates to completion (proving
// the refusals are the guard talking and not some unrelated breakage).
func TestMigrate_RefusesASessionClaimingTheMigrationApplicationName(t *testing.T) {
	ctx := context.Background()
	raw := postgrestest.SetupRawDB(t)

	admin, err := pgxpool.New(ctx, raw)
	require.NoError(t, err)
	defer admin.Close()

	runner, runnerURL := newMigrationRunner(t, admin, raw, "ledger_squat")
	applyBaselineAsRunner(t, admin, runner, runnerURL)

	before := ledgerOwnerMembership(t, admin)

	// The application pool, online on the migration credential, calling
	// itself what Migrate calls itself. One `SET` is the whole attack.
	app, err := pgx.Connect(ctx, runnerURL)
	require.NoError(t, err)
	defer func() { _ = app.Close(context.Background()) }()
	_, err = app.Exec(ctx, "SET application_name = 'azex-ledger-migrate'")
	require.NoError(t, err)

	var reported string
	require.NoError(t, app.QueryRow(ctx, "SELECT current_setting('application_name')").Scan(&reported))
	require.Equal(t, "azex-ledger-migrate", reported,
		"sanity: the squatting session must actually be reporting Migrate's own application_name")

	err = postgres.Migrate(runnerURL)
	require.Error(t, err,
		"a session holding the migration credential must be counted whatever it calls itself: the exclusion key cannot "+
			"be a value the audited session writes")
	assert.Contains(t, err.Error(), "pg_stat_activity")
	assert.Contains(t, err.Error(), "MIGRATE_DATABASE_URL")

	assert.Equal(t, before, ledgerOwnerMembership(t, admin),
		"a refused run must not have arranged anything first")
	var version int
	require.NoError(t, admin.QueryRow(ctx, "SELECT version FROM schema_migrations").Scan(&version))
	assert.Equal(t, 1, version, "and must not have applied any migration past the baseline")

	// Same session, honest name: refused too. Pinned so the fix cannot
	// degenerate into "refuse only sessions carrying THIS name".
	_, err = app.Exec(ctx, "SET application_name = 'myapp'")
	require.NoError(t, err)
	err = postgres.Migrate(runnerURL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "myapp", "the refusal lists what is in the way, by its own label")

	// And with the squatter gone the same run completes -- otherwise this
	// test would pass for a credential that simply cannot migrate at all.
	require.NoError(t, app.Close(ctx))
	waitForNoSessionsAs(t, admin, runner)
	require.NoError(t, postgres.Migrate(runnerURL))
	assert.Equal(t, before, ledgerOwnerMembership(t, admin))

	var dirty bool
	require.NoError(t, admin.QueryRow(ctx, "SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty))
	assert.False(t, dirty)
	assert.Equal(t, latestMigrationVersion(t), version)
}
