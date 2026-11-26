// Package pgtestenv holds the PostgreSQL test-environment facts shared by the
// integration fixture (internal/postgrestest) and the end-of-run cleanup
// command (internal/pgtestenv/pgtestclean): which server the suite connects
// to by default, how test databases and per-test roles are named, the lock
// every test process holds while it runs, and how a finished run removes what
// it created without touching anyone else's.
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
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// DefaultDatabaseURL is the server the integration suite uses when
// DATABASE_URL is unset: the shared local PostgreSQL on 127.0.0.1:5432 with
// its `dev` superuser. Test-only; never a production connection string.
const DefaultDatabaseURL = "postgres://dev:dev@127.0.0.1:5432/postgres?sslmode=disable"

// ClusterMigrationLockKey mirrors postgres.clusterMigrationLockKey (pinned
// equal by postgres/pgtestenv_lock_key_test.go). Migrate holds it exclusively
// on the cluster's `postgres` database for its whole run, and so does Clean,
// which makes cleanup mutually exclusive with every concurrent Migrate.
const ClusterMigrationLockKey int64 = 2573143714

// RunLockKey is the advisory lock (on the cluster's `postgres` database) that
// every ledger test process holds in SHARED mode from its first PostgreSQL
// test until it exits (HoldRunLock). Clean takes it EXCLUSIVELY: it succeeds
// only when no ledger test process -- of any run, any worktree, any machine
// pointed at this server -- is alive, and only then removes anything.
// Arbitrary fixed value; must not collide with ClusterMigrationLockKey.
const RunLockKey int64 = 2573143715

// RoleGraphTestLockKey is the advisory lock (on the cluster's `postgres`
// database) that a test holds EXCLUSIVELY while it creates throwaway
// migration-credential roles and asserts on the cluster-wide ledger_owner
// membership graph (HoldRoleGraphTestLock). That graph is shared by every
// test process on the server: without the lock, one process's runner role,
// granted ledger_owner mid-run, shows up in another process's "membership
// unchanged" assertion. Arbitrary fixed value; distinct from the keys above.
const RoleGraphTestLockKey int64 = 2573143716

// HoldRoleGraphTestLock takes RoleGraphTestLockKey exclusively on its own
// connection, polling (try + wait) until ctx ends, and returns the function
// that releases it. Try + poll, not the blocking form, for the same reason
// as HoldRunLock.
func HoldRoleGraphTestLock(ctx context.Context, base string) (release func(), err error) {
	maint, err := MaintenanceURL(base)
	if err != nil {
		return nil, err
	}
	conn, err := pgx.Connect(ctx, maint)
	if err != nil {
		return nil, fmt.Errorf("pgtestenv: connect for role-graph test lock: %w", err)
	}
	release = func() { _ = conn.Close(context.WithoutCancel(ctx)) }
	for {
		var got bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", RoleGraphTestLockKey).Scan(&got); err != nil {
			release()
			return nil, fmt.Errorf("pgtestenv: take role-graph test lock: %w", err)
		}
		if got {
			return release, nil
		}
		select {
		case <-ctx.Done():
			release()
			return nil, fmt.Errorf("pgtestenv: role-graph test lock still held by another test: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// SharedRolePasswordLockKey is the transaction-scoped advisory lock, taken
// on the cluster's `postgres` database (advisory locks are per database), held
// around ALTER ROLE ... PASSWORD on the shared roles so concurrent test
// processes never update the same pg_authid row at once
// (postgrestest.SetSharedRolePassword, scripts/dev-db.sh).
const SharedRolePasswordLockKey int64 = 2573143717

// ProcessLockNamespace is the first key of the two-key advisory lock
// (ProcessLockNamespace, pid) that every ledger test process holds
// EXCLUSIVELY (on the cluster's `postgres` database) for as long as it runs
// (HoldProcessLock). Holding it marks the process's own databases
// ledger_test_<pid>_* as alive; a lock nobody holds means the process that
// created ledger_test_<pid>_0 has exited, so ReclaimOrphanTemplates may drop
// it. Two-key locks live in a different key space from the bigint locks
// above, so this cannot collide with them. Arbitrary fixed value.
const ProcessLockNamespace int32 = 25731437

// TemplateDatabaseName is the migrated template database of test process
// pid (n = 0, which DatabaseName's per-process counter never issues).
func TemplateDatabaseName(pid int) string {
	return DatabaseName(pid, 0)
}

// templateDatabaseName matches TemplateDatabaseName's output only.
var templateDatabaseName = regexp.MustCompile(`^ledger_test_([0-9]+)_0$`)

// HoldProcessLock takes (ProcessLockNamespace, pid) exclusively on conn, a
// connection to the cluster's `postgres` database that the caller keeps open
// for the rest of the process. It polls (try + wait) because a concurrent
// ReclaimOrphanTemplates holds the lock for a moment while it checks pid; it
// fails when ctx ends first, which means another LIVE test process on this
// server has the same pid (another machine or container pointed at it) --
// the template name would be shared, so the run must not continue.
func HoldProcessLock(ctx context.Context, conn *pgx.Conn, pid int) error {
	for {
		var got bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1::int4, $2::int4)", ProcessLockNamespace, int32(pid)).Scan(&got); err != nil {
			return fmt.Errorf("pgtestenv: take process lock: %w", err)
		}
		if got {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("pgtestenv: process lock for pid %d is held by another live test process on this server "+
				"(same pid on another machine or container), so this process's database names would collide: %w", pid, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// ReclaimOrphanTemplates drops the template databases (ledger_test_<pid>_0)
// of test processes that have exited: those whose process lock it can take.
// It never touches ownPID's template, nor any other name. Only templates are
// reclaimed here: per-test databases are dropped by their own t.Cleanup, and
// a ledger test process from before process locks existed holds none, so
// reclaiming its per-test databases by this rule would drop live ones (it
// never creates a template, so its databases never match).
func ReclaimOrphanTemplates(ctx context.Context, base string, ownPID int) error {
	maint, err := MaintenanceURL(base)
	if err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, maint)
	if err != nil {
		return fmt.Errorf("pgtestenv: connect for template reclaim: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))

	dbs, err := names(ctx, conn, `SELECT datname FROM pg_database WHERE datname LIKE 'ledger\_test\_%\_0' ORDER BY 1`)
	if err != nil {
		return err
	}
	for _, db := range dbs {
		m := templateDatabaseName.FindStringSubmatch(db)
		if m == nil {
			continue
		}
		var pid int64
		if _, err := fmt.Sscan(m[1], &pid); err != nil || pid == int64(ownPID) || pid > int64(^uint32(0)>>1) {
			continue
		}
		var got bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1::int4, $2::int4)", ProcessLockNamespace, int32(pid)).Scan(&got); err != nil {
			return fmt.Errorf("pgtestenv: try process lock %d: %w", pid, err)
		}
		if !got {
			continue // its process is alive
		}
		_, dropErr := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{db}.Sanitize()+" WITH (FORCE)")
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1::int4, $2::int4)", ProcessLockNamespace, int32(pid)); err != nil {
			return fmt.Errorf("pgtestenv: release process lock %d: %w", pid, err)
		}
		if dropErr != nil {
			return fmt.Errorf("pgtestenv: drop orphan template %s: %w", db, dropErr)
		}
	}
	return nil
}

// DefaultRoleNames are the cluster-wide roles the production migrations
// create. Ordinary tests share them (the migrations create them idempotently,
// exactly as a second ledger database on a production cluster would).
var DefaultRoleNames = []string{"ledger_owner", "ledger_app", "ledger_ro"}

// testDatabaseName matches only the names DatabaseName generates:
// ledger_test_<pid>_<counter>. A wt-assigned `ledger_test_<hex>` name or any
// other project's database never matches.
var testDatabaseName = regexp.MustCompile(`^ledger_test_[0-9]+_[0-9]+$`)

// testRoleName matches only roles the test suite itself creates: the private
// roundtrip roles ledger_test_<pid>_<n>_{owner,app,ro}, and the throwaway
// login roles ledger_noadmin_<nanos> / ledger_bootstrap_<nanos> that the
// migration-credential tests create and drop (left only by a killed run).
var testRoleName = regexp.MustCompile(`^(ledger_test_[0-9]+_[0-9]+_(owner|app|ro)|ledger_noadmin_[0-9]+|ledger_bootstrap_[0-9]+)$`)

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
// the same machine.
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

// HoldRunLock takes RunLockKey in SHARED mode on a dedicated connection and
// returns that connection, which the caller keeps open for the rest of the
// process: the lock is released when the process exits and the server drops
// the session. Shared holders never block one another; the only exclusive
// holder is Clean, for the few statements of a cleanup, so the poll ends
// quickly. try + poll instead of the blocking form because the repository
// pins that it contains no blocking session-level advisory lock
// (postgres/advisory_lock_shape_pin_test.go).
func HoldRunLock(ctx context.Context, base string) (*pgx.Conn, error) {
	maint, err := MaintenanceURL(base)
	if err != nil {
		return nil, err
	}
	conn, err := pgx.Connect(ctx, maint)
	if err != nil {
		return nil, fmt.Errorf("pgtestenv: connect for run lock: %w", err)
	}
	for {
		var got bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock_shared($1)", RunLockKey).Scan(&got); err != nil {
			_ = conn.Close(context.WithoutCancel(ctx))
			return nil, fmt.Errorf("pgtestenv: take run lock: %w", err)
		}
		if got {
			return conn, nil
		}
		select {
		case <-ctx.Done():
			_ = conn.Close(context.WithoutCancel(ctx))
			return nil, fmt.Errorf("pgtestenv: run lock held exclusively (a cleanup is running) for too long: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Clean removes what ledger test runs left on the server at base, but only
// when no ledger test process is running against it: it takes the cluster
// migration lock and RunLockKey, both EXCLUSIVELY and without waiting, and
// does nothing if either is busy (an active run's own cleanup finishes the
// job). Holding both, it drops
//
//   - test databases ledger_test_<pid>_<n> and the suite's own test roles
//     (ledger_test_<pid>_<n>_{owner,app,ro}, ledger_noadmin_<n>,
//     ledger_bootstrap_<n>) -- normally none, since every test drops its own;
//     these are leftovers of a killed run;
//   - the shared roles ledger_owner / ledger_app / ledger_ro, but only when
//     no database on the cluster depends on them (DROP ROLE refuses on any
//     pg_shdepend entry, in any database), so another project that installed
//     the ledger into its own database keeps them.
//
// No other name is ever touched.
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

	// errs collects per-object failures and progress-log write failures; a
	// failed write is reported, never silently dropped, and never stops the
	// cleanup itself.
	var errs []error
	logf := func(format string, args ...any) {
		if _, err := fmt.Fprintf(log, format, args...); err != nil {
			errs = append(errs, fmt.Errorf("pgtestenv: write log: %w", err))
		}
	}

	for _, lock := range []struct {
		key  int64
		busy string
	}{
		{ClusterMigrationLockKey, "a Migrate is running on this server"},
		{RunLockKey, "another ledger test process is running on this server"},
	} {
		var locked bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", lock.key).Scan(&locked); err != nil {
			return fmt.Errorf("pgtestenv: try lock %d: %w", lock.key, err)
		}
		if !locked {
			logf("pgtestclean: skipped, %s; its run cleans up (or run `make test-clean-db` later)\n", lock.busy)
			return errors.Join(errs...)
		}
		// Released when conn closes (deferred above) at the latest.
		defer func(key int64) {
			_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", key)
		}(lock.key)
	}

	dbs, err := names(ctx, conn, `SELECT datname FROM pg_database WHERE datname LIKE 'ledger\_test\_%' ORDER BY 1`)
	if err != nil {
		return err
	}
	for _, db := range dbs {
		if !testDatabaseName.MatchString(db) {
			continue
		}
		if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{db}.Sanitize()+" WITH (FORCE)"); err != nil {
			errs = append(errs, fmt.Errorf("drop leftover database %s: %w", db, err))
			continue
		}
		logf("pgtestclean: dropped leftover database %s\n", db)
	}

	roles, err := names(ctx, conn, `SELECT rolname FROM pg_roles WHERE rolname LIKE 'ledger\_%' ORDER BY 1`)
	if err != nil {
		return err
	}
	for _, role := range roles {
		if !testRoleName.MatchString(role) {
			continue
		}
		if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize()); err != nil {
			errs = append(errs, fmt.Errorf("drop leftover role %s: %w", role, err))
			continue
		}
		logf("pgtestclean: dropped leftover role %s\n", role)
	}

	for _, role := range DefaultRoleNames {
		var exists, depended bool
		if err := conn.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1),
			       EXISTS (SELECT 1 FROM pg_shdepend d JOIN pg_roles r ON r.oid = d.refobjid
			               WHERE d.refclassid = 'pg_authid'::regclass AND r.rolname = $1)
		`, role).Scan(&exists, &depended); err != nil {
			return fmt.Errorf("pgtestenv: inspect role %s: %w", role, err)
		}
		if !exists {
			continue
		}
		if depended {
			logf("pgtestclean: kept %s (objects in some database depend on it)\n", role)
			continue
		}
		if _, err := conn.Exec(ctx, "DROP ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
			// DROP ROLE refuses on any dependency the query above missed --
			// the safe outcome. Report it and keep going.
			logf("pgtestclean: kept %s (%v)\n", role, err)
			continue
		}
		logf("pgtestclean: dropped %s\n", role)
	}
	return errors.Join(errs...)
}

func names(ctx context.Context, conn *pgx.Conn, query string) ([]string, error) {
	rows, err := conn.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("pgtestenv: list: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
