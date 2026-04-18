package postgres_test

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

// setupTestDB starts a PostgreSQL container, runs migrations, and returns a pool.
// Skips the test if Docker is not available.
func setupTestDB(t *testing.T) *pgxpool.Pool {
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
	t.Cleanup(func() { container.Terminate(ctx) })

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	// Migrate expects a pgx5:// URL for the pgx/v5 driver
	migrateURL := strings.Replace(connStr, "postgres://", "pgx5://", 1)
	err = ledgerpg.Migrate(migrateURL)
	require.NoError(t, err)

	pool, err := pgxpool.New(ctx, connStr)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// Verify connection
	err = pool.Ping(ctx)
	require.NoError(t, err)

	return pool
}

// seedCurrency creates a test currency and returns its ID.
func seedCurrency(t *testing.T, pool *pgxpool.Pool, code, name string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		"INSERT INTO currencies (code, name) VALUES ($1, $2) RETURNING id",
		code, name,
	).Scan(&id)
	require.NoError(t, err)
	return id
}

// seedClassification creates a test classification and returns its ID.
func seedClassification(t *testing.T, pool *pgxpool.Pool, code, name, normalSide string, isSystem bool) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		"INSERT INTO classifications (code, name, normal_side, is_system) VALUES ($1, $2, $3, $4) RETURNING id",
		code, name, normalSide, isSystem,
	).Scan(&id)
	require.NoError(t, err)
	return id
}

// seedJournalType creates a test journal type and returns its ID.
func seedJournalType(t *testing.T, pool *pgxpool.Pool, code, name string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		"INSERT INTO journal_types (code, name) VALUES ($1, $2) RETURNING id",
		code, name,
	).Scan(&id)
	require.NoError(t, err)
	return id
}

// uniqueKey generates a unique idempotency key for tests.
var testKeyCounter int

func uniqueKey(prefix string) string {
	testKeyCounter++
	return fmt.Sprintf("%s-%d", prefix, testKeyCounter)
}
