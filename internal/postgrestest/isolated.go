package postgrestest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// SetupIsolatedRawDB creates an unmigrated database on a dedicated cluster for
// tests that drop cluster-wide roles. A different database on the ordinary
// cluster is NOT sufficient. Only -short skips; unavailable infrastructure or
// unprovable isolation fails the test. See docs/TESTING.md.
func SetupIsolatedRawDB(t testing.TB) string {
	t.Helper()
	ordinary := baseConnection(t) // also enforces explicit -short behavior
	isolated := strings.Replace(os.Getenv("LEDGER_TEST_ISOLATED_DATABASE_URL"), "pgx5://", "postgres://", 1)
	if isolated == "" {
		container, connStr, err := startContainer(t)
		require.NoError(t, err, "destructive migration tests need Docker or LEDGER_TEST_ISOLATED_DATABASE_URL on a dedicated cluster")
		t.Cleanup(func() { terminateContainer(t, container) })
		isolated = connStr
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	require.NoError(t, requireSeparateCluster(ctx, ordinary, isolated))
	// Registered after container cleanup: the database is dropped first.
	return createDatabase(t, isolated)
}

func requireSeparateCluster(ctx context.Context, ordinary, isolated string) error {
	clusterID := func(connStr string) (string, error) {
		pool, err := pgxpool.New(ctx, connStr)
		if err != nil {
			return "", err
		}
		defer pool.Close()
		var id string
		err = pool.QueryRow(ctx, "SELECT system_identifier::text FROM pg_control_system()").Scan(&id)
		return id, err
	}
	ordinaryID, err := clusterID(ordinary)
	if err != nil {
		return fmt.Errorf("verify ordinary test cluster identity: %w", err)
	}
	isolatedID, err := clusterID(isolated)
	if err != nil {
		return fmt.Errorf("verify isolated test cluster identity: %w", err)
	}
	if ordinaryID == "" || isolatedID == "" || ordinaryID == isolatedID {
		return fmt.Errorf("LEDGER_TEST_ISOLATED_DATABASE_URL must use a dedicated cluster: same PostgreSQL cluster or missing identity")
	}
	return nil
}

// Serialize startup only, sharing the existing cross-package Ryuk mutex.
// BasicWaitStrategies waits for PostgreSQL's final startup and the mapped port,
// including Docker Desktop's host proxy, before ConnectionString is consumed.
func startContainer(t testing.TB) (*tcpostgres.PostgresContainer, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	startupLock := flock.New(filepath.Join(os.TempDir(), "ledger-postgrestest-container.lock"))
	locked, err := startupLock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		return nil, "", fmt.Errorf("lock container startup: %w", err)
	}
	if !locked {
		return nil, "", fmt.Errorf("lock container startup: lock not acquired")
	}
	defer func() {
		if err := startupLock.Unlock(); err != nil {
			t.Errorf("release PostgreSQL container startup lock: %v", err)
		}
	}()
	container, err := tcpostgres.Run(ctx, "postgres:17",
		tcpostgres.WithDatabase("postgres"), tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"), tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		terminateContainer(t, container)
		return nil, "", fmt.Errorf("start PostgreSQL container: %w", err)
	}
	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		terminateContainer(t, container)
		return nil, "", fmt.Errorf("PostgreSQL container connection: %w", err)
	}
	return container, connStr, nil
}

func terminateContainer(t testing.TB, container *tcpostgres.PostgresContainer) {
	t.Helper()
	if container == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := container.Terminate(ctx); err != nil {
		t.Errorf("terminate PostgreSQL test container: %v", err)
	}
}
