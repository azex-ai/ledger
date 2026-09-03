package postgres_test

// Pins for the mechanism that lets migrations 002..N act as ledger_owner.
// migrate_bootstrap_role_test.go pins that it makes the install work, and
// migrate_window_subject_test.go pins that it is scoped to the connection
// running the migrations. These pin what it leaves behind, which is where a
// temporary authority turns into a permanent one:
//
//   - a run that fails partway must still leave pg_auth_members exactly as it
//     found it, and
//   - a REVOKE that fails must be reported, not swallowed.
//
// The second was the code's original behaviour, on the argument that the
// credential holds ADMIN OPTION on ledger_owner permanently anyway so it could
// retake the membership at will. The gap that argument misses is the one
// working-agreements.md §3 is about: "revoked" and "silently still granted"
// had identical output, so a deployment could not tell which one it was in.
//
// These replace two pins written for the previous mechanism, which granted the
// runner `ledger_owner WITH INHERIT TRUE` for the span of each migration.
// Their subject -- "the window is released on every exit path" -- survives;
// what the window IS has changed, so the assertions moved from "does the
// runner still inherit?" to "is the cluster-wide role graph byte-for-byte
// where it started?", which also catches a residual SET grant that the old
// question would have called clean (M-5, 2026-09-02 W3 review).

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

func TestMigrate_LeavesTheRoleGraphUntouchedWhenTheRunFails(t *testing.T) {
	ctx := context.Background()
	raw := postgrestest.SetupRawDB(t)

	admin, err := pgxpool.New(ctx, raw)
	require.NoError(t, err)
	defer admin.Close()

	runner, runnerURL := newMigrationRunner(t, admin, raw, "ledger_failrun")

	require.NoError(t, postgres.Migrate(runnerURL), "sanity: the install this test then breaks must first work")

	// From here the credential is in the state a fresh install reaches 002 in,
	// so the failing run below goes through the branch that has to grant
	// itself a membership -- the one with something to leave behind.
	stripLedgerOwnerSetOption(t, admin, runner)

	before := ledgerOwnerMembership(t, admin)

	// The injected failure, chosen because it aborts inside the run rather
	// than before it: golang-migrate refuses to touch a database another run
	// left dirty. What is being pinned is the exit path, not the cause.
	_, err = admin.Exec(ctx, "UPDATE schema_migrations SET dirty = true")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "UPDATE schema_migrations SET dirty = false") })

	err = postgres.Migrate(runnerURL)
	require.Error(t, err, "sanity: this run has to fail, or the release below is trivially satisfied")

	assert.Equal(t, before, ledgerOwnerMembership(t, admin),
		"a failed run must not leave the credential holding anything: pg_auth_members has to be exactly where it started")

	var inherits bool
	require.NoError(t, admin.QueryRow(ctx,
		"SELECT pg_has_role($1, 'ledger_owner', 'USAGE')", runner).Scan(&inherits))
	assert.False(t, inherits, "and it must certainly not be left inheriting ledger_owner")
}

func TestPrepareLedgerOwnerIdentity_GrantsTheNarrowestMembershipThatWorks(t *testing.T) {
	ctx := context.Background()
	raw := postgrestest.SetupRawDB(t)
	require.NoError(t, postgres.Migrate(raw))

	admin, err := pgxpool.New(ctx, raw)
	require.NoError(t, err)
	defer admin.Close()

	runner, runnerURL := newMigrationRunner(t, admin, raw, "ledger_narrow")

	// Role membership is a cluster-wide catalog that every concurrent
	// Migrate() rewrites, and this is the lock Migrate holds for exactly that
	// reason -- taking it here makes this test mutually exclusive with them
	// instead of racing them. See export_test.go and I-47.
	unlock, err := postgres.AcquireClusterLockForTest(raw)
	require.NoError(t, err)
	defer unlock()

	stripLedgerOwnerSetOption(t, admin, runner)
	before := ledgerOwnerMembership(t, admin)

	setRole, granted, err := postgres.PrepareLedgerOwnerIdentityForTest(runnerURL)
	require.NoError(t, err)
	require.True(t, setRole, "a credential that only holds ADMIN OPTION has to switch roles to act as ledger_owner")
	require.Equal(t, runner, granted, "and it has to grant itself the membership that makes SET ROLE possible")

	// The narrowness IS the finding: `WITH INHERIT TRUE` is what made every
	// other session on this credential owner-equivalent for the duration.
	var inherit, set bool
	require.NoError(t, admin.QueryRow(ctx, `
		SELECT am.inherit_option, am.set_option
		FROM pg_auth_members am
		JOIN pg_roles r ON r.oid = am.roleid
		JOIN pg_roles m ON m.oid = am.member
		JOIN pg_roles g ON g.oid = am.grantor
		WHERE r.rolname = 'ledger_owner' AND m.rolname = $1 AND g.rolname = $1
	`, runner).Scan(&inherit, &set))
	assert.False(t, inherit, "the membership must not confer ledger_owner's privileges on a session that did not ask for them")
	assert.True(t, set, "it must be enough to SET ROLE with, which is the whole reason it exists")

	var inherits bool
	require.NoError(t, admin.QueryRow(ctx,
		"SELECT pg_has_role($1, 'ledger_owner', 'USAGE')", runner).Scan(&inherits))
	assert.False(t, inherits, "so an ordinary statement from any other session on this credential still fails the ownership check")

	require.NoError(t, postgres.RevokeLedgerOwnerForTest(runnerURL, granted))
	assert.Equal(t, before, ledgerOwnerMembership(t, admin), "and the round trip puts the role graph back")
}

func TestRevokeLedgerOwner_FailureIsReportedNotSwallowed(t *testing.T) {
	raw := postgrestest.SetupRawDB(t)
	require.NoError(t, postgres.Migrate(raw))

	t.Run("the REVOKE itself failing", func(t *testing.T) {
		// A role that does not exist: REVOKE raises 42704. Standing in for
		// every reason the statement can fail on a database that is reachable.
		absent := fmt.Sprintf("ledger_absent_runner_%d", time.Now().UnixNano())

		err := postgres.RevokeLedgerOwnerForTest(raw, absent)
		require.Error(t, err, "a REVOKE that did not happen must not report success")
		assert.Contains(t, err.Error(), "REVOKE ledger_owner FROM",
			"the error has to tell the operator what to run by hand -- being told only that something failed leaves the membership standing either way")
	})

	t.Run("the database being unreachable", func(t *testing.T) {
		// The half most likely to happen in production: the migration
		// finished, then the network went. Port 1 refuses immediately.
		unreachable, err := url.Parse(raw)
		require.NoError(t, err)
		unreachable.Host = "127.0.0.1:1"

		err = postgres.RevokeLedgerOwnerForTest(unreachable.String(), "some_runner")
		require.Error(t, err, "an unreachable database means the membership is still in place, which is not success")
		assert.Contains(t, err.Error(), "REVOKE ledger_owner FROM")
	})
}

// newMigrationRunner creates the credential docs/RUNBOOK.md sanctions: a
// non-superuser CREATEROLE role owning the database, with ADMIN OPTION on the
// three ledger roles.
//
// `SET TRUE` is needed for 001 itself and only for 001: since PostgreSQL 16,
// `ALTER TABLE ... OWNER TO ledger_owner` requires the caller to be able to
// SET ROLE to the new owner, and 001's ownership sweep is nothing else. On a
// cold cluster the credential gets that from CREATE ROLE's
// `createrole_self_grant='set'`; on this shared test server the roles already
// exist, so it has to be granted, which is the case migration 007 section 1
// was written for.
//
// A test that then wants the state a REAL first install is in when migration
// 002 starts must call stripLedgerOwnerSetOption: 001's closing `REVOKE
// ledger_owner FROM <runner>` deletes the self-granted row, so a fresh install
// arrives at 002 holding admin option and nothing else (measured on
// postgres:17.10). Skipping that strip leaves the credential already able to
// switch roles, which is a real deployment shape too -- but not the one every
// first install takes.
func newMigrationRunner(t *testing.T, admin *pgxpool.Pool, raw, prefix string) (name, runnerURL string) {
	t.Helper()
	ctx := context.Background()

	var dbName string
	require.NoError(t, admin.QueryRow(ctx, "SELECT current_database()").Scan(&dbName))

	// Unique per run: role names are cluster-wide while databases are not, so
	// a fixed name collides with a leftover from an interrupted run and with a
	// concurrent copy of this test on the shared server.
	name = fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	const password = "migration-runner-test-not-a-real-secret" //nolint:gosec // test-only credential

	_, err := admin.Exec(ctx, fmt.Sprintf("CREATE ROLE %s LOGIN CREATEROLE PASSWORD '%s'", pgxIdent(name), password))
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
		// Unconditionally, including on the path where the assertions passed:
		// a leftover membership would make the NEXT run find the credential
		// already able to switch roles and pass without exercising anything.
		_, _ = c.Exec(cctx, fmt.Sprintf("ALTER DATABASE %s OWNER TO %s", pgxIdent(dbName), pgxIdent(self)))
		_, _ = c.Exec(cctx, fmt.Sprintf("REVOKE ledger_owner FROM %s", pgxIdent(name)))
		_, _ = c.Exec(cctx, fmt.Sprintf("REASSIGN OWNED BY %s TO %s", pgxIdent(name), pgxIdent(self)))
		_, _ = c.Exec(cctx, fmt.Sprintf("DROP OWNED BY %s", pgxIdent(name)))
		_, _ = c.Exec(cctx, fmt.Sprintf("DROP ROLE IF EXISTS %s", pgxIdent(name)))
	})

	// In PostgreSQL 15+ schema `public` is owned by pg_database_owner, so
	// handing the database over is what gives the credential CREATE on it --
	// the ordinary shape on managed Postgres, where the account you are given
	// owns the database it provisioned for you.
	_, err = admin.Exec(ctx, fmt.Sprintf("ALTER DATABASE %s OWNER TO %s", pgxIdent(dbName), pgxIdent(name)))
	require.NoError(t, err)

	// Roles are cluster-wide and postgrestest shares one server across a test
	// binary, so ledger_owner/app/ro very likely already exist here, created
	// by another test's SetupDB as the superuser. A credential installing onto
	// a cluster that already carries these names is the case migration 007
	// section 1 was written for; it needs ADMIN OPTION on them, which the
	// credential that created them always has.
	for _, role := range []string{"ledger_owner", "ledger_app", "ledger_ro"} {
		var exists bool
		require.NoError(t, admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", role).Scan(&exists))
		if exists {
			_, err = admin.Exec(ctx, fmt.Sprintf("GRANT %s TO %s WITH ADMIN TRUE, INHERIT FALSE, SET TRUE", role, pgxIdent(name)))
			require.NoError(t, err)
		}
	}

	u, err := url.Parse(raw)
	require.NoError(t, err)
	u.User = url.UserPassword(name, password)
	return name, u.String()
}

// stripLedgerOwnerSetOption puts a test credential into the state 001 leaves a
// fresh install in: admin option on ledger_owner, and no way to switch to it
// without granting itself one first. See newMigrationRunner.
func stripLedgerOwnerSetOption(t *testing.T, admin *pgxpool.Pool, runner string) {
	t.Helper()
	_, err := admin.Exec(context.Background(),
		fmt.Sprintf("REVOKE SET OPTION FOR ledger_owner FROM %s", pgxIdent(runner)))
	require.NoError(t, err)

	var canSet bool
	require.NoError(t, admin.QueryRow(context.Background(),
		"SELECT pg_has_role($1, 'ledger_owner', 'SET')", runner).Scan(&canSet))
	require.False(t, canSet, "sanity: the credential must arrive at 002 unable to switch roles, as a fresh install does")
}
