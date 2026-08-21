package postgres_test

// Pins for migration 042 (P1 -- DB role least-privilege). I-22:
// docs/INVARIANTS.md. Design: docs/plans/2026-08-21-tamper-evident-ledger-design.md
// §3. Contract: docs/plans/2026-08-21-integrity-hardening-contracts.md §1/§5.
//
// These tests connect as ledger_app / ledger_owner over real network
// sockets (not just inspecting pg_catalog) because the risk being pinned is
// "can a role holding these credentials actually do X", which a catalog
// query alone cannot prove (e.g. a role created NOLOGIN would look fine in
// an ACL query and still be unusable, or vice versa).

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// withRole returns connStr with its user/password replaced, keeping host,
// port, database and query params intact.
func withRole(t testing.TB, connStr, user, password string) string {
	t.Helper()
	u, err := url.Parse(connStr)
	require.NoError(t, err)
	u.User = url.UserPassword(user, password)
	return u.String()
}

// assertPermissionDenied requires err to be a Postgres insufficient_privilege
// error (SQLSTATE 42501) -- the error class Postgres raises uniformly for
// both ACL failures (permission denied for table X) and ownership failures
// (must be owner of relation X), which covers both the GRANT-based and the
// DDL/ownership-based restrictions this migration relies on.
func assertPermissionDenied(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "expected a *pgconn.PgError, got %T: %v", err, err)
	assert.Equal(t, "42501", pgErr.Code, "expected insufficient_privilege (42501), got %s: %s", pgErr.Code, pgErr.Message)
}

// TestMigration042_LedgerAppIsLeastPrivilege proves both halves of I-22:
// (a) before 042, the single connection every environment uses today has
// completely unrestricted DDL over the ledger's append-only table -- so the
// restrictions asserted in (b) are not vacuously true; and (b) after 042,
// ledger_app can do its job (SELECT/INSERT/UPDATE ordinary tables,
// SELECT/INSERT on journal_entries) but cannot DROP/TRUNCATE/ALTER/DELETE
// anything, anywhere.
func TestMigration042_LedgerAppIsLeastPrivilege(t *testing.T) {
	ctx := context.Background()
	connStr := postgrestest.SetupRawDB(t)
	migrateURL := strings.Replace(connStr, "postgres://", "pgx5://", 1)

	source, err := postgres.NewMigrationSource()
	require.NoError(t, err)
	m, err := migrate.NewWithSourceInstance("iofs", source, migrateURL)
	require.NoError(t, err)

	// --- BEFORE: migrate to 041 (one before roles exist) -----------------
	require.NoError(t, m.Migrate(41))

	preRolesConn, err := pgxpool.New(ctx, connStr)
	require.NoError(t, err)
	_, err = preRolesConn.Exec(ctx, "TRUNCATE journal_entries")
	require.NoError(t, err,
		"sanity check failed: pre-042 there must be no role boundary at all -- "+
			"if this TRUNCATE fails, the restrictions asserted below are not proving anything")
	preRolesConn.Close()

	// --- Apply 042 (and everything after it) ------------------------------
	require.NoError(t, m.Up())
	srcErr, dbErr := m.Close()
	require.NoError(t, srcErr)
	require.NoError(t, dbErr)

	adminPool, err := pgxpool.New(ctx, connStr)
	require.NoError(t, err)
	t.Cleanup(adminPool.Close)

	const appPassword = "roles-test-app-password-not-a-real-secret" //nolint:gosec // test-only credential
	_, err = adminPool.Exec(ctx, fmt.Sprintf("ALTER ROLE ledger_app WITH PASSWORD '%s'", appPassword))
	require.NoError(t, err)

	appPool, err := pgxpool.New(ctx, withRole(t, connStr, "ledger_app", appPassword))
	require.NoError(t, err)
	t.Cleanup(appPool.Close)
	require.NoError(t, appPool.Ping(ctx))

	// --- AFTER: ledger_app cannot do any DDL or mutate journal_entries ---
	t.Run("cannot TRUNCATE journal_entries", func(t *testing.T) {
		_, err := appPool.Exec(ctx, "TRUNCATE journal_entries")
		assertPermissionDenied(t, err)
	})

	t.Run("cannot DROP TRIGGER on journal_entries", func(t *testing.T) {
		_, err := appPool.Exec(ctx, "DROP TRIGGER journal_entries_no_update ON journal_entries")
		assertPermissionDenied(t, err)
	})

	t.Run("cannot ALTER TABLE journal_entries", func(t *testing.T) {
		_, err := appPool.Exec(ctx, "ALTER TABLE journal_entries ADD COLUMN pwned INT")
		assertPermissionDenied(t, err)
	})

	t.Run("cannot CREATE TABLE (no DDL of any kind)", func(t *testing.T) {
		_, err := appPool.Exec(ctx, "CREATE TABLE evil (id INT)")
		assertPermissionDenied(t, err)
	})

	t.Run("cannot UPDATE journal_entries", func(t *testing.T) {
		_, err := appPool.Exec(ctx, "UPDATE journal_entries SET amount = amount WHERE id < 0")
		assertPermissionDenied(t, err)
	})

	t.Run("cannot DELETE from journal_entries", func(t *testing.T) {
		_, err := appPool.Exec(ctx, "DELETE FROM journal_entries WHERE id < 0")
		assertPermissionDenied(t, err)
	})

	t.Run("cannot read or write schema_migrations", func(t *testing.T) {
		_, err := appPool.Exec(ctx, "SELECT * FROM schema_migrations")
		assertPermissionDenied(t, err)
	})

	// --- AFTER: ledger_app can still do its actual job --------------------
	t.Run("can SELECT/INSERT/UPDATE an ordinary table", func(t *testing.T) {
		_, err := appPool.Exec(ctx, "INSERT INTO currencies (uid, code, name) VALUES (gen_random_uuid(), 'RT1', 'RoleTest1')")
		require.NoError(t, err)
		_, err = appPool.Exec(ctx, "UPDATE currencies SET name = 'RoleTest1x' WHERE code = 'RT1'")
		require.NoError(t, err)
		var name string
		require.NoError(t, appPool.QueryRow(ctx, "SELECT name FROM currencies WHERE code = 'RT1'").Scan(&name))
		assert.Equal(t, "RoleTest1x", name)
	})

	t.Run("can SELECT/INSERT journal_entries", func(t *testing.T) {
		currencyUID := postgrestest.SeedCurrency(t, adminPool, "RT2", "RoleTest2")
		classUID := postgrestest.SeedClassification(t, adminPool, "rt_wallet", "RT Wallet", "debit", false)
		jtUID := postgrestest.SeedJournalType(t, adminPool, "rt_deposit", "RT Deposit")

		var journalID int64
		require.NoError(t, adminPool.QueryRow(ctx, `
			INSERT INTO journals (uid, journal_type_id, idempotency_key, total_debit, total_credit)
			SELECT gen_random_uuid(), jt.id, 'roles-test-j1', 10, 10
			FROM journal_types jt WHERE jt.uid = $1::uuid
			RETURNING id
		`, jtUID).Scan(&journalID))

		currencyID := postgrestest.InternalID(t, adminPool, "currencies", currencyUID)
		classID := postgrestest.InternalID(t, adminPool, "classifications", classUID)

		_, err := appPool.Exec(ctx, `
			INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
			VALUES ($1, 999001, $2, $3, 'debit', 10)
		`, journalID, currencyID, classID)
		require.NoError(t, err, "ledger_app must be able to INSERT into journal_entries")

		var amount string
		require.NoError(t, appPool.QueryRow(ctx, `
			SELECT amount::text FROM journal_entries WHERE journal_id = $1
		`, journalID).Scan(&amount))
		assert.Equal(t, "10.000000000000000000", amount)
	})
}

// TestMigration042_DoesNotStrandTheMigrationRunner is the pin
// code-reviewer flagged: testcontainers' bootstrap user and the
// docker-compose POSTGRES_USER are both real Postgres superusers, so a
// prior version of this migration (transferring table/sequence ownership to
// ledger_owner and REVOKE ALL ON SCHEMA public FROM PUBLIC) silently passed
// every other test in this file while actually locking a non-superuser
// migration runner out of its own database the moment the migration
// committed -- exactly the shape of a managed-Postgres master user (RDS's
// master user is not a real superuser; Cloud SQL/Neon are similar).
//
// This simulates that: a NOSUPERUSER role owns the database (and,
// transitively via pg_database_owner in PG15+, the public schema and
// everything it creates in it -- precisely how today's single-connection
// deployments end up owning the ledger's tables), runs every migration
// through its own connection, and then must still be able to do the same
// write it could do before migrating.
func TestMigration042_DoesNotStrandTheMigrationRunner(t *testing.T) {
	ctx := context.Background()
	connStr := postgrestest.SetupRawDB(t)

	adminPool, err := pgxpool.New(ctx, connStr)
	require.NoError(t, err)
	t.Cleanup(adminPool.Close)

	u, err := url.Parse(connStr)
	require.NoError(t, err)
	dbName := strings.TrimPrefix(u.Path, "/")
	require.NotEmpty(t, dbName)

	const runnerPassword = "roles-test-runner-password-not-a-real-secret" //nolint:gosec // test-only credential
	_, err = adminPool.Exec(ctx, fmt.Sprintf("CREATE ROLE migration_runner LOGIN CREATEROLE NOSUPERUSER PASSWORD '%s'", runnerPassword))
	require.NoError(t, err)
	_, err = adminPool.Exec(ctx, fmt.Sprintf("ALTER DATABASE %s OWNER TO migration_runner", dbName))
	require.NoError(t, err, "sanity: migration_runner must actually own the database (and therefore the public schema) before migrating, or this test would not be simulating anything")

	runnerURL := withRole(t, connStr, "migration_runner", runnerPassword)
	migrateURL := strings.Replace(runnerURL, "postgres://", "pgx5://", 1)

	require.NoError(t, postgres.Migrate(migrateURL), "migrating as the (non-superuser) role that owns everything must not fail")

	runnerPool, err := pgxpool.New(ctx, runnerURL)
	require.NoError(t, err)
	t.Cleanup(runnerPool.Close)

	_, err = runnerPool.Exec(ctx, "INSERT INTO currencies (uid, code, name) VALUES (gen_random_uuid(), 'MR1', 'MigrationRunnerTest')")
	require.NoError(t, err, "the role that ran every migration must still be able to write afterward -- a migration must never strand the identity that is running it")
}

// TestMigration042_LedgerAppInsertsIntoPartitionCreatedAfterGrant is the
// partition-inheritance pin the task explicitly calls out: migration 042's
// GRANT on journal_entries only covers the parent and whatever partitions
// exist at migration time. This proves a partition created LATER by
// PartitionService (running as ledger_owner, the role split the "migrate"
// phase intends) is still writable by ledger_app through the parent table
// name, without any additional GRANT ever being issued on that partition.
//
// Migration 042 (expand phase, see its header) does not transfer table
// ownership -- that happens in the later "migrate" migration, alongside the
// DATABASE_URL cutover. This test manually applies that expected end state
// to journal_entries only, so the partition-inheritance behavior can be
// pinned on its own without coupling this test to a migration that does not
// exist yet.
func TestMigration042_LedgerAppInsertsIntoPartitionCreatedAfterGrant(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	const ownerPassword = "roles-test-owner-password-not-a-real-secret" //nolint:gosec // test-only credential
	const appPassword = "roles-test-app-password-2-not-a-real-secret"   //nolint:gosec // test-only credential
	_, err := pool.Exec(ctx, fmt.Sprintf("ALTER ROLE ledger_owner WITH PASSWORD '%s'", ownerPassword))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, fmt.Sprintf("ALTER ROLE ledger_app WITH PASSWORD '%s'", appPassword))
	require.NoError(t, err)
	// Simulate the future "migrate" migration's ownership transfer, scoped
	// to just the one table this test needs -- CREATE TABLE ... PARTITION OF
	// requires owning the parent.
	_, err = pool.Exec(ctx, "ALTER TABLE journal_entries OWNER TO ledger_owner")
	require.NoError(t, err)

	baseURL := roleURLFromPool(pool)

	ownerPool, err := pgxpool.New(ctx, withRole(t, baseURL, "ledger_owner", ownerPassword))
	require.NoError(t, err)
	t.Cleanup(ownerPool.Close)
	require.NoError(t, ownerPool.Ping(ctx))

	appPool, err := pgxpool.New(ctx, withRole(t, baseURL, "ledger_app", appPassword))
	require.NoError(t, err)
	t.Cleanup(appPool.Close)
	require.NoError(t, appPool.Ping(ctx))

	// A date far enough out that neither 037's horizon nor any other test in
	// this package could already have created the partition.
	future := time.Now().AddDate(3, 0, 0)
	store := postgres.NewPartitionStore(ownerPool)
	created, err := store.EnsureMonthlyPartitions(ctx, future, 0)
	require.NoError(t, err, "ledger_owner must be able to run the partition-creation DDL PartitionService issues")
	require.NotEmpty(t, created, "expected a brand-new partition for a date 3 years out")

	currencyUID := postgrestest.SeedCurrency(t, pool, "PT1", "PartitionTest")
	classUID := postgrestest.SeedClassification(t, pool, "pt_wallet", "PT Wallet", "debit", false)
	jtUID := postgrestest.SeedJournalType(t, pool, "pt_deposit", "PT Deposit")

	var journalID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO journals (uid, journal_type_id, idempotency_key, total_debit, total_credit)
		SELECT gen_random_uuid(), jt.id, 'roles-test-partition-j1', 5, 5
		FROM journal_types jt WHERE jt.uid = $1::uuid
		RETURNING id
	`, jtUID).Scan(&journalID))
	currencyID := postgrestest.InternalID(t, pool, "currencies", currencyUID)
	classID := postgrestest.InternalID(t, pool, "classifications", classUID)

	_, err = appPool.Exec(ctx, `
		INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at)
		VALUES ($1, 999002, $2, $3, 'debit', 5, $4)
	`, journalID, currencyID, classID, future)
	require.NoError(t, err, "ledger_app must be able to insert into a partition ledger_owner created after migration 042's GRANT ran")

	var count int
	require.NoError(t, pool.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s", created[0])).Scan(&count))
	assert.Equal(t, 1, count, "the row must have physically routed into the new partition, not the default")
}

// roleURLFromPool derives a connection string from an already-connected
// pool's resolved config, so callers can build role-specific URLs against
// the same (dynamically-assigned, testcontainers) host/port/database
// without re-parsing a DSN string.
func roleURLFromPool(pool *pgxpool.Pool) string {
	cfg := pool.Config().ConnConfig
	return fmt.Sprintf("postgres://placeholder:placeholder@%s:%d/%s?sslmode=disable", cfg.Host, cfg.Port, cfg.Database)
}

// TestMigration042_RoleAttributes pins the static catalog shape (role
// attributes + grants) that the behavioral tests above build on. Migration
// 042 is expand-only (see its header): it does not transfer table
// ownership, so this test does not assert any -- that assertion belongs to
// whichever future "migrate" migration actually performs the transfer.
func TestMigration042_RoleAttributes(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rows, err := pool.Query(ctx, `
		SELECT rolname, rolcanlogin, rolsuper, rolcreatedb, rolcreaterole
		FROM pg_roles WHERE rolname IN ('ledger_owner', 'ledger_app', 'ledger_ro')
	`)
	require.NoError(t, err)
	defer rows.Close()

	type attrs struct{ canLogin, super, createDB, createRole bool }
	got := map[string]attrs{}
	for rows.Next() {
		var name string
		var a attrs
		require.NoError(t, rows.Scan(&name, &a.canLogin, &a.super, &a.createDB, &a.createRole))
		got[name] = a
	}
	require.NoError(t, rows.Err())
	require.Len(t, got, 3, "expected ledger_owner, ledger_app, ledger_ro to all exist")

	for _, name := range []string{"ledger_owner", "ledger_app", "ledger_ro"} {
		a, ok := got[name]
		require.True(t, ok, "%s must exist", name)
		assert.True(t, a.canLogin, "%s must be able to log in", name)
		assert.False(t, a.super, "%s must not be superuser", name)
		assert.False(t, a.createDB, "%s must not be able to create databases", name)
		assert.False(t, a.createRole, "%s must not be able to create roles", name)
	}

	// ledger_app: SELECT/INSERT/UPDATE on an ordinary table, SELECT/INSERT
	// only (no UPDATE) on journal_entries, nothing on schema_migrations.
	assertGrants(t, pool, "ledger_app", "currencies", []string{"SELECT", "INSERT", "UPDATE"})
	assertGrants(t, pool, "ledger_app", "journal_entries", []string{"SELECT", "INSERT"})
	assertGrants(t, pool, "ledger_app", "schema_migrations", nil)

	// ledger_ro: SELECT everywhere, including journal_entries.
	assertGrants(t, pool, "ledger_ro", "currencies", []string{"SELECT"})
	assertGrants(t, pool, "ledger_ro", "journal_entries", []string{"SELECT"})
}

// assertGrants queries information_schema.role_table_grants for exactly the
// privilege types (grantee, table) holds on table -- nil/empty means "no
// grants at all".
func assertGrants(t *testing.T, pool *pgxpool.Pool, grantee, table string, want []string) {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `
		SELECT privilege_type FROM information_schema.role_table_grants
		WHERE grantee = $1 AND table_schema = 'public' AND table_name = $2
	`, grantee, table)
	require.NoError(t, err)
	defer rows.Close()

	var got []string
	for rows.Next() {
		var p string
		require.NoError(t, rows.Scan(&p))
		got = append(got, p)
	}
	require.NoError(t, rows.Err())
	assert.ElementsMatch(t, want, got, "%s privileges on %s", grantee, table)
}

// TestMigration042_DownDropsRolesAndRestoresOwnership pins that the rollback
// path is safe to actually run: the three roles are gone afterward, and the
// original connection -- which never lost anything, since 042 is
// additive-only (see its header) -- can still operate normally.
func TestMigration042_DownDropsRolesAndRestoresOwnership(t *testing.T) {
	ctx := context.Background()
	connStr := postgrestest.SetupRawDB(t)
	migrateURL := strings.Replace(connStr, "postgres://", "pgx5://", 1)

	source, err := postgres.NewMigrationSource()
	require.NoError(t, err)
	m, err := migrate.NewWithSourceInstance("iofs", source, migrateURL)
	require.NoError(t, err)

	require.NoError(t, m.Up())
	require.NoError(t, m.Steps(-1)) // rolls back exactly 042
	srcErr, dbErr := m.Close()
	require.NoError(t, srcErr)
	require.NoError(t, dbErr)

	pool, err := pgxpool.New(ctx, connStr)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	var roleCount int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_roles WHERE rolname IN ('ledger_owner', 'ledger_app', 'ledger_ro')
	`).Scan(&roleCount))
	assert.Equal(t, 0, roleCount, "down migration must drop all three roles")

	_, err = pool.Exec(ctx, "INSERT INTO currencies (uid, code, name) VALUES (gen_random_uuid(), 'DT1', 'DownTest')")
	require.NoError(t, err, "the original connection must still own everything (and be able to write) after rollback")
}
