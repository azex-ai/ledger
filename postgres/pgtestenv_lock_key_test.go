package postgres_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/pgtestenv"
	"github.com/azex-ai/ledger/postgres"
)

// pgtestclean drops the shared test roles only while holding the cluster
// migration lock, so that it can never interleave with a Migrate. That only
// works if both sides lock the same key.
func TestPgtestenvLocksTheClusterMigrationKey(t *testing.T) {
	require.Equal(t, int64(postgres.ClusterMigrationLockKeyForTest), pgtestenv.ClusterMigrationLockKey)
}
