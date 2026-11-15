// Package postgrestest hosts the PostgreSQL fixture used by the ledger's
// integration tests. Every test process connects to ONE server -- DATABASE_URL,
// or by default the shared local instance pgtestenv.DefaultDatabaseURL -- and
// every test gets a fresh database there, dropped on cleanup. No container,
// no second cluster: isolation is by database name (and, for the destructive
// migration roundtrip, by role name -- see roles.go). See docs/TESTING.md.
package postgrestest

import (
	"context"
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, base)
	require.NoError(t, err)
	defer admin.Close()
	require.NoErrorf(t, admin.Ping(ctx),
		"cannot reach the test PostgreSQL server, so this integration test cannot run. This is a failure, not a skip: "+
			"start the shared local instance (docs/TESTING.md), point DATABASE_URL at a test server, or run "+
			"`make test-short` to skip every integration test explicitly")
	_, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	require.NoError(t, err)

	u, err := url.Parse(base)
	require.NoError(t, err)
	u.Path = "/" + name
	connStr := u.String()
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
	return connStr
}

// SetupRawDB creates an isolated PostgreSQL database WITHOUT running migrations
// and returns its connection string. For tests that drive golang-migrate
// manually (e.g. migrate to an intermediate version, seed, continue). An
// unreachable server fails the test; only explicit -short skips it.
func SetupRawDB(t testing.TB) string {
	t.Helper()
	return isolatedConnection(t)
}

// SetupDB creates a fresh database on the test server, runs ledger migrations,
// and returns a *pgxpool.Pool. An unreachable server fails the test. Only
// explicit -short skips integration tests.
//
// Accepts testing.TB so it can be reused from benchmarks as well as tests.
func SetupDB(t testing.TB) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	connStr := isolatedConnection(t)

	// Migrate expects a pgx5:// URL for the pgx/v5 driver.
	migrateURL := strings.Replace(connStr, "postgres://", "pgx5://", 1)
	require.NoError(t, ledgerpg.Migrate(migrateURL))

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
