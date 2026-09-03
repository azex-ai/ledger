package postgres_test

// The two pins for M-5 (docs/audits/2026-09-02-deep-audit/w3-review/
// money-path.md): what a session holding the migration credential can see and
// do while migrations run.
//
// The mechanism these replaced narrowed the window in TIME (one membership per
// migration) and never in SUBJECT: `GRANT ledger_owner TO <runner> WITH
// INHERIT TRUE` writes pg_auth_members, a cluster-wide shared catalog, and
// Postgres's ownership checks consult has_privs_of_role() per statement
// without regard for which session is asking. Every other session holding the
// same credential -- in a single-credential deployment, the application's own
// pool -- was therefore owner-equivalent for the duration, and I-22's
// append-only triggers were droppable by the application while a deploy ran.
// Measured, not reasoned: on the previous mechanism the second test below
// failed on both of its assertions.
//
// Two layers replaced it, and there is one test each:
//
//   - Migrate refuses to start at all while another session holds the
//     migration credential (assertSoleSessionOnCredential). A credential that
//     can act as ledger_owner is not one to serve traffic on, and this is
//     where that stops being advice.
//   - What it arranges for itself, when it does run, is invisible to any
//     session that does not deliberately switch roles: SET-only membership,
//     used by exactly one connection.
//
// The second is deterministic rather than racing a fast migration run: an
// exclusive lock on a table migration 003 alters parks the run, and
// pg_stat_activity is the witness that the probe ran inside the window.
// Without that witness it would pass just as happily by arriving after the run
// finished, which is the shape working-agreements.md §3 is about.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

func TestMigrate_RefusesWhileAnotherSessionHoldsTheMigrationCredential(t *testing.T) {
	ctx := context.Background()
	raw := postgrestest.SetupRawDB(t)

	admin, err := pgxpool.New(ctx, raw)
	require.NoError(t, err)
	defer admin.Close()

	runner, runnerURL := newMigrationRunner(t, admin, raw, "ledger_guard")
	applyBaselineAsRunner(t, admin, runner, runnerURL)

	before := ledgerOwnerMembership(t, admin)

	// The application pool, online on the migration credential. One connection
	// is the whole scenario: the deployment shape every `examples/*/main.go`
	// warns about, and the one M-5 found the ledger silently tolerating.
	app, err := pgx.Connect(ctx, runnerURL)
	require.NoError(t, err)
	defer func() { _ = app.Close(context.Background()) }()

	err = postgres.Migrate(runnerURL)
	require.Error(t, err, "a credential that is simultaneously serving traffic must not be elevated to run migrations on")
	assert.Contains(t, err.Error(), "pg_stat_activity",
		"the operator has to be able to check the claim, which means being told where it came from")
	assert.Contains(t, err.Error(), "MIGRATE_DATABASE_URL",
		"and being told what to do about it: a refusal without a remedy is a deploy nobody can unblock")

	assert.Equal(t, before, ledgerOwnerMembership(t, admin),
		"a refused run must not have arranged anything first")

	var version int
	require.NoError(t, admin.QueryRow(ctx, "SELECT version FROM schema_migrations").Scan(&version))
	assert.Equal(t, 1, version, "and must not have applied any migration past the baseline")

	// The same run, once the application is gone, succeeds -- otherwise this
	// test would pass for a credential that simply cannot migrate at all.
	require.NoError(t, app.Close(ctx))
	waitForNoSessionsAs(t, admin, runner)
	require.NoError(t, postgres.Migrate(runnerURL))
	assert.Equal(t, before, ledgerOwnerMembership(t, admin))
}

func TestMigrate_WindowIsNotVisibleToOtherSessionsOfTheSameCredential(t *testing.T) {
	ctx := context.Background()
	raw := postgrestest.SetupRawDB(t)

	admin, err := pgxpool.New(ctx, raw)
	require.NoError(t, err)
	defer admin.Close()

	var dbName string
	require.NoError(t, admin.QueryRow(ctx, "SELECT current_database()").Scan(&dbName))

	runner, runnerURL := newMigrationRunner(t, admin, raw, "ledger_window")
	applyBaselineAsRunner(t, admin, runner, runnerURL)

	membersBefore := ledgerOwnerMembership(t, admin)

	// The parking brake: migration 003 puts a mutation guard on `currencies`,
	// and CREATE TRIGGER takes SHARE ROW EXCLUSIVE on its table -- which the
	// ACCESS EXCLUSIVE held here, from the superuser pool in an open
	// transaction, conflicts with. (An earlier draft locked webhook_nonces to
	// park 002 instead. It did not park anything: 002's only statement is a
	// GRANT, and GRANT updates pg_class's ACL through the syscache without
	// ever taking a lock on the relation it names. Measured, not reasoned.)
	brake, err := admin.Begin(ctx)
	require.NoError(t, err)
	_, err = brake.Exec(ctx, "LOCK TABLE public.currencies IN ACCESS EXCLUSIVE MODE")
	require.NoError(t, err)
	released := false
	defer func() {
		if !released {
			_ = brake.Rollback(ctx)
		}
	}()

	migrateDone := make(chan error, 1)
	go func() { migrateDone <- postgres.Migrate(runnerURL) }()

	// The witness: a backend belonging to the migration credential, waiting on
	// a lock. Until this is true the probe below would be measuring a window
	// that has not opened yet -- or one that has already closed.
	require.Eventually(t, func() bool {
		// A Migrate that ran to completion without ever blocking means the
		// brake did not hold, and every assertion below would be measuring
		// the state AFTER the window closed -- i.e. passing for the wrong
		// reason. Say which of the two it was.
		select {
		case err := <-migrateDone:
			t.Fatalf("Migrate finished without ever parking inside the window (err=%v); the parking brake did not hold", err)
		default:
		}
		var blocked int
		if err := admin.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE usename = $1 AND wait_event_type = 'Lock' AND datname = $2
		`, runner, dbName).Scan(&blocked); err != nil {
			return false
		}
		return blocked > 0
	}, 60*time.Second, 50*time.Millisecond,
		"sanity: migrations 002..N must actually be in flight and parked, or this probe proves nothing")

	// ---- the attack, on a second connection holding the same credential ----
	//
	// Opened here, mid-run, and therefore after the session guard has already
	// had its say: this is the residual that guard cannot cover, and the one
	// the SET-only membership exists to make worthless.
	attacker, err := pgx.Connect(ctx, runnerURL)
	require.NoError(t, err)
	defer func() { _ = attacker.Close(context.Background()) }()

	var inheritsDuring bool
	require.NoError(t, attacker.QueryRow(ctx,
		"SELECT pg_has_role(current_user, 'ledger_owner', 'USAGE')").Scan(&inheritsDuring))
	assert.False(t, inheritsDuring,
		"a second session on the migration credential must not inherit ledger_owner while migrations run (M-5)")

	// Rolled back either way: on the failing shape this statement SUCCEEDS,
	// and a pin must not be the thing that destroys the guard it is reporting.
	tx, err := attacker.Begin(ctx)
	require.NoError(t, err)
	_, dropErr := tx.Exec(ctx, "DROP TRIGGER journal_entries_no_update ON journal_entries")
	_ = tx.Rollback(ctx)

	require.Error(t, dropErr,
		"I-22's append-only trigger was droppable by a second session on the migration credential while Migrate ran")
	var pgErr *pgconn.PgError
	require.ErrorAs(t, dropErr, &pgErr)
	assert.Equal(t, "42501", pgErr.Code,
		"the refusal must be insufficient_privilege, not some incidental failure: %s", dropErr)

	// ---- release, and let the run finish ----
	require.NoError(t, brake.Commit(ctx))
	released = true

	select {
	case err := <-migrateDone:
		require.NoError(t, err)
	case <-time.After(120 * time.Second):
		t.Fatal("Migrate did not return after the lock was released")
	}

	var triggerStillThere bool
	require.NoError(t, admin.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'journal_entries_no_update' AND NOT tgisinternal)
	`).Scan(&triggerStillThere))
	assert.True(t, triggerStillThere, "the probe's DROP TRIGGER must have changed nothing")

	var version int
	var dirty bool
	require.NoError(t, admin.QueryRow(ctx, "SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty))
	assert.False(t, dirty)
	assert.Equal(t, latestMigrationVersion(t), version)

	// And the role graph is exactly where it started: whatever Migrate needed
	// in order to act as ledger_owner, it did not leave any of it behind.
	assert.Equal(t, membersBefore, ledgerOwnerMembership(t, admin),
		"Migrate must leave pg_auth_members exactly as it found it")
}

// applyBaselineAsRunner installs 001 on the migration credential and leaves it
// in the state a fresh install reaches 002 in, with no session of its own
// still connected.
//
// 001 has to be out of the way before either test above, for opposite reasons:
// the refusal test needs a run that gets as far as the session guard, and the
// parking test needs `002..N` to be the only thing left for the lock to park.
// Driving golang-migrate directly is how the test gets exactly `001` --
// postgres.Migrate always runs to the latest version.
func applyBaselineAsRunner(t *testing.T, admin *pgxpool.Pool, runner, runnerURL string) {
	t.Helper()

	src, err := postgres.NewMigrationSource()
	require.NoError(t, err)
	baseline, err := migrate.NewWithSourceInstance("iofs", src,
		strings.Replace(runnerURL, "postgres://", "pgx5://", 1))
	require.NoError(t, err)
	require.NoError(t, baseline.Migrate(1))
	_, _ = baseline.Close()

	// See newMigrationRunner: admin option only, which is what 001 leaves
	// behind and the branch that has something to arrange.
	stripLedgerOwnerSetOption(t, admin, runner)

	// And no leftovers of our own: `baseline` above connected as this
	// credential, and a backend that has been asked to close is still listed
	// until it exits. The session guard counts by application_name precisely
	// so that Migrate's own connections cannot trip it -- but golang-migrate
	// opened that one from a URL, so it carries none, and a test that did not
	// wait here would fail intermittently for a reason that is not a finding.
	waitForNoSessionsAs(t, admin, runner)
}

// waitForNoSessionsAs blocks until the given role has no backends left.
func waitForNoSessionsAs(t *testing.T, admin *pgxpool.Pool, role string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var n int
		if err := admin.QueryRow(context.Background(),
			"SELECT count(*) FROM pg_stat_activity WHERE usename = $1", role).Scan(&n); err != nil {
			return false
		}
		return n == 0
	}, 15*time.Second, 25*time.Millisecond, "a leftover session on %s would make the guard fire for the wrong reason", role)
}

// ledgerOwnerMembership snapshots every membership row in ledger_owner --
// member, grantor and all three options -- so a test can assert a Migrate run
// left the cluster-wide role graph untouched rather than merely "not
// inheriting", which a residual SET or ADMIN grant would also satisfy.
func ledgerOwnerMembership(t *testing.T, admin *pgxpool.Pool) []string {
	t.Helper()
	rows, err := admin.Query(context.Background(), `
		SELECT format('%s <- %s admin=%s inherit=%s set=%s by %s',
		              r.rolname, m.rolname, am.admin_option, am.inherit_option, am.set_option, g.rolname)
		FROM pg_auth_members am
		JOIN pg_roles r ON r.oid = am.roleid
		JOIN pg_roles m ON m.oid = am.member
		JOIN pg_roles g ON g.oid = am.grantor
		WHERE r.rolname = 'ledger_owner'
		ORDER BY 1
	`)
	require.NoError(t, err)
	defer rows.Close()

	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	return out
}
