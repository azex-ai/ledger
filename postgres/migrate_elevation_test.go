package postgres_test

// Pins for the mechanism D-M2 introduced (2026-09-02 deep audit): Migrate
// takes ledger_owner's privileges for migrations 002..N and gives them back.
// migrate_bootstrap_role_test.go pins that the mechanism makes the install
// work. These two pin what happens when it does not work, which is where a
// temporary privilege turns into a permanent one:
//
//   - a migration that raises halfway through the elevated window must still
//     leave pg_auth_members with no `ledger_owner -> runner` membership, and
//   - a REVOKE that fails must be reported, not swallowed.
//
// The second was the code's original behaviour, on the argument that the
// credential holds ADMIN OPTION on ledger_owner permanently anyway so it could
// retake the membership at will. The gap that argument misses is the one
// working-agreements.md §3 is about: "revoked" and "silently still granted"
// had identical output, so a deployment could not tell which one it was in.
//
// Both drive the unexported functions through export_test.go rather than
// postgres.Migrate, because the failure has to happen INSIDE the elevated
// window and the migration set is embedded -- there is no seam for "make 014
// raise" that does not involve breaking a real migration file.

import (
	"context"
	"errors"
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

func TestWithLedgerOwner_ReleasesTheMembershipWhenTheMigrationFails(t *testing.T) {
	ctx := context.Background()
	raw := postgrestest.SetupRawDB(t)

	// A full install first: this test needs `ledger_owner` to exist, and the
	// only sanctioned way for it to exist is 001_baseline having created it.
	require.NoError(t, postgres.Migrate(raw))

	admin, err := pgxpool.New(ctx, raw)
	require.NoError(t, err)
	defer admin.Close()

	// Unique per run: role names are cluster-wide while databases are not, so
	// a fixed name collides with a leftover from an interrupted run and with a
	// concurrent copy of this test on the shared server.
	runner := fmt.Sprintf("ledger_runner_%d", time.Now().UnixNano())
	const runnerPassword = "elevation-test-not-a-real-secret" //nolint:gosec // test-only credential

	_, err = admin.Exec(ctx, fmt.Sprintf(
		"CREATE ROLE %s LOGIN CREATEROLE PASSWORD '%s'", pgxIdent(runner), runnerPassword))
	require.NoError(t, err)
	t.Cleanup(func() {
		c, err := pgxpool.New(context.Background(), raw)
		if err != nil {
			return
		}
		defer c.Close()
		cctx := context.Background()
		// Unconditionally, including on the path where the assertions below
		// passed: a leftover membership would make the NEXT run of this test
		// find the credential already elevated, take the alreadyHas branch,
		// and pass without exercising anything.
		_, _ = c.Exec(cctx, fmt.Sprintf("REVOKE ledger_owner FROM %s", pgxIdent(runner)))
		_, _ = c.Exec(cctx, fmt.Sprintf("DROP OWNED BY %s", pgxIdent(runner)))
		_, _ = c.Exec(cctx, fmt.Sprintf("DROP ROLE IF EXISTS %s", pgxIdent(runner)))
	})

	// `ADMIN TRUE, INHERIT FALSE, SET TRUE` is the shape 001_baseline
	// describes for the credential that creates these roles, and it is what
	// makes this test exercise anything: a credential that already inherits
	// ledger_owner takes the "nothing to do" branch and never elevates.
	_, err = admin.Exec(ctx, fmt.Sprintf(
		"GRANT ledger_owner TO %s WITH ADMIN TRUE, INHERIT FALSE, SET TRUE", pgxIdent(runner)))
	require.NoError(t, err)

	// Role membership is a cluster-wide catalog that every concurrent
	// Migrate() rewrites, and this is the lock Migrate holds for exactly that
	// reason -- taking it here makes this test mutually exclusive with them
	// instead of racing them. See export_test.go and I-47.
	unlock, err := postgres.AcquireClusterLockForTest(raw)
	require.NoError(t, err)
	defer unlock()

	require.False(t, inheritsLedgerOwner(t, admin, runner),
		"sanity: the credential must NOT already hold ledger_owner's privileges, or this test proves nothing")

	runnerURL, err := url.Parse(raw)
	require.NoError(t, err)
	runnerURL.User = url.UserPassword(runner, runnerPassword)

	// The injected failure. `migration 014 raised` stands in for any statement
	// inside the elevated window failing; what is being pinned is the exit
	// path, not the cause.
	injected := errors.New("injected: migration 014 raised")
	var elevatedInside bool
	err = postgres.WithLedgerOwnerForTest(runnerURL.String(), func() error {
		elevatedInside = inheritsLedgerOwner(t, admin, runner)
		return injected
	})

	require.ErrorIs(t, err, injected,
		"the migration's own error must survive -- releasing the membership must not replace the reason the install failed")
	require.True(t, elevatedInside,
		"sanity: the body must have run with ledger_owner's privileges, otherwise the release below is trivially satisfied")

	assert.False(t, inheritsLedgerOwner(t, admin, runner),
		"a failed migration must not leave the credential inheriting ledger_owner")

	// And the row itself, not just its effect: the elevation is one
	// pg_auth_members row with inherit_option, distinguishable from the
	// ADMIN/SET grant above, which is the operator's own and must survive.
	var elevationRows int
	require.NoError(t, admin.QueryRow(ctx, `
		SELECT count(*) FROM pg_auth_members m
		JOIN pg_roles r ON r.oid = m.roleid
		JOIN pg_roles g ON g.oid = m.member
		WHERE r.rolname = 'ledger_owner' AND g.rolname = $1 AND m.inherit_option
	`, runner).Scan(&elevationRows))
	assert.Zero(t, elevationRows, "no `GRANT ledger_owner TO %s WITH INHERIT TRUE` may be left in pg_auth_members", runner)
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
			"the error has to tell the operator what to run by hand -- being told only that something failed leaves the privilege standing either way")
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

// inheritsLedgerOwner reports whether role has ledger_owner's privileges by
// inheritance -- the same predicate elevateToLedgerOwner probes, and the one
// Postgres's ownership checks consult.
func inheritsLedgerOwner(t *testing.T, admin *pgxpool.Pool, role string) bool {
	t.Helper()
	var inherits bool
	require.NoError(t, admin.QueryRow(context.Background(),
		"SELECT pg_has_role($1, 'ledger_owner', 'USAGE')", role).Scan(&inherits))
	return inherits
}
