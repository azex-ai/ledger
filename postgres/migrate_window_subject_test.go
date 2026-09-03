package postgres_test

// The attack probe for M-5 (docs/audits/2026-09-02-deep-audit/w3-review/
// money-path.md): Migrate's ledger_owner window must be scoped to the
// connection running the migrations, not to the credential running them.
//
// The mechanism it replaced narrowed the window in TIME (one membership per
// migration) and never in SUBJECT: `GRANT ledger_owner TO <runner> WITH
// INHERIT TRUE` writes pg_auth_members, a cluster-wide shared catalog, and
// Postgres's ownership checks consult has_privs_of_role() per statement
// without regard for which session is asking. Every other session holding the
// same credential -- in a single-credential deployment, the application's own
// pool -- therefore became owner-equivalent for the duration, and I-22's
// append-only triggers were droppable by the application while the deploy ran.
//
// This test is the assertion nobody had written: a SECOND connection on the
// SAME credential, opened while migrations 002..N are in flight, must see
// nothing. It is deterministic rather than racing a fast migration run: an
// exclusive lock on a table migration 002 touches parks the run inside the
// window, and pg_stat_activity is the witness that the probe really ran there.
// Without that witness the probe would pass just as happily by arriving after
// the run finished, which is the shape working-agreements.md §3 is about.

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

func TestMigrate_WindowIsNotVisibleToOtherSessionsOfTheSameCredential(t *testing.T) {
	ctx := context.Background()
	raw := postgrestest.SetupRawDB(t)

	admin, err := pgxpool.New(ctx, raw)
	require.NoError(t, err)
	defer admin.Close()

	var dbName string
	require.NoError(t, admin.QueryRow(ctx, "SELECT current_database()").Scan(&dbName))

	runner, runnerURL := newMigrationRunner(t, admin, raw, "ledger_window")

	// 001 first, on the runner's own authority, so that the window this test
	// is about (migrations 002..N) is the only thing postgres.Migrate has left
	// to do -- and so the lock below can park it there deterministically.
	src, err := postgres.NewMigrationSource()
	require.NoError(t, err)
	baseline, err := migrate.NewWithSourceInstance("iofs", src,
		strings.Replace(runnerURL, "postgres://", "pgx5://", 1))
	require.NoError(t, err)
	require.NoError(t, baseline.Migrate(1))
	_, _ = baseline.Close()

	// And the credential is left in the state 001 leaves a fresh install in:
	// admin option on ledger_owner, no way to act as it without arranging one.
	// See newMigrationRunner.
	stripLedgerOwnerSetOption(t, admin, runner)

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
