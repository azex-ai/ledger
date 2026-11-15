// Package postgrestest hosts the PostgreSQL fixture used by the ledger's
// integration tests. Every test process connects to ONE server -- DATABASE_URL,
// or by default the shared local instance pgtestenv.DefaultDatabaseURL -- and
// every test gets a fresh database there, dropped on cleanup. No container,
// no second cluster: isolation is by database name (and, for the destructive
// migration roundtrip, by role name -- see roles.go). See docs/TESTING.md.
package postgrestest

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/pgtestenv"
	ledgerpg "github.com/azex-ai/ledger/postgres"
)

var databaseCounter atomic.Int64

// SharedRolePassword is the one password tests may set on the shared roles
// ledger_app / ledger_ro (ALTER ROLE ... PASSWORD) to log in as them. Roles
// are cluster-wide and every test process on the server shares them, so a
// per-test password would let one process invalidate another's credential
// mid-run; with a single value, concurrent ALTERs are interchangeable.
// Test-only; never a real secret.
const SharedRolePassword = "ledger-test-shared-role-password-not-a-real-secret" //nolint:gosec // test-only credential

// SetSharedRolePassword sets SharedRolePassword on the shared role (ledger_app
// or ledger_ro) of the server pool points at. Every test process on the
// server does this to the same pg_authid row, and two ALTER ROLE statements
// on one row at the same moment make the loser fail with "tuple concurrently
// updated" (XX000). So the ALTER runs on the server's `postgres` database, in
// a transaction holding pgtestenv.SharedRolePasswordLockKey there (advisory
// locks are per database; the per-test databases would not exclude each
// other). scripts/dev-db.sh takes the same lock for the same reason.
func SetSharedRolePassword(ctx context.Context, pool *pgxpool.Pool, role string) error {
	maint, err := pgtestenv.MaintenanceURL(pool.Config().ConnString())
	if err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, maint)
	if err != nil {
		return fmt.Errorf("postgrestest: connect to set %s password: %w", role, err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", pgtestenv.SharedRolePasswordLockKey); err != nil {
			return fmt.Errorf("postgrestest: lock for %s password: %w", role, err)
		}
		if _, err := tx.Exec(ctx, "ALTER ROLE "+pgx.Identifier{role}.Sanitize()+" WITH PASSWORD '"+SharedRolePassword+"'"); err != nil {
			return fmt.Errorf("postgrestest: set %s password: %w", role, err)
		}
		return nil
	})
}

func baseConnection(t testing.TB) string {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode: skipping PostgreSQL integration test")
	}
	// F-9 (2026-09-03 independent review): an unreachable database must FAIL
	// the test, never skip it -- a suite that did not execute is not a suite
	// that passed. The only explicit way to run without a database is -short
	// (above). Reachability is checked by createNamedDatabase's Ping, which
	// fails the test through require.NoErrorf.
	base := pgtestenv.BaseURL()
	runLock.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		// The connection is deliberately never closed: the shared run lock
		// must outlive every test in this process, and the server releases it
		// when the process exits. See pgtestenv.RunLockKey.
		runLock.conn, runLock.err = pgtestenv.HoldRunLock(ctx, base)
		if runLock.err != nil {
			return
		}
		// Held on the same never-closed connection, so it is released exactly
		// when this process exits: while it is held, this process's template
		// is not an orphan (pgtestenv.ReclaimOrphanTemplates).
		lockCtx, lockCancel := context.WithTimeout(ctx, 10*time.Second)
		defer lockCancel()
		runLock.err = pgtestenv.HoldProcessLock(lockCtx, runLock.conn, os.Getpid())
	})
	require.NoErrorf(t, runLock.err,
		"cannot reach the test PostgreSQL server (or take its test-run lock), so this integration test cannot run. "+
			"This is a failure, not a skip: start the shared local instance (docs/TESTING.md), point DATABASE_URL at a "+
			"test server, or run `make test-short` to skip every integration test explicitly")
	return base
}

// runLock is this process's shared hold on pgtestenv.RunLockKey, taken before
// its first database is created; while any test process holds it,
// pgtestclean removes nothing.
var runLock struct {
	once sync.Once
	conn *pgx.Conn
	err  error
}

func isolatedConnection(t testing.TB) string {
	t.Helper()
	return createDatabase(t, baseConnection(t))
}

func createDatabase(t testing.TB, base string) string {
	t.Helper()
	return createNamedDatabase(t, base, nextDatabaseName())
}

// nextDatabaseName is unique across every process sharing the server: the
// counter alone resets in each package's test binary, and `go test ./...`
// (or a second worktree's run) points all of them at the same server, so the
// pid is part of the name (F-X1, 2026-09-02 audit).
func nextDatabaseName() string {
	return pgtestenv.DatabaseName(os.Getpid(), databaseCounter.Add(1))
}

func createNamedDatabase(t testing.TB, base, name string) string {
	t.Helper()
	return createDatabaseFrom(t, base, name, "")
}

// createDatabaseFrom creates database name -- a copy of template when
// template is non-empty -- and drops it when t finishes.
func createDatabaseFrom(t testing.TB, base, name, template string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, base)
	require.NoError(t, err)
	defer admin.Close()
	require.NoErrorf(t, admin.Ping(ctx),
		"cannot reach the test PostgreSQL server, so this integration test cannot run. This is a failure, not a skip: "+
			"start the shared local instance (docs/TESTING.md), point DATABASE_URL at a test server, or run "+
			"`make test-short` to skip every integration test explicitly")
	stmt := "CREATE DATABASE " + pgx.Identifier{name}.Sanitize()
	if template != "" {
		stmt += " TEMPLATE " + pgx.Identifier{template}.Sanitize()
	}
	_, err = admin.Exec(ctx, stmt)
	require.NoError(t, err)

	connStr := databaseURL(t, base, name)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		admin, err := pgxpool.New(cleanupCtx, base)
		if err != nil {
			t.Errorf("connect for database cleanup: %v", err)
			return
		}
		defer admin.Close()
		if _, err := admin.Exec(cleanupCtx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database %s: %v", name, err)
		}
	})
	if template != "" {
		require.NoError(t, applyDatabaseLevelMigrationState(ctx, admin, name))
	}
	return connStr
}

func databaseURL(t testing.TB, base, name string) string {
	t.Helper()
	u, err := url.Parse(base)
	require.NoError(t, err)
	u.Path = "/" + name
	return u.String()
}

// migrated is this process's migrated template database, created by the
// first SetupDB and reused by every later one. It lives as long as the
// process: no single test owns it, so no t.Cleanup can drop it. Its name,
// ledger_test_<pid>_0, is in the cleanup's namespace (n = 0 is never issued
// by nextDatabaseName), so `make test` / `make test-clean-db` removes it
// once no ledger test process is running, and the next test process to
// create its own template drops it once this process has exited
// (pgtestenv.ReclaimOrphanTemplates).
var migrated struct {
	once sync.Once
	name string
	err  error
}

func migratedTemplate(t testing.TB, base string) string {
	t.Helper()
	migrated.once.Do(func() {
		migrated.name, migrated.err = createMigratedTemplate(base)
	})
	require.NoError(t, migrated.err, "create this test process's migrated template database (the first SetupDB "+
		"in this process failed to; every later SetupDB in the process reports that same, cached error)")
	return migrated.name
}

func createMigratedTemplate(base string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pid := os.Getpid()
	name := pgtestenv.TemplateDatabaseName(pid)
	// Templates of exited processes (a direct `go test` leaves one per package
	// binary) are reclaimed here, by the next process to start, since the
	// run-lock-gated cleanup rarely gets a quiet server.
	if err := pgtestenv.ReclaimOrphanTemplates(ctx, base, pid); err != nil {
		return "", fmt.Errorf("postgrestest: template: %w", err)
	}
	admin, err := pgxpool.New(ctx, base)
	if err != nil {
		return "", fmt.Errorf("postgrestest: template: connect: %w", err)
	}
	defer admin.Close()
	// This process holds (ProcessLockNamespace, pid) exclusively (taken in
	// baseConnection before any SetupDB gets here), so a database with this
	// name can only be a leftover of an exited process that had this pid.
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
		return "", fmt.Errorf("postgrestest: template: drop leftover %s: %w", name, err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return "", fmt.Errorf("postgrestest: template: create %s: %w", name, err)
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", errors.New("postgrestest: template: parse database url: malformed")
	}
	u.Path = "/" + name
	// Migrate expects a pgx5:// URL for the pgx/v5 driver.
	if err := ledgerpg.Migrate(strings.Replace(u.String(), "postgres://", "pgx5://", 1)); err != nil {
		return "", fmt.Errorf("postgrestest: template: migrate %s: %w", name, err)
	}
	return name, nil
}

// applyDatabaseLevelMigrationState re-applies, on a copy of the template,
// what the migrations set on the DATABASE object itself. CREATE DATABASE
// ... TEMPLATE copies everything inside the database but not the database's
// own ACL (pg_database.datacl) or its ALTER DATABASE settings, so without
// this a copy would silently lack migration 030's REVOKE TEMPORARY ... FROM
// PUBLIC. TestTemplateCopyMatchesTheTemplate pins that a copy ends up with
// the template's ACL and settings, so a future migration adding
// database-level state fails there instead of passing on a weaker copy.
func applyDatabaseLevelMigrationState(ctx context.Context, admin *pgxpool.Pool, name string) error {
	if _, err := admin.Exec(ctx, "REVOKE TEMPORARY ON DATABASE "+pgx.Identifier{name}.Sanitize()+" FROM PUBLIC"); err != nil {
		return fmt.Errorf("postgrestest: revoke TEMPORARY on %s: %w", name, err)
	}
	return nil
}

// SetupRawDB creates an isolated PostgreSQL database WITHOUT running migrations
// and returns its connection string. For tests that drive golang-migrate
// manually (e.g. migrate to an intermediate version, seed, continue). An
// unreachable server fails the test; only explicit -short skips it.
func SetupRawDB(t testing.TB) string {
	t.Helper()
	return isolatedConnection(t)
}

// SetupDB creates a fresh, fully migrated database on the test server and
// returns a *pgxpool.Pool. An unreachable server fails the test. Only
// explicit -short skips integration tests.
//
// The database is a copy of this process's migrated template (see
// migrated): ledger migrations run once per test process, through
// the same postgres.Migrate a host calls, and every SetupDB after that is a
// CREATE DATABASE ... TEMPLATE. Running Migrate per test made every test
// queue on the cluster-wide migration lock (postgres.clusterMigrationLockKey)
// behind every other test process on the server.
//
// Accepts testing.TB so it can be reused from benchmarks as well as tests.
func SetupDB(t testing.TB) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	base := baseConnection(t)
	template := migratedTemplate(t, base)
	connStr := createDatabaseFrom(t, base, nextDatabaseName(), template)

	pool, err := pgxpool.New(ctx, connStr)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	require.NoError(t, pool.Ping(ctx))
	return pool
}

// SeedCurrency creates a test currency row and returns its uid. The exponent
// column takes its schema default (18 — the loosest setting), matching the
// pre-exponent behavior most callers still rely on.
func SeedCurrency(t *testing.T, pool *pgxpool.Pool, code, name string) string {
	t.Helper()
	var uid string
	err := pool.QueryRow(context.Background(),
		"INSERT INTO currencies (uid, code, name) VALUES (gen_random_uuid(), $1, $2) RETURNING uid::text",
		code, name,
	).Scan(&uid)
	require.NoError(t, err)
	return uid
}

// SeedCurrencyWithExponent creates a test currency row with an explicit
// exponent and returns its uid. Use this (instead of SeedCurrency) whenever a
// test exercises precision enforcement (I-16).
func SeedCurrencyWithExponent(t *testing.T, pool *pgxpool.Pool, code, name string, exponent int32) string {
	t.Helper()
	var uid string
	err := pool.QueryRow(context.Background(),
		"INSERT INTO currencies (uid, code, name, exponent) VALUES (gen_random_uuid(), $1, $2, $3) RETURNING uid::text",
		code, name, exponent,
	).Scan(&uid)
	require.NoError(t, err)
	return uid
}

// SeedClassification creates a test classification row and returns its uid.
func SeedClassification(t *testing.T, pool *pgxpool.Pool, code, name, normalSide string, isSystem bool) string {
	t.Helper()
	var uid string
	err := pool.QueryRow(context.Background(),
		"INSERT INTO classifications (uid, code, name, normal_side, is_system) VALUES (gen_random_uuid(), $1, $2, $3, $4) RETURNING uid::text",
		code, name, normalSide, isSystem,
	).Scan(&uid)
	require.NoError(t, err)
	return uid
}

// SeedClassificationWithRole creates a test classification row with an
// explicit balance_role (”, 'available', 'pending', 'locked') and returns
// its uid. Use this whenever a test exercises the balance breakdown or the
// Reserve availability base.
func SeedClassificationWithRole(t *testing.T, pool *pgxpool.Pool, code, name, normalSide string, isSystem bool, balanceRole string) string {
	t.Helper()
	var uid string
	err := pool.QueryRow(context.Background(),
		"INSERT INTO classifications (uid, code, name, normal_side, is_system, balance_role) VALUES (gen_random_uuid(), $1, $2, $3, $4, $5) RETURNING uid::text",
		code, name, normalSide, isSystem, balanceRole,
	).Scan(&uid)
	require.NoError(t, err)
	return uid
}

// SeedJournalType creates a test journal_type row and returns its uid.
// holder_kind is left at its default (”, untagged) -- use
// SeedJournalTypeWithHolderKind for tests that need a specific
// core.HolderTxKind.
func SeedJournalType(t *testing.T, pool *pgxpool.Pool, code, name string) string {
	t.Helper()
	var uid string
	err := pool.QueryRow(context.Background(),
		"INSERT INTO journal_types (uid, code, name) VALUES (gen_random_uuid(), $1, $2) RETURNING uid::text",
		code, name,
	).Scan(&uid)
	require.NoError(t, err)
	return uid
}

// SeedJournalTypeWithHolderKind creates a test journal_type row tagged with
// an explicit core.HolderTxKind (M-7 fix, docs/INVARIANTS.md I-44) and
// returns its uid. holderKind must be one of core.HolderTxKind's named
// string values (e.g. "deposit") or "" for untagged.
func SeedJournalTypeWithHolderKind(t *testing.T, pool *pgxpool.Pool, code, name, holderKind string) string {
	t.Helper()
	var uid string
	err := pool.QueryRow(context.Background(),
		"INSERT INTO journal_types (uid, code, name, holder_kind) VALUES (gen_random_uuid(), $1, $2, $3) RETURNING uid::text",
		code, name, holderKind,
	).Scan(&uid)
	require.NoError(t, err)
	return uid
}

// keyCounter generates monotonically-increasing suffixes for idempotency keys
// inside a single test binary. Atomic so concurrent tests can call it safely.
var keyCounter atomic.Int64

// UniqueKey returns a unique idempotency key by appending a counter to prefix.
func UniqueKey(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, keyCounter.Add(1))
}

// InternalID resolves a uid string (as returned by the Seed helpers) back to
// the table's internal bigint id. For tests that manipulate or assert against
// internal storage with raw SQL; the internal id never crosses the library's
// public API.
func InternalID(t testing.TB, pool *pgxpool.Pool, table, uid string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		fmt.Sprintf("SELECT id FROM %s WHERE uid = $1::uuid", table), uid,
	).Scan(&id)
	require.NoError(t, err)
	return id
}
