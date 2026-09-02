package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"os"
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
//
// # What the credential in databaseURL must be able to do
//
// Three prerequisites, all of them install-time only. This comment is the
// source for them. docs/RUNBOOK.md's "Database roles" section carries the
// operator-facing version and, as of 2026-09-03, is behind on the third and
// still says "every migration after 001 runs as ledger_owner and needs no
// elevated privilege" -- a mechanism that has never existed (D-M2 / D-M7,
// docs/audits/2026-09-02-deep-audit/TODO.md; that file belongs to another
// worker in this remediation wave).
//
//  1. CREATE ROLE, and CREATE on schema `public` -- 001_baseline creates the
//     three roles and every object. A superuser has this; so does the
//     database-owning, CREATEROLE account managed Postgres hands out.
//  2. CONNECT on the cluster's `postgres` maintenance database -- where the
//     cross-database migration lock lives. See acquireClusterLock.
//  3. **Either** superuser, **or** ledger_owner itself, **or** ADMIN OPTION on
//     ledger_owner. Everything after 001 alters objects 001 transferred to
//     ledger_owner, so this call takes that role's privileges for the span of
//     migrations 002..N and gives them back before returning (see
//     withLedgerOwner). The credential that installed 001 always satisfies
//     this: Postgres gives a role's creator a permanent ADMIN OPTION on it.
//     A third-party role that did not create ledger_owner does not, and is
//     refused here with a message naming the three ways out -- before any
//     migration runs, rather than at whichever later statement happens to
//     need the authority first.
//
// A returned error does not imply "nothing was applied". The one case where
// both are true at once is a failure to hand ledger_owner's privileges back:
// the schema is then up to date AND the migration credential is left holding
// them, which is reported rather than logged because nothing else in the
// deployment can notice it. The message says which.
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

	// 001_baseline is the only migration that can run on the bootstrap
	// credential's own authority: it creates every object it touches, so it
	// owns every object it touches. Its last act is to transfer all of them to
	// ledger_owner -- and from that point the bootstrap credential, which
	// holds SET but not INHERIT on that role, no longer passes Postgres's
	// ownership check for any of them. Everything after 001 that GRANTs,
	// ALTERs or REPLACEs a 001-created object needs ledger_owner's authority.
	//
	// A superuser has it implicitly, which is why this was never noticed: a
	// non-superuser install died at 002's `GRANT DELETE ON public.
	// webhook_nonces TO ledger_app` with SQLSTATE 42501 (reproduced on
	// postgres:17.10 with a CREATEROLE, non-superuser role owning the target
	// database), golang-migrate marked the database dirty at 002, and 003
	// onward never ran. docs/RUNBOOK.md sanctions exactly that credential
	// ("superuser, or a role with the CREATEROLE attribute") and states that
	// "every migration after 001 runs as ledger_owner" -- a description of a
	// mechanism that did not exist. This is that mechanism.
	if err := migrateBaselineFirst(m); err != nil {
		return err
	}
	return applyRemainingMigrations(databaseURL, m)
}

// applyRemainingMigrations applies 002..N one migration at a time, each inside
// its own ledger_owner window, and reports the first failure.
//
// One window per migration rather than one for the whole run. The reason is
// measured, not defensive: 018 opens the same window itself -- 001's "Keepsake
// 2 of 2" idiom, `GRANT ledger_owner TO <runner> WITH INHERIT TRUE` at the top
// and `REVOKE ledger_owner FROM <runner>` at the bottom -- and its REVOKE
// takes ours with it. It has to: the runner is the only role that can issue
// either grant, so both carry the same grantor, and Postgres has exactly one
// row of them to revoke.
//
// Under a single run-wide window that left 019, 020 and 021 running
// unprivileged. Measured on postgres:17.10 as a CREATEROLE, non-superuser
// bootstrap: 020 died at `CREATE TRIGGER ... ON public.account_policies` with
// "permission denied for table account_policies", golang-migrate marked the
// database dirty at 20, and 021 never ran -- the same D-M2 shape the window
// was introduced to fix, moved eighteen migrations along, and invisible to
// every other test because they all install as a superuser (which takes the
// no-op branch of the elevation and is unaffected by anybody's GRANT).
//
// Re-taking the membership before each migration makes that coupling
// impossible in both directions: what a migration does to its own membership
// cannot outlive that migration, and no migration has to know this mechanism
// exists. It also narrows the window from "the whole install" to "one
// migration", which is the direction 001's header asks this credential to move
// in. A REVOKE of a membership a migration already revoked is a Postgres
// WARNING, not an error (verified), so the window's own release stays honest.
func applyRemainingMigrations(databaseURL string, m *migrate.Migrate) error {
	for {
		err := withLedgerOwner(databaseURL, func() error { return m.Steps(1) })

		// The three ways golang-migrate says "there was nothing left to do",
		// all of which mean this install is complete: ErrNoChange, os.ErrNotExist
		// (readUp's answer when the limit was reached with nothing applied) and
		// ErrShortLimit (fewer migrations available than asked for).
		var short migrate.ErrShortLimit
		switch {
		case err == nil:
			continue
		case errors.Is(err, migrate.ErrNoChange), errors.Is(err, os.ErrNotExist), errors.As(err, &short):
			return nil
		default:
			return fmt.Errorf("postgres: migrate: up: %w", err)
		}
	}
}

// withLedgerOwner runs fn with the migration credential holding ledger_owner's
// privileges, and takes them back away before returning -- on every exit path
// fn can take, including an error and including a panic.
//
// The elevated span is a function argument rather than a `defer` in Migrate so
// that "the membership is released even when the thing inside fails" is a
// property one function owns and a test can drive directly. The failure that
// matters is not the happy path: it is the migration that raises halfway
// through, which is exactly when the release is easiest to lose and hardest to
// notice, because the error everyone reads is the migration's.
//
// Both halves are no-ops when the credential already has those privileges --
// which covers a superuser, and covers connecting as ledger_owner itself.
func withLedgerOwner(databaseURL string, fn func() error) (err error) {
	runner, elevated, err := elevateToLedgerOwner(databaseURL)
	if err != nil {
		return err
	}
	if !elevated {
		return fn()
	}

	// errors.Join, not "return the first one": a migration failure and a
	// failure to give the privileges back are independent facts about
	// different things, and the second one is the one nobody else can see.
	defer func() {
		err = errors.Join(err, revokeLedgerOwner(databaseURL, runner))
	}()
	return fn()
}

// migrateBaselineFirst applies 001_baseline alone when nothing has been
// applied yet, so that `ledger_owner` exists before elevateToLedgerOwner runs.
//
// Guarded on ErrNilVersion rather than "version < 1": migrate.Migrate(1) on a
// database already past 1 migrates it DOWN to 1, which on this schema means
// running every down file from the current version backwards. Calling it
// unconditionally would turn `Migrate` into a destructive operation for every
// caller that is merely up to date.
func migrateBaselineFirst(m *migrate.Migrate) error {
	_, _, err := m.Version()
	switch {
	case errors.Is(err, migrate.ErrNilVersion):
		if err := m.Migrate(1); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return fmt.Errorf("postgres: migrate: baseline: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("postgres: migrate: read version: %w", err)
	default:
		// Already at or past 001, so ledger_owner exists.
		return nil
	}
}

// elevateToLedgerOwner gives the migration credential ledger_owner's
// privileges, and reports which credential it was and whether anything was
// actually granted. elevated is false when the credential already had them --
// which covers a superuser, and covers connecting as ledger_owner itself --
// and in that case there is nothing for revokeLedgerOwner to undo.
//
// The grant is not a privilege the credential did not already command. Postgres
// gives the creator of a role a permanent ADMIN OPTION on it that a
// non-superuser cannot strip from itself, and 001_baseline's header says so in
// as many words: the bootstrap credential "can always repeat the GRANT/REVOKE
// dance above to regain ledger_owner's privileges", which is why 001 calls that
// credential install-time-only and tells operators to rotate or retire it. What
// this does is make the window explicit and bounded -- held for one Migrate
// call, released on every exit path -- instead of leaving the operator to
// discover they need it from a permission error two migrations in.
//
// WITH INHERIT TRUE, not SET ROLE: ownership checks consult
// has_privs_of_role(), which follows inheritance and ignores SET-only
// membership, and golang-migrate opens its own connection for the migration
// statements, so a SET ROLE issued on any connection this function could reach
// would not apply to them anyway.
//
// Failing here is deliberately fatal rather than "try anyway and see". A
// credential that cannot take ledger_owner's privileges also cannot run any
// migration after 001, so continuing only converts one actionable error into a
// dirty database and a 42501 from whichever statement happens to be first.
func elevateToLedgerOwner(databaseURL string) (runner string, elevated bool, err error) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, strings.Replace(databaseURL, "pgx5://", "postgres://", 1))
	if err != nil {
		return "", false, fmt.Errorf("postgres: migrate: elevate: connect: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	var alreadyHas bool
	if err := conn.QueryRow(ctx, `
		SELECT current_user,
		       EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ledger_owner')
		         AND pg_has_role(current_user, 'ledger_owner', 'USAGE')
	`).Scan(&runner, &alreadyHas); err != nil {
		return "", false, fmt.Errorf("postgres: migrate: elevate: probe role: %w", err)
	}
	if alreadyHas {
		return runner, false, nil
	}

	if _, err := conn.Exec(ctx, fmt.Sprintf("GRANT ledger_owner TO %s WITH INHERIT TRUE", pgx.Identifier{runner}.Sanitize())); err != nil {
		return "", false, fmt.Errorf("postgres: migrate: elevate: %q needs ledger_owner's privileges to run any migration after 001_baseline "+
			"(001 transfers every object it creates to ledger_owner, so later GRANT/ALTER/REPLACE statements fail the ownership check without them). "+
			"Run migrations as a superuser, as a role holding ADMIN OPTION on ledger_owner (the credential that installed 001 always does), "+
			"or as ledger_owner itself: %w", runner, err)
	}

	return runner, true, nil
}

// revokeLedgerOwner ends the window elevateToLedgerOwner opened.
//
// Returns an error rather than swallowing one, which is a correction to this
// code's first shape. That version argued the failure was harmless because the
// credential holds ADMIN OPTION on ledger_owner permanently anyway (001's
// header), so a lost REVOKE "leaves it with something it can retake at will
// rather than with something new". True, and beside the point: retaking it is
// a deliberate act somebody performs, while this leaves the privilege standing
// with nobody aware of it. The whole argument for elevating inside Migrate at
// all is that the window is bounded and explicit -- a silently unbounded
// window is the thing this mechanism was introduced to avoid, and
// working-agreements.md §3's test ("if this step had never run, would anything
// I can see be different?") answered no.
//
// So the operator is told, and told what to do. The cost is a Migrate that can
// return an error after applying every migration successfully; Migrate's own
// doc comment says so, and the alternative is a migration credential that
// quietly stays owner-equivalent for as long as it exists.
func revokeLedgerOwner(databaseURL, runner string) error {
	const remedy = "the migration credential %q is still a member of ledger_owner and still inherits its privileges. " +
		"Revoke it by hand (REVOKE ledger_owner FROM %q) -- until then that credential can ALTER, DROP and TRUNCATE " +
		"every object in the schema, which is the standing authority 001_baseline asks operators not to leave lying around: %w"

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, strings.Replace(databaseURL, "pgx5://", "postgres://", 1))
	if err != nil {
		return fmt.Errorf("postgres: migrate: revoke: connect: "+remedy, runner, runner, err)
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, fmt.Sprintf("REVOKE ledger_owner FROM %s", pgx.Identifier{runner}.Sanitize())); err != nil {
		return fmt.Errorf("postgres: migrate: revoke: "+remedy, runner, runner, err)
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
