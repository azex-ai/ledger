package postgrestest

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRequireSeparateCluster_RejectsAnotherDatabaseOnSameServer(t *testing.T) {
	ordinary := baseConnection(t)
	otherDatabase := SetupRawDB(t)
	require.NotEqual(t, ordinary, otherDatabase)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.ErrorContains(t, requireSeparateCluster(ctx, ordinary, otherDatabase), "same PostgreSQL cluster")
}

func TestRequireSeparateCluster_QueryFailureIsNotIsolation(t *testing.T) {
	ordinary := baseConnection(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, requireSeparateCluster(ctx, ordinary, ordinary))
}

func TestSetupIsolatedRawDB_UsesConfiguredCluster(t *testing.T) {
	baseConnection(t) // honor -short before any container operation
	configured := strings.Replace(os.Getenv("LEDGER_TEST_ISOLATED_DATABASE_URL"), "pgx5://", "postgres://", 1)
	if configured == "" {
		container, connStr, err := startContainer(t)
		require.NoError(t, err)
		t.Cleanup(func() { terminateContainer(t, container) })
		configured = connStr
	}
	// Match the pgx5 scheme accepted by the ordinary fixture as well.
	t.Setenv("LEDGER_TEST_ISOLATED_DATABASE_URL", strings.Replace(configured, "postgres://", "pgx5://", 1))
	actual := SetupIsolatedRawDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// A fresh database, on exactly the configured cluster rather than a
	// silently created fallback container. No migration runs on this DB.
	require.NotEqual(t, configured, actual)
	require.ErrorContains(t, requireSeparateCluster(ctx, configured, actual), "same PostgreSQL cluster")
}
