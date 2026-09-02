package postgres_test

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// clusterMigrationLockKey duplicates the unexported constant in
// postgres/migrate.go on purpose: this test is about the externally
// observable behaviour of a lock a *foreign* process holds, so it must name
// the key the way that process would -- as a literal. If the constant ever
// changes, this test starts passing for the wrong reason, which is why the
// assertion below also requires the error message to name the key.
const clusterMigrationLockKey = 2573143714

// recordingLogger captures the Info lines Migrate emits while waiting.
type recordingLogger struct{ info []string }

func (l *recordingLogger) Info(msg string, _ ...any)  { l.info = append(l.info, msg) }
func (l *recordingLogger) Warn(msg string, _ ...any)  {}
func (l *recordingLogger) Error(msg string, _ ...any) {}

var _ core.Logger = (*recordingLogger)(nil)

// TestMigrate_ClusterLockHeldElsewhere_FailsWithinBudget pins B-m4: a
// Migrate that cannot get the cluster migration lock must fail with a
// diagnosable error inside a bounded time, and must say out loud that it is
// waiting. Before the fix this was a bare blocking pg_advisory_lock on
// context.Background() with no timeout, no log line and no cancellation --
// so a leaked lock (SIGKILLed holder + half-open TCP connection) hung every
// Migrate on the cluster indefinitely and silently. Remove the budget and
// this test hangs until the -timeout kills the binary.
func TestMigrate_ClusterLockHeldElsewhere_FailsWithinBudget(t *testing.T) {
	connStr := postgrestest.SetupRawDB(t)
	ctx := context.Background()

	// Hold the lock from an independent session on the cluster's maintenance
	// database -- the same database and key acquireClusterLock uses.
	maintenanceURL := maintenanceURLFor(t, connStr)
	holder, err := pgx.Connect(ctx, maintenanceURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Close(context.Background()) })
	_, err = holder.Exec(ctx, "SELECT pg_advisory_lock($1)", int64(clusterMigrationLockKey))
	require.NoError(t, err)

	logger := &recordingLogger{}
	migrateURL := strings.Replace(connStr, "postgres://", "pgx5://", 1)

	done := make(chan error, 1)
	go func() {
		done <- postgres.Migrate(migrateURL,
			postgres.WithMigrateLockBudget(2*time.Second),
			postgres.WithMigrateLogger(logger),
		)
	}()

	select {
	case err := <-done:
		require.Error(t, err, "Migrate must not claim success while another session holds the cluster lock")
		assert.Contains(t, err.Error(), "cluster migration lock")
		assert.Contains(t, err.Error(), "2573143714",
			"the error must name the advisory key so an operator can find the holder in pg_locks")
	case <-time.After(30 * time.Second):
		t.Fatal("Migrate never returned: the cluster migration lock wait is unbounded again (B-m4)")
	}

	assert.NotEmpty(t, logger.info,
		"Migrate must log that it is waiting for the cluster migration lock -- a startup path that can block on a cluster-wide lock and says nothing is the whole failure this closed")
}

// TestMigrate_ClusterLockReleased_Succeeds is the companion: the bounded wait
// is a wait, not a fast fail. Migrate must still succeed once the foreign
// holder lets go.
func TestMigrate_ClusterLockReleased_Succeeds(t *testing.T) {
	connStr := postgrestest.SetupRawDB(t)
	ctx := context.Background()

	maintenanceURL := maintenanceURLFor(t, connStr)
	holder, err := pgx.Connect(ctx, maintenanceURL)
	require.NoError(t, err)
	_, err = holder.Exec(ctx, "SELECT pg_advisory_lock($1)", int64(clusterMigrationLockKey))
	require.NoError(t, err)

	go func() {
		time.Sleep(3 * time.Second)
		_ = holder.Close(context.Background())
	}()

	migrateURL := strings.Replace(connStr, "postgres://", "pgx5://", 1)
	require.NoError(t, postgres.Migrate(migrateURL, postgres.WithMigrateLockBudget(60*time.Second)))
}

// TestMigrateContext_CancelledWhileWaiting pins the ctx-aware sibling: a boot
// sequence being torn down stops waiting instead of holding the process open.
func TestMigrateContext_CancelledWhileWaiting(t *testing.T) {
	connStr := postgrestest.SetupRawDB(t)
	bg := context.Background()

	maintenanceURL := maintenanceURLFor(t, connStr)
	holder, err := pgx.Connect(bg, maintenanceURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Close(context.Background()) })
	_, err = holder.Exec(bg, "SELECT pg_advisory_lock($1)", int64(clusterMigrationLockKey))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(bg, 2*time.Second)
	defer cancel()

	migrateURL := strings.Replace(connStr, "postgres://", "pgx5://", 1)
	done := make(chan error, 1)
	go func() {
		done <- postgres.MigrateContext(ctx, migrateURL, postgres.WithMigrateLockBudget(5*time.Minute))
	}()

	select {
	case err := <-done:
		require.Error(t, err)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(30 * time.Second):
		t.Fatal("MigrateContext ignored its context while waiting for the cluster migration lock")
	}
}

func maintenanceURLFor(t *testing.T, connStr string) string {
	t.Helper()
	u, err := url.Parse(strings.Replace(connStr, "pgx5://", "postgres://", 1))
	require.NoError(t, err)
	u.Path = "/postgres"
	return u.String()
}
