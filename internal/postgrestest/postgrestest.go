// Package postgrestest hosts the testcontainers-backed PostgreSQL fixture used
// by the ledger's integration tests. A test process shares one server while
// every test gets a fresh database, preserving isolation without repeatedly
// paying container startup cost.
package postgrestest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	ledgerpg "github.com/azex-ai/ledger/postgres"
)

var sharedServer struct {
	once    sync.Once
	connStr string
	err     error
}

var databaseCounter atomic.Int64

func baseConnection(t testing.TB) string {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode: skipping PostgreSQL integration test")
	}
	if configured := os.Getenv("DATABASE_URL"); configured != "" {
		return strings.Replace(configured, "pgx5://", "postgres://", 1)
	}
	sharedServer.once.Do(func() {
		ctx := context.Background()
		// Package test binaries run concurrently under `go test ./...`.
		// Serialize only container startup because Docker Desktop can race
		// testcontainers' shared Ryuk creation across processes.
		startupLock := flock.New(filepath.Join(os.TempDir(), "ledger-postgrestest-container.lock"))
		locked, lockErr := startupLock.TryLockContext(ctx, 100*time.Millisecond)
		if lockErr != nil {
			sharedServer.err = fmt.Errorf("lock container startup: %w", lockErr)
			return
		}
		if !locked {
			sharedServer.err = fmt.Errorf("lock container startup: lock not acquired")
			return
		}
		defer func() { _ = startupLock.Unlock() }()
		container, err := tcpostgres.Run(ctx, "postgres:17",
			tcpostgres.WithDatabase("postgres"),
			tcpostgres.WithUsername("test"),
			tcpostgres.WithPassword("test"),
		)
		if err != nil {
			sharedServer.err = err
			return
		}
		// testcontainers' Ryuk sidecar removes the process-scoped shared
		// container after the test binary exits.
		sharedServer.connStr, sharedServer.err = container.ConnectionString(ctx, "sslmode=disable")
	})
	if sharedServer.err != nil && strings.Contains(sharedServer.err.Error(), "Cannot connect to the Docker daemon") {
		t.Skip("Docker daemon not running, skipping integration test")
	}
	require.NoError(t, sharedServer.err)
	return sharedServer.connStr
}

func isolatedConnection(t testing.TB) string {
	t.Helper()
	ctx := context.Background()
	base := baseConnection(t)
	name := fmt.Sprintf("ledger_test_%d", databaseCounter.Add(1))
	admin, err := pgxpool.New(ctx, base)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return admin.Ping(ctx) == nil }, 15*time.Second, 250*time.Millisecond)
	_, err = admin.Exec(ctx, "CREATE DATABASE "+name)
	require.NoError(t, err)
	admin.Close()

	u, err := url.Parse(base)
	require.NoError(t, err)
	u.Path = "/" + name
	connStr := u.String()
	t.Cleanup(func() {
		admin, err := pgxpool.New(ctx, base)
		if err != nil {
			return
		}
		defer admin.Close()
		_, _ = admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
	})
	return connStr
}

// SetupRawDB starts a PostgreSQL container WITHOUT running migrations and
// returns its connection string. For tests that drive golang-migrate manually
// (e.g. migrate to an intermediate version, seed, continue). The test is
// skipped when Docker isn't available.
func SetupRawDB(t testing.TB) string {
	t.Helper()
	return isolatedConnection(t)
}

// SetupDB starts a PostgreSQL container, runs ledger migrations, and returns
// a *pgxpool.Pool. The test is skipped (not failed) when the Docker daemon
// isn't available so contributors can still run unit tests on machines
// without Docker.
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
