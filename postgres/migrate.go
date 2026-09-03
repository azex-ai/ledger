package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/azex-ai/ledger/core"
)

// clusterMigrationLockKey is the pg_advisory_lock key acquireClusterLock
// holds for the duration of every Migrate() call. Value is
// crc32(azex-ai/ledger:cluster-migration-lock) — arbitrary, just fixed and
// documented so a collision is a deliberate choice, not an accident. See
// acquireClusterLock and docs/INVARIANTS.md I-47 for why this exists.
const clusterMigrationLockKey = 2573143714

// clusterLockBudget is the default total time acquireClusterLock spends
// waiting for the cluster migration lock before giving up. Five minutes is
// long enough for another node's full migration run and short enough that a
// leaked lock surfaces as a failed deploy rather than a process that never
// finishes booting.
const clusterLockBudget = 5 * time.Minute

// clusterLockPollInterval is how often acquireClusterLock retries, and
// therefore how often it logs that it is still waiting.
const clusterLockPollInterval = 2 * time.Second

// migrateConfig holds the knobs MigrateContext exposes. Zero value means
// "the documented defaults"; nothing here reads the environment.
type migrateConfig struct {
	logger     core.Logger
	lockBudget time.Duration
}

// MigrateOption configures MigrateContext.
type MigrateOption func(*migrateConfig)

// WithMigrateLogger routes Migrate's own diagnostics (currently: waiting for
// the cluster migration lock) through the consumer's core.Logger. Without it
// they go to slog.Default(), the same fallback EventStore.warn uses -- never
// nowhere. A startup path that can block on a cluster-wide lock must say so:
// the failure this replaces was a Migrate() that hung silently and forever
// (concurrency.md 2026-09-02 B-m4).
func WithMigrateLogger(l core.Logger) MigrateOption {
	return func(c *migrateConfig) {
		if l != nil {
			c.logger = l
		}
	}
}

// WithMigrateLockBudget overrides how long Migrate waits for the cluster
// migration lock before returning an error. Non-positive values are ignored.
func WithMigrateLockBudget(d time.Duration) MigrateOption {
	return func(c *migrateConfig) {
		if d > 0 {
			c.lockBudget = d
		}
	}
}

func newMigrateConfig(opts []MigrateOption) migrateConfig {
	cfg := migrateConfig{lockBudget: clusterLockBudget}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

func (c migrateConfig) info(msg string, args ...any) {
	if c.logger != nil {
		c.logger.Info(msg, args...)
		return
	}
	slog.Info(msg, args...)
}

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
// source for them; docs/RUNBOOK.md's "Database roles" section carries the
// operator-facing version.
//
//  1. CREATE ROLE, and CREATE on schema `public` -- 001_baseline creates the
//     three roles and every object. A superuser has this; so does the
//     database-owning, CREATEROLE account managed Postgres hands out.
//  2. CONNECT on the cluster's `postgres` maintenance database -- where the
//     cross-database migration lock lives. See acquireClusterLock.
//  3. The ability to ACT AS ledger_owner: **either** superuser, **or**
//     ledger_owner itself, **or** a role that can `SET ROLE ledger_owner`
//     (which the credential that installed 001 can always arrange -- Postgres
//     gives a role's creator a permanent ADMIN OPTION on it). Everything after
//     001 alters objects 001 transferred to ledger_owner, so migrations 002..N
//     run on a connection that has switched to that role. A credential with
//     none of the three is refused here with a message naming them -- before
//     any migration runs, rather than at whichever later statement happens to
//     need the authority first.
//
// # Why the identity is per-connection
//
// The window is opened on the one connection that runs the migrations, not on
// the credential that opens it. Role membership (pg_auth_members) is a
// cluster-wide shared catalog and Postgres's ownership checks consult
// has_privs_of_role() per statement, without regard for which session is
// asking: an earlier version of this code took `GRANT ledger_owner TO
// <runner> WITH INHERIT TRUE` for the span of each migration, which made
// EVERY session holding that credential owner-equivalent for the duration --
// in a single-credential deployment, the application's own pool. Measured on
// postgres:17.10: a second connection on the migration credential dropped
// `journal_entries_no_update` mid-run and Migrate still returned nil, so I-22
// did not hold while a deploy was in flight (M-5,
// docs/audits/2026-09-02-deep-audit/w3-review/money-path.md). Pinned by
// TestMigrate_WindowIsNotVisibleToOtherSessionsOfTheSameCredential.
//
// `SET ROLE` needs a membership carrying the SET option, and 001 deliberately
// leaves the runner without one: its closing `REVOKE ledger_owner FROM
// <runner>` removes the whole row CREATE ROLE's `createrole_self_grant='set'`
// created, and only the creator's permanent ADMIN OPTION (a separate row,
// granted by the bootstrap superuser) survives -- measured, and the reason
// this cannot simply issue SET ROLE and be done. So when the runner cannot
// switch roles yet, this grants itself the narrowest membership that lets it
// (`WITH SET TRUE, INHERIT FALSE`) and revokes it again before returning.
// That membership confers nothing on a session that does not deliberately
// switch roles, and it is nothing the credential could not grant itself at
// any other moment via that same ADMIN OPTION -- which 001's header already
// calls out as this install's one unclosable residual capability.
//
// A returned error does not imply "nothing was applied". The one case where
// both are true at once is a failure to take that membership back: the schema
// is then up to date AND the migration credential is left holding it, which is
// reported rather than logged because nothing else in the deployment can
// notice it. The message says which.
func Migrate(databaseURL string, opts ...MigrateOption) error {
	return MigrateContext(context.Background(), databaseURL, opts...)
}

// MigrateContext is Migrate with a caller-supplied context, so a boot
// sequence that is being torn down can stop waiting for the cluster
// migration lock instead of hanging until the lock is released. Migrate
// forwards context.Background() (expand step per deployment.md: the existing
// signature is unchanged, the ctx-aware sibling is additive).
func MigrateContext(ctx context.Context, databaseURL string, opts ...MigrateOption) error {
	cfg := newMigrateConfig(opts)
	databaseURL = toMigrateURL(databaseURL)

	if err := waitForDatabase(databaseURL, 10*time.Second); err != nil {
		return fmt.Errorf("postgres: migrate: wait for database: %w", err)
	}

	unlock, err := acquireClusterLock(ctx, databaseURL, cfg)
	if err != nil {
		return fmt.Errorf("postgres: migrate: acquire cluster lock: %w", err)
	}
	defer unlock()

	// 001_baseline is the only migration that can run on the bootstrap
	// credential's own authority: it creates every object it touches, so it
	// owns every object it touches. Its last act is to transfer all of them to
	// ledger_owner and then hand back the membership that made the transfer
	// possible -- and from that point the bootstrap credential passes
	// Postgres's ownership check for none of them. Everything after 001 that
	// GRANTs, ALTERs or REPLACEs a 001-created object needs ledger_owner's
	// authority.
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
	if err := applyBaseline(databaseURL); err != nil {
		return err
	}
	return applyRemainingMigrations(databaseURL)
}

// applyBaseline runs 001_baseline, and only 001_baseline, on the credential in
// databaseURL -- the one migration that needs that credential's own authority
// (CREATE ROLE) and the one migration that can run without ledger_owner's.
func applyBaseline(databaseURL string) error {
	src, err := iofs.New(migrations, "sql/migrations")
	if err != nil {
		return fmt.Errorf("postgres: migrate: init source: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, databaseURL)
	if err != nil {
		return fmt.Errorf("postgres: migrate: init migrate: %w", err)
	}
	// Close errors on a completed migration are non-actionable (errcheck excludes Close).
	defer m.Close()

	return migrateBaselineFirst(m)
}

// applyRemainingMigrations applies 002..N on a connection that IS
// ledger_owner, and closes that connection before returning.
//
// The identity is established on ONE session -- the one golang-migrate runs
// every statement on -- rather than by making the migration credential
// owner-equivalent everywhere it is used. See Migrate's "Why the identity is
// per-connection".
//
// golang-migrate opens its own connection when handed a URL, which is why the
// *sql.DB is built here and passed in through WithInstance instead: it is the
// only way to say "run these on THIS session". The pgx driver takes a single
// *sql.Conn out of it and uses that for the migration statements and for both
// schema_migrations writes, so pinning the pool to one connection is what
// makes "the session that switched roles" and "the session that migrates" the
// same sentence.
func applyRemainingMigrations(databaseURL string) (err error) {
	connCfg, parseErr := pgx.ParseConfig(strings.Replace(databaseURL, "pgx5://", "postgres://", 1))
	if parseErr != nil {
		// Not wrapped: pgx echoes the DSN, password and all, and a malformed
		// DATABASE_URL must not spill its credentials into a log line.
		return fmt.Errorf("postgres: migrate: parse database url: malformed")
	}

	setRole, granted, err := prepareLedgerOwnerIdentity(databaseURL)
	if err != nil {
		return err
	}
	if granted != "" {
		// errors.Join, not "return the first one": a migration failure and a
		// failure to give the membership back are independent facts about
		// different things, and the second one is the one nobody else can see.
		defer func() { err = errors.Join(err, revokeLedgerOwner(databaseURL, granted)) }()
	}

	var opts []stdlib.OptionOpenDB
	if setRole {
		opts = append(opts, stdlib.OptionAfterConnect(func(ctx context.Context, conn *pgx.Conn) error {
			if _, err := conn.Exec(ctx, "SET ROLE ledger_owner"); err != nil {
				return fmt.Errorf("postgres: migrate: set role ledger_owner: %w", err)
			}
			return nil
		}))
	}

	db := stdlib.OpenDB(*connCfg, opts...)
	// One connection, never recycled: SET ROLE lives in the session, so a pool
	// that quietly opened a second one would run the next migration as the
	// runner again -- and the failure would be a permission error somewhere in
	// the middle of the chain, not here. AfterConnect covers that case too;
	// this makes it unreachable rather than merely handled.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	driver, err := migratepgx.WithInstance(db, &migratepgx.Config{DatabaseName: connCfg.Database})
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("postgres: migrate: open migration connection: %w", err)
	}

	src, err := iofs.New(migrations, "sql/migrations")
	if err != nil {
		_ = driver.Close()
		return fmt.Errorf("postgres: migrate: init source: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, connCfg.Database, driver)
	if err != nil {
		_ = driver.Close()
		return fmt.Errorf("postgres: migrate: init migrate: %w", err)
	}
	// Closes the driver, its connection and the *sql.DB above. Close errors on
	// a completed migration are non-actionable (errcheck excludes Close).
	defer m.Close()

	if upErr := m.Up(); upErr != nil && !errors.Is(upErr, migrate.ErrNoChange) {
		return fmt.Errorf("postgres: migrate: up: %w", upErr)
	}
	return nil
}

// prepareLedgerOwnerIdentity works out how the migration connection is going
// to be ledger_owner, and does the least the cluster requires for that.
//
// setRole is true when the connection must issue `SET ROLE ledger_owner`;
// granted names the credential a SET-only membership had to be created for,
// and is empty when nothing was changed -- which is the case for a superuser,
// for ledger_owner itself, and for any credential an operator has already made
// a member with the SET option.
//
// The middle step asks the question as a capability ("can this connection
// switch to that role?") rather than as a predicate over pg_auth_members. More
// than one catalogue shape permits it -- an operator's explicit grant, an
// inherited membership, a createrole self-grant -- and SET ROLE succeeding is
// the only thing any of them are wanted for.
//
// Failing here is deliberately fatal rather than "try anyway and see". A
// credential that cannot act as ledger_owner cannot run any migration after
// 001, so continuing only converts one actionable error into a dirty database
// and a 42501 from whichever statement happens to be first.
func prepareLedgerOwnerIdentity(databaseURL string) (setRole bool, granted string, err error) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, strings.Replace(databaseURL, "pgx5://", "postgres://", 1))
	if err != nil {
		return false, "", fmt.Errorf("postgres: migrate: owner identity: connect: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	var runner string
	var alreadyHas bool
	if err := conn.QueryRow(ctx, `
		SELECT current_user,
		       EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ledger_owner')
		         AND pg_has_role(current_user, 'ledger_owner', 'USAGE')
	`).Scan(&runner, &alreadyHas); err != nil {
		return false, "", fmt.Errorf("postgres: migrate: owner identity: probe role: %w", err)
	}
	if alreadyHas {
		// A superuser, or ledger_owner itself. Deliberately left alone rather
		// than switched to ledger_owner anyway: a superuser install has run
		// 002..N as a superuser since this schema existed, and 007's role
		// hardening is one statement that needs more than ledger_owner has.
		return false, "", nil
	}

	// A failed statement outside a transaction does not poison the session, so
	// this can be tried and recovered from on the same connection.
	if _, roleErr := conn.Exec(ctx, "SET ROLE ledger_owner"); roleErr == nil {
		return true, "", nil
	}

	if _, grantErr := conn.Exec(ctx, fmt.Sprintf(
		"GRANT ledger_owner TO %s WITH SET TRUE, INHERIT FALSE", pgx.Identifier{runner}.Sanitize()),
	); grantErr != nil {
		return false, "", fmt.Errorf("postgres: migrate: %q cannot act as ledger_owner, and every migration after 001_baseline needs to "+
			"(001 transfers every object it creates to ledger_owner, so later GRANT/ALTER/REPLACE statements fail the ownership check without it). "+
			"Run migrations as a superuser, as ledger_owner itself, or as a role that can SET ROLE ledger_owner -- the credential that installed 001 "+
			"holds ADMIN OPTION on it permanently and can grant itself exactly that: %w", runner, grantErr)
	}

	return true, runner, nil
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

// revokeLedgerOwner takes back the SET-only membership
// prepareLedgerOwnerIdentity created, and is called only when it created one.
//
// Returns an error rather than swallowing one, which is a correction to this
// code's first shape. That version argued the failure was harmless because the
// credential holds ADMIN OPTION on ledger_owner permanently anyway (001's
// header), so a lost REVOKE "leaves it with something it can retake at will
// rather than with something new". True, and beside the point: retaking it is
// a deliberate act somebody performs, while this leaves the membership
// standing with nobody aware of it. The whole argument for granting it inside
// Migrate at all is that it is bounded and explicit -- a silently unbounded
// one is the thing this mechanism was introduced to avoid, and
// working-agreements.md §3's test ("if this step had never run, would anything
// I can see be different?") answered no.
//
// So the operator is told, and told what to do. The cost is a Migrate that can
// return an error after applying every migration successfully; Migrate's own
// doc comment says so, and the alternative is a migration credential that
// quietly keeps a standing route to ledger_owner for as long as it exists.
func revokeLedgerOwner(databaseURL, runner string) error {
	const remedy = "the migration credential %q is still a member of ledger_owner with the SET option this run gave it. " +
		"Revoke it by hand (REVOKE ledger_owner FROM %q) -- until then any session on that credential can SET ROLE ledger_owner and from " +
		"there ALTER, DROP and TRUNCATE every object in the schema, which is the standing authority 001_baseline asks operators not to " +
		"leave lying around: %w"

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
//
// Non-blocking, polled, bounded, and logged (concurrency.md 2026-09-02
// B-m4). This used to be a bare blocking pg_advisory_lock on
// context.Background(): when the holder's process was SIGKILLed and its TCP
// connection went half-open, Postgres would not reclaim the session until
// tcp_keepalives_idle, and in the meantime EVERY Migrate() call against the
// cluster — including other projects sharing a local dev server, which is
// exactly what I-47 is designed to serialize — blocked here forever with no
// log line, no timeout and no way to cancel. Failing loudly after a budget
// is strictly better than a boot that never returns, and the per-attempt
// Info line names what is being waited on.
//
// Using the try_ variant rather than a lock_timeout also keeps the claim
// AcquireBalanceLock's residual-risk note in queries/journals.sql makes
// about the single-key advisory space true: there is no blocking
// session-level advisory lock anywhere in this repository, so nothing here
// can participate in a wait-for cycle. Pinned by
// TestNoBlockingSessionAdvisoryLocks.
func acquireClusterLock(ctx context.Context, databaseURL string, cfg migrateConfig) (unlock func(), err error) {
	lockURL, err := maintenanceDatabaseURL(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("derive maintenance database url: %w", err)
	}

	conn, err := pgx.Connect(ctx, lockURL)
	if err != nil {
		return nil, fmt.Errorf("connect to maintenance database: %w", err)
	}
	release := func() { _ = conn.Close(context.WithoutCancel(ctx)) }

	deadline := time.Now().Add(cfg.lockBudget)
	for attempt := 1; ; attempt++ {
		var got bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", int64(clusterMigrationLockKey)).Scan(&got); err != nil {
			release()
			return nil, fmt.Errorf("pg_try_advisory_lock: %w", err)
		}
		if got {
			if attempt > 1 {
				cfg.info("postgres: migrate: acquired cluster migration lock", "attempts", attempt, "key", clusterMigrationLockKey)
			}
			return release, nil
		}
		if time.Now().After(deadline) {
			release()
			return nil, fmt.Errorf(
				"cluster migration lock (advisory key %d on the cluster's postgres database) still held after %s and %d attempts: "+
					"another Migrate is running against this cluster, or a previous Migrate's connection has not been reclaimed "+
					"(check pg_locks where locktype='advisory' and objid=%d on the postgres database)",
				clusterMigrationLockKey, cfg.lockBudget, attempt, clusterMigrationLockKey,
			)
		}
		cfg.info("postgres: migrate: waiting for cluster migration lock",
			"attempt", attempt,
			"key", clusterMigrationLockKey,
			"budget", cfg.lockBudget.String(),
		)
		select {
		case <-ctx.Done():
			release()
			return nil, fmt.Errorf("waiting for cluster migration lock: %w", ctx.Err())
		case <-time.After(clusterLockPollInterval):
		}
	}
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
