package postgres

import (
	"context"
	"embed"
	"fmt"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
)

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
