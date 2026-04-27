// Package postgrestest hosts the testcontainers-backed PostgreSQL fixture used
// by the ledger's integration tests. It lives in its own Go submodule so the
// heavyweight test dependencies (testcontainers-go, the Docker SDK, moby/*,
// gopsutil, OpenTelemetry) stay out of `go.sum` for library consumers.
//
// Library users never import this package — only ledger's own test suite does.
package postgrestest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	ledgerpg "github.com/azex-ai/ledger/postgres"
)

// SetupDB starts a PostgreSQL container, runs ledger migrations, and returns
// a *pgxpool.Pool. The test is skipped (not failed) when the Docker daemon
// isn't available so contributors can still run unit tests on machines
// without Docker.
func SetupDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:17",
		tcpostgres.WithDatabase("ledger_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
	)
	if err != nil && strings.Contains(err.Error(), "Cannot connect to the Docker daemon") {
		t.Skip("Docker daemon not running, skipping integration test")
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	// Migrate expects a pgx5:// URL for the pgx/v5 driver.
	migrateURL := strings.Replace(connStr, "postgres://", "pgx5://", 1)
	require.NoError(t, ledgerpg.Migrate(migrateURL))

	pool, err := pgxpool.New(ctx, connStr)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	require.NoError(t, pool.Ping(ctx))
	return pool
}

// SeedCurrency creates a test currency row and returns its ID.
func SeedCurrency(t *testing.T, pool *pgxpool.Pool, code, name string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		"INSERT INTO currencies (code, name) VALUES ($1, $2) RETURNING id",
		code, name,
	).Scan(&id)
	require.NoError(t, err)
	return id
}

// SeedClassification creates a test classification row and returns its ID.
func SeedClassification(t *testing.T, pool *pgxpool.Pool, code, name, normalSide string, isSystem bool) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		"INSERT INTO classifications (code, name, normal_side, is_system) VALUES ($1, $2, $3, $4) RETURNING id",
		code, name, normalSide, isSystem,
	).Scan(&id)
	require.NoError(t, err)
	return id
}

// SeedJournalType creates a test journal_type row and returns its ID.
func SeedJournalType(t *testing.T, pool *pgxpool.Pool, code, name string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		"INSERT INTO journal_types (code, name) VALUES ($1, $2) RETURNING id",
		code, name,
	).Scan(&id)
	require.NoError(t, err)
	return id
}

// keyCounter generates monotonically-increasing suffixes for idempotency keys
// inside a single test binary. Tests are not concurrent w.r.t. this counter,
// which is fine — every call inside one test process gets a unique key.
var keyCounter int

// UniqueKey returns a unique idempotency key by appending a counter to prefix.
func UniqueKey(prefix string) string {
	keyCounter++
	return fmt.Sprintf("%s-%d", prefix, keyCounter)
}
