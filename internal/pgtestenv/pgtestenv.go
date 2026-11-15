// Package pgtestenv holds the PostgreSQL test-environment facts shared by the
// integration fixture (internal/postgrestest) and the end-of-run cleanup
// command (internal/pgtestenv/pgtestclean): which server the suite connects
// to by default, how test databases and per-test roles are named, and how a
// finished run removes what it created without touching anyone else's.
//
// The suite runs against ONE PostgreSQL cluster -- by default the machine's
// shared development instance -- and isolates by database name and role name,
// never by cluster. See docs/TESTING.md.
package pgtestenv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5"
)

// DefaultDatabaseURL is the server the integration suite uses when
// DATABASE_URL is unset: the shared local PostgreSQL on 127.0.0.1:5432 with
// its `dev` superuser. Test-only; never a production connection string.
const DefaultDatabaseURL = "postgres://dev:dev@127.0.0.1:5432/postgres?sslmode=disable"

// ClusterMigrationLockKey mirrors postgres.clusterMigrationLockKey (pinned
// equal by postgres/pgtestenv_lock_key_test.go). Migrate holds it exclusively
// on the cluster's `postgres` database for its whole run, and so does Clean
// while it drops roles, which makes cleanup mutually exclusive with every
// concurrent Migrate on the same server.
const ClusterMigrationLockKey int64 = 2573143714

// DefaultRoleNames are the cluster-wide roles the production migrations
// create. Ordinary tests share them (the migrations create them idempotently,
// exactly as a second ledger database on a production cluster would).
var DefaultRoleNames = []string{"ledger_owner", "ledger_app", "ledger_ro"}

// testDatabaseName matches only the names DatabaseName generates:
// ledger_test_<pid>_<counter>. A wt-assigned `ledger_test_<hex>` name or any
// other project's database never matches.
var testDatabaseName = regexp.MustCompile(`^ledger_test_([0-9]+)_[0-9]+$`)

// testRoleName matches only roles RolePrefix-derived names produce:
// ledger_test_<pid>_<counter>_{owner,app,ro}.
var testRoleName = regexp.MustCompile(`^ledger_test_([0-9]+)_[0-9]+_(owner|app|ro)$`)

// BaseURL returns DATABASE_URL (pgx5:// normalized to postgres://) or
// DefaultDatabaseURL.
func BaseURL() string {
	if configured := os.Getenv("DATABASE_URL"); configured != "" {
		return strings.Replace(configured, "pgx5://", "postgres://", 1)
	}
	return DefaultDatabaseURL
}

// DatabaseName is the name of the n-th test database created by process pid.
// The pid makes names unique across the package binaries `go test ./...`
// runs concurrently and across concurrent runs from different worktrees on
// the same server; the pid also lets Clean tell a live run's database from a
// crashed one's.
func DatabaseName(pid int, n int64) string {
	return fmt.Sprintf("ledger_test_%d_%d", pid, n)
}

// MaintenanceURL points base at the cluster's `postgres` database, keeping
// everything else (the same rewrite postgres.Migrate uses for its lock).
func MaintenanceURL(base string) (string, error) {
	u, err := url.Parse(strings.Replace(base, "pgx5://", "postgres://", 1))
	if err != nil {
		// Do not wrap: url.Error echoes the raw URL, password included.
		return "", errors.New("pgtestenv: parse database url: malformed")
	}
	u.Path = "/postgres"
	return u.String(), nil
}

// processAlive reports whether pid is a running process on this machine.
// EPERM means it exists but belongs to someone else: alive.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Clean removes what ledger test runs left on the server at base:
//
//   - test databases (ledger_test_<pid>_<n>) and per-test roles
//     (ledger_test_<pid>_<n>_{owner,app,ro}) whose creating process is no
//     longer running -- normally none, since every test drops its own; these
//     are leftovers of a killed run;
//   - the shared default roles ledger_owner / ledger_app / ledger_ro, but only
//     when NO database on the cluster still depends on them (DROP ROLE refuses
//     on any pg_shdepend entry, in any database), so a concurrent run's live
//     test database, or another project that installed the ledger into its own
//     database, keeps them.
//
// It takes the cluster migration lock exclusively first and does nothing if
// the lock is busy (a Migrate or a cluster-role test is in flight somewhere;
// that run's own cleanup will finish the job). Database names belonging to
// live processes are never touched.
func Clean(ctx context.Context, base string, log io.Writer) error {
	maint, err := MaintenanceURL(base)
	if err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, maint)
	if err != nil {
		return fmt.Errorf("pgtestenv: connect: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))

	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", ClusterMigrationLockKey).Scan(&locked); err != nil {
		return fmt.Errorf("pgtestenv: try cluster lock: %w", err)
	}
	if !locked {
		fmt.Fprintln(log, "pgtestclean: cluster migration lock is busy (another ledger test run is active); leaving cleanup to it")
		return nil
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", ClusterMigrationLockKey)
	}()

	dbs, err := names(ctx, conn, "SELECT datname FROM pg_database WHERE datname LIKE 'ledger\\_test\\_%' ORDER BY 1")
	if err != nil {
		return err
	}
	var errs []error
	for _, db := range dbs {
		m := testDatabaseName.FindStringSubmatch(db)
		if m == nil || pidAlive(m[1]) {
			continue
		}
		if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{db}.Sanitize()+" WITH (FORCE)"); err != nil {
			errs = append(errs, fmt.Errorf("drop stale database %s: %w", db, err))
			continue
		}
		fmt.Fprintf(log, "pgtestclean: dropped stale database %s\n", db)
	}

	roles, err := names(ctx, conn, "SELECT rolname FROM pg_roles WHERE rolname LIKE 'ledger\\_test\\_%' ORDER BY 1")
	if err != nil {
		return err
	}
	for _, role := range roles {
		m := testRoleName.FindStringSubmatch(role)
		if m == nil || pidAlive(m[1]) {
			continue
		}
		if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize()); err != nil {
			errs = append(errs, fmt.Errorf("drop stale role %s: %w", role, err))
			continue
		}
		fmt.Fprintf(log, "pgtestclean: dropped stale role %s\n", role)
	}

	for _, role := range DefaultRoleNames {
		var exists, depended bool
		if err := conn.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1),
			       EXISTS (SELECT 1 FROM pg_shdepend d JOIN pg_roles r ON r.oid = d.refobjid
			               WHERE d.refclassid = 'pg_authid'::regclass AND r.rolname = $1 AND d.deptype <> 'p')
		`, role).Scan(&exists, &depended); err != nil {
			return fmt.Errorf("pgtestenv: inspect role %s: %w", role, err)
		}
		if !exists {
			continue
		}
		if depended {
			fmt.Fprintf(log, "pgtestclean: kept %s (objects in some database still depend on it)\n", role)
			continue
		}
		if _, err := conn.Exec(ctx, "DROP ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
			// A dependency that appeared after the check (or one pg_shdepend
			// records that the query above did not anticipate) makes DROP ROLE
			// refuse -- the safe outcome. Report it, keep going.
			fmt.Fprintf(log, "pgtestclean: kept %s (%v)\n", role, err)
			continue
		}
		fmt.Fprintf(log, "pgtestclean: dropped %s\n", role)
	}
	return errors.Join(errs...)
}

func pidAlive(s string) bool {
	pid, err := strconv.Atoi(s)
	if err != nil {
		return true // unparseable: treat as someone else's, never drop
	}
	return processAlive(pid)
}

func names(ctx context.Context, conn *pgx.Conn, query string) ([]string, error) {
	rows, err := conn.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("pgtestenv: list: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
