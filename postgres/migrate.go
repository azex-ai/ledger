package postgres

import (
	"context"
	"embed"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
)

// clusterMigrationLockKey is the pg_advisory_lock key acquireClusterLock
// holds for the duration of every Migrate() call. Value is
// crc32(azex-ai/ledger:cluster-migration-lock) — arbitrary, just fixed and
// documented so a collision is a deliberate choice, not an accident. See
// acquireClusterLock and docs/INVARIANTS.md I-47 for why this exists.
const clusterMigrationLockKey = 2573143714

//go:embed sql/migrations/*.sql
var migrations embed.FS

// NewMigrationSource returns a fresh iofs source over the embedded migration
// files. Exposed so tests can drive golang-migrate directly (e.g. migrate to
// an intermediate version, seed data, then continue) — production callers use
// Migrate, which always runs to the latest version.
func NewMigrationSource() (source.Driver, error) {
	d, err := iofs.New(migrations, "sql/migrations")
	if err != nil {
		return nil, fmt.Errorf("postgres: migrate: init source: %w", err)
	}
	return d, nil
}

// Migrate runs all pending schema migrations against the given database URL.
//
// Accepts either scheme. golang-migrate selects its driver by URL scheme and
// only knows "pgx5", but every other entry point in this library -- pgxpool,
// the examples, DATABASE_URL itself -- speaks "postgres", and requiring the
// caller to hold two spellings of one connection string is a trap they hit at
// the first line of their integration. Normalizing here costs nothing.
func Migrate(databaseURL string) error {
	databaseURL = toMigrateURL(databaseURL)

	if err := waitForDatabase(databaseURL, 10*time.Second); err != nil {
		return fmt.Errorf("postgres: migrate: wait for database: %w", err)
	}

	unlock, err := acquireClusterLock(databaseURL)
	if err != nil {
		return fmt.Errorf("postgres: migrate: acquire cluster lock: %w", err)
	}
	defer unlock()

	source, err := iofs.New(migrations, "sql/migrations")
	if err != nil {
		return fmt.Errorf("postgres: migrate: init source: %w", err)
	}

	m, err := migrate.NewWithSourceInstance("iofs", source, databaseURL)
	if err != nil {
		return fmt.Errorf("postgres: migrate: init migrate: %w", err)
	}
	// Close errors on a completed migration are non-actionable (errcheck excludes Close).
	defer m.Close()

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("postgres: migrate: up: %w", err)
	}
	return nil
}

// toMigrateURL rewrites a postgres:// or postgresql:// URL to the pgx5://
// scheme golang-migrate's driver registry is keyed on. A URL already using
// pgx5:// is returned unchanged.
func toMigrateURL(databaseURL string) string {
	switch {
	case strings.HasPrefix(databaseURL, "postgresql://"):
		return "pgx5://" + strings.TrimPrefix(databaseURL, "postgresql://")
	case strings.HasPrefix(databaseURL, "postgres://"):
		return "pgx5://" + strings.TrimPrefix(databaseURL, "postgres://")
	default:
		return databaseURL
	}
}

// acquireClusterLock serializes every Migrate() call against a Postgres
// cluster, regardless of which database each call targets. It returns a
// function that releases the lock; the caller must defer it.
//
// 001_baseline and 007_role_hardening_and_partition_security_definer issue
// CREATE ROLE / ALTER ROLE / GRANT <role> TO <role> against ledger_owner,
// ledger_app and ledger_ro. Roles (pg_authid) and role membership
// (pg_auth_members) are cluster-wide shared catalogs — they are not scoped
// to whichever database a session happens to be connected to. Two Migrate()
// calls installing into two DIFFERENT databases on the same cluster
// therefore raced on those rows, and Postgres rejected the loser's UPDATE
// with "tuple concurrently updated" instead of blocking it, once its own
// transaction was unblocked by the winner's commit (reproduced directly:
// two concurrent `ALTER ROLE` sessions against two different databases on
// one cluster, no schema involved).
//
// golang-migrate's own advisory lock (database/pgx/v5, Postgres.Lock) does
// not help here: its key is derived from the *target database name*
// (database.GenerateAdvisoryLockId), and PostgreSQL's pg_advisory_lock is
// itself scoped to the database of the connection that took it — verified
// empirically, two sessions connected to two different databases on one
// cluster do not contend for the same advisory-lock key at all. Locking
// against the database Migrate() is about to migrate can therefore never
// serialize two callers targeting two different databases.
//
// The lock instead has to come from a database every caller can reach no
// matter which database it is about to migrate: `postgres`, the
// maintenance database every PostgreSQL cluster creates at initdb time.
// This is an additional install prerequisite — CONNECT on `postgres` — see
// docs/RUNBOOK.md's "Database roles" operational notes. It is strictly
// narrower than the CREATEROLE/ownership authority 001_baseline already
// requires of the same connection, and CONNECT on every database is
// granted to PUBLIC by default.
//
// Session-level (not transaction-level): the lock must still be held while
// golang-migrate opens its own separate connection to the target database
// and runs every pending migration's transaction on it, so it cannot live
// inside one of those transactions. It is released by closing the
// connection that holds it, which Postgres does automatically for
// session-level advisory locks — no explicit pg_advisory_unlock call is
// needed on the success path or any error path.
func acquireClusterLock(databaseURL string) (unlock func(), err error) {
	lockURL, err := maintenanceDatabaseURL(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("derive maintenance database url: %w", err)
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, lockURL)
	if err != nil {
		return nil, fmt.Errorf("connect to maintenance database: %w", err)
	}

	// Blocks indefinitely until acquired -- that is the point: every other
	// Migrate() call against this cluster waits here rather than racing.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", int64(clusterMigrationLockKey)); err != nil {
		_ = conn.Close(ctx)
		return nil, fmt.Errorf("pg_advisory_lock: %w", err)
	}

	return func() { _ = conn.Close(ctx) }, nil
}

// maintenanceDatabaseURL rewrites databaseURL to point at the cluster's
// `postgres` maintenance database, keeping scheme, credentials, host, port
// and query parameters unchanged. databaseURL must already be in pgx.Connect
// form (postgres:// or pgx5://, not a bare DSN).
func maintenanceDatabaseURL(databaseURL string) (string, error) {
	u, err := url.Parse(strings.Replace(databaseURL, "pgx5://", "postgres://", 1))
	if err != nil {
		// Do NOT wrap err: net/url.Error.Error() echoes the raw URL, password
		// and all. A malformed DATABASE_URL must not spill its credentials into
		// logs.
		return "", fmt.Errorf("parse database url: malformed")
	}
	u.Path = "/postgres"
	return u.String(), nil
}

func waitForDatabase(databaseURL string, timeout time.Duration) error {
	pingURL := strings.Replace(databaseURL, "pgx5://", "postgres://", 1)
	ctx := context.Background()

	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := pgx.Connect(ctx, pingURL)
		if err == nil {
			pingErr := conn.Ping(ctx)
			conn.Close(ctx)
			if pingErr == nil {
				return nil
			}
			lastErr = pingErr
		} else {
			lastErr = err
		}
		time.Sleep(250 * time.Millisecond)
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("timed out after %s", timeout)
	}
	return lastErr
}
