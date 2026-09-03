package postgres

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
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

// migrationAppName is the application_name every connection Migrate opens
// reports. It is a LABEL -- for an operator reading pg_stat_activity, and for
// the list of foreign sessions the guard's refusal prints -- and deliberately
// not an identity: application_name is a value any client sets for itself,
// so a session that wanted to be mistaken for one of ours only had to say so
// (2026-09-03 independent review, install-roles.md M2, measured: one
// `SET application_name` and a refused run became MIGRATE OK). What a
// connection of ours actually IS, is recorded in migrateRun.
const migrationAppName = "azex-ledger-migrate"

// migrateRun is the identity of one Migrate call: the backend pids of every
// connection it has opened, on any database of the cluster.
//
// A pid is assigned by the server and cannot be claimed by a client, which is
// the whole point -- it is the property assertSoleSessionOnCredential
// excludes ITSELF by, having previously excluded itself by application_name.
//
// Recording every pid of the run, rather than only the probing connection's,
// is what makes this safe where `pid <> pg_backend_pid()` alone would not be:
// Migrate opens several connections across a run (the cluster lock on the
// maintenance database, the readiness probe, 001's, the identity probe,
// 002..N's, the revoke), and a backend that has just been asked to close
// still appears in pg_stat_activity for the moment it takes to exit. Those
// are ours whether they are still open or not, so they are excluded by
// identity instead of by timing. A random per-run application_name suffix
// would have solved the self-report half only: a session holding the same
// credential can read pg_stat_activity for rows of its own role, so it could
// copy the nonce as easily as the constant.
type migrateRun struct {
	mu   sync.Mutex
	pids map[uint32]struct{}
}

func newMigrateRun() *migrateRun {
	return &migrateRun{pids: map[uint32]struct{}{}}
}

// note records a connection as belonging to this run. Nil-safe so a caller
// that has no run to attribute a connection to (there is none today) cannot
// panic; a nil run simply excludes nothing, which is fail-CLOSED for the
// guard (it would count our own sessions and refuse).
func (r *migrateRun) note(conn *pgx.Conn) {
	if r == nil || conn == nil || conn.PgConn() == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pids[conn.PgConn().PID()] = struct{}{}
}

// ownPIDs returns the recorded pids as int4s, the type pg_stat_activity.pid
// is compared against.
func (r *migrateRun) ownPIDs() []int32 {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int32, 0, len(r.pids))
	for pid := range r.pids {
		out = append(out, int32(pid))
	}
	return out
}

// afterConnectNoting returns an OptionOpenDB that records every connection
// the returned *sql.DB opens, and then runs extra (if non-nil).
//
// One option, not two: stdlib.OptionAfterConnect OVERWRITES the hook rather
// than chaining, so passing it twice would silently drop the first -- and the
// first is the one that keeps Migrate's own connections out of the session
// guard's count.
func afterConnectNoting(run *migrateRun, extra func(context.Context, *pgx.Conn) error) stdlib.OptionOpenDB {
	return stdlib.OptionAfterConnect(func(ctx context.Context, conn *pgx.Conn) error {
		run.note(conn)
		if extra != nil {
			return extra(ctx, conn)
		}
		return nil
	})
}

// migrationConnConfig parses databaseURL for pgx and stamps it with
// migrationAppName. Every connection this file opens goes through it.
func migrationConnConfig(databaseURL string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(strings.Replace(databaseURL, "pgx5://", "postgres://", 1))
	if err != nil {
		// Not wrapped: pgx's parse error echoes the DSN, password and all, and
		// a malformed DATABASE_URL must not spill its credentials into a log.
		return nil, fmt.Errorf("parse database url: malformed")
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["application_name"] = migrationAppName
	return cfg, nil
}

// connectForMigration opens a single pgx connection carrying migrationAppName
// and records its backend pid against run (see migrateRun).
func connectForMigration(ctx context.Context, databaseURL string, run *migrateRun) (*pgx.Conn, error) {
	cfg, err := migrationConnConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	run.note(conn)
	return conn, nil
}

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
// And one thing it must NOT be doing: for anything other than a superuser or
// ledger_owner itself, no other session may be connected as this credential
// while Migrate runs. See "The single-credential deployment is refused" below.
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
// That arrangement is made BEFORE 001 too, and not only before 002..N: on a
// cluster whose ledger roles already exist (a second ledger database on one
// server), 001 never gets the self-grant CREATE ROLE would have given it and
// its own closing ownership sweep cannot run -- see
// prepareBaselineOwnerMembership, and install-roles.md M1 for what that used
// to look like from the outside.
// That membership confers nothing on a session that does not deliberately
// switch roles, and it is nothing the credential could not grant itself at
// any other moment via that same ADMIN OPTION -- which 001's header already
// calls out as this install's one unclosable residual capability.
//
// # The single-credential deployment is refused, not tolerated
//
// A session that holds this credential can still switch to ledger_owner
// deliberately, and an application with a SQL injection is a session that does
// what it is told. That is not a configuration this library can make safe, so
// it is one it declines to migrate on: before arranging anything, and again
// after the membership exists, Migrate counts the other sessions connected as
// this credential (pg_stat_activity, excluding its own connections by backend
// pid) and returns an error naming the count and the remedy if there are any.
// A superuser or ledger_owner credential arranges nothing and is not subject
// to the check. See assertSoleSessionOnCredential for why the exclusion key
// is a pid and not application_name (2026-09-03, install-roles.md M2: a name
// the audited session sets for itself is not an identity); pinned by
// TestMigrate_RefusesWhileAnotherSessionHoldsTheMigrationCredential and
// TestMigrate_RefusesASessionClaimingTheMigrationApplicationName.
//
// The check binds sessions that already exist, not one that connects while the
// run is in progress: such a session can still SET ROLE ledger_owner
// deliberately for the rest of the run. Measured, pinned in
// TestMigrate_WindowIsNotVisibleToOtherSessionsOfTheSameCredential, and
// written up in docs/INVARIANTS.md I-22. What removes it is a migration
// credential the application does not hold, which is why that is stated as a
// requirement and not a preference.
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
	run := newMigrateRun()

	if err := waitForDatabase(databaseURL, 10*time.Second, run); err != nil {
		return fmt.Errorf("postgres: migrate: wait for database: %w", err)
	}

	unlock, err := acquireClusterLock(ctx, databaseURL, cfg, run)
	if err != nil {
		return fmt.Errorf("postgres: migrate: acquire cluster lock: %w", err)
	}
	defer unlock()

	return applyAllMigrations(ctx, databaseURL, run)
}

// applyAllMigrations is MigrateContext's body from the cluster lock onwards,
// split out for one reason: it needs a NAMED return so a deferred
// errors.Join can add a failed revoke to whatever the run itself returned,
// and `(err error)` on MigrateContext itself would change that function's
// printed signature -- which docs/api-surface.txt records and
// TestAPISurface_MatchesSnapshot compares. A cosmetic diff in the public API
// snapshot is not a thing to spend a BREAKING.md entry on.
func applyAllMigrations(ctx context.Context, databaseURL string, run *migrateRun) (err error) {
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
	//
	// 001 needs ledger_owner's SET membership too, on any cluster where the
	// three roles already exist -- arranged here, before a single statement
	// runs, so the alternative (dying inside 001 and leaving the database
	// dirty) is unreachable rather than merely documented. See
	// prepareBaselineOwnerMembership.
	grantedForBaseline, err := prepareBaselineOwnerMembership(ctx, databaseURL, run)
	if err != nil {
		return err
	}
	if grantedForBaseline != "" {
		// Same errors.Join reasoning as applyRemainingMigrations': a failure
		// to hand the membership back is an independent fact nobody else can
		// see. Registered here rather than around applyBaseline alone
		// because applyRemainingMigrations reuses the same membership --
		// prepareLedgerOwnerIdentity finds SET ROLE already working and
		// grants nothing of its own.
		defer func() { err = errors.Join(err, revokeLedgerOwner(databaseURL, grantedForBaseline, run)) }()
	}

	if err := applyBaseline(databaseURL, run); err != nil {
		return err
	}
	return applyRemainingMigrations(databaseURL, run)
}

// prepareBaselineOwnerMembership makes 001_baseline runnable on a cluster
// that already carries the three ledger roles -- the second and every later
// ledger database on one server, which is a documented deployment (I-47's
// cluster lock exists for it) and was, until this existed, an install that
// died halfway.
//
// What went wrong (2026-09-03 independent review, install-roles.md M1,
// reproduced twice): 001 acquires its SET membership on ledger_owner as a
// side effect of CREATING it (`SET LOCAL createrole_self_grant='set'` +
// `CREATE ROLE`). With the roles already present, `IF NOT EXISTS` skips the
// CREATE, the membership is never granted, and section 14's closing
// `ALTER TABLE ... OWNER TO ledger_owner` fails -- since PostgreSQL 16 that
// statement requires the caller to be able to SET ROLE to the new owner.
// The failure surfaced as `must be able to SET ROLE "ledger_owner"` followed
// by an echo of the entire 1600-line migration, with `schema_migrations`
// left at `(1, dirty=t)` for a human to force. Migrate's own preflight
// (prepareLedgerOwnerIdentity) had nothing to say about it because it ran
// AFTER applyBaseline.
//
// Returns the credential a SET-only membership had to be created for (empty
// when nothing was changed), which the caller must revoke when the run ends.
//
// Deliberately a no-op on a cold cluster: ledger_owner not existing means 001
// is about to create it and self-grant, which is the path every first install
// takes and the one this must not disturb. Also a no-op for a superuser or
// for any credential an operator has already made a member -- the same three
// exits prepareLedgerOwnerIdentity takes, asked one phase earlier.
func prepareBaselineOwnerMembership(ctx context.Context, databaseURL string, run *migrateRun) (granted string, err error) {
	conn, err := connectForMigration(ctx, databaseURL, run)
	if err != nil {
		return "", fmt.Errorf("postgres: migrate: baseline identity: connect: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	var runner string
	var ownerExists, canActAsOwner bool
	if err := conn.QueryRow(ctx, `
		SELECT current_user,
		       EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ledger_owner'),
		       COALESCE((SELECT pg_has_role(current_user, oid, 'USAGE') FROM pg_roles WHERE rolname = 'ledger_owner'), false)
	`).Scan(&runner, &ownerExists, &canActAsOwner); err != nil {
		return "", fmt.Errorf("postgres: migrate: baseline identity: probe role: %w", err)
	}
	if !ownerExists || canActAsOwner {
		return "", nil
	}

	// The capability, not the catalogue shape: more than one arrangement
	// permits SET ROLE, and this is the only thing any of them is wanted for.
	// A failed statement outside a transaction does not poison the session.
	if _, roleErr := conn.Exec(ctx, "SET ROLE ledger_owner"); roleErr == nil {
		if _, resetErr := conn.Exec(ctx, "RESET ROLE"); resetErr != nil {
			return "", fmt.Errorf("postgres: migrate: baseline identity: reset role: %w", resetErr)
		}
		return "", nil
	}

	// Granting a route to ledger_owner is only a bounded thing to do if this
	// credential is not simultaneously serving traffic -- the same refusal
	// applyRemainingMigrations makes, for the same reason, now also covering
	// the phase that used to run before it.
	if err := assertSoleSessionOnCredential(ctx, conn, runner, run); err != nil {
		return "", err
	}

	if _, grantErr := conn.Exec(ctx, fmt.Sprintf(
		"GRANT ledger_owner TO %s WITH SET TRUE, INHERIT FALSE", pgx.Identifier{runner}.Sanitize()),
	); grantErr != nil {
		return "", fmt.Errorf("postgres: migrate: the roles ledger_owner/ledger_app/ledger_ro already exist on this cluster "+
			"(another ledger database on the same server), and %q can neither SET ROLE to ledger_owner nor grant itself that "+
			"membership. 001_baseline transfers every object it creates to ledger_owner, and since PostgreSQL 16 that requires "+
			"the caller to be able to SET ROLE to it -- so the install would fail halfway and leave the database marked dirty. "+
			"Nothing has been applied. Fix it with ONE of: run migrations as a superuser or as ledger_owner itself; or have a "+
			"superuser (or a holder of ADMIN OPTION on ledger_owner -- the credential that installed the first ledger database "+
			"on this cluster holds it permanently) run: GRANT ledger_owner TO %s WITH SET TRUE, INHERIT FALSE: %w",
			runner, pgx.Identifier{runner}.Sanitize(), grantErr)
	}

	// And again, now that the membership exists: a pool that connected during
	// the two statements above would otherwise hold a credential that can
	// reach ledger_owner with nothing having noticed.
	if err := assertSoleSessionOnCredential(ctx, conn, runner, run); err != nil {
		return "", errors.Join(err, revokeLedgerOwner(databaseURL, runner, run))
	}

	return runner, nil
}

// applyBaseline runs 001_baseline, and only 001_baseline, on the credential in
// databaseURL -- the one migration that needs that credential's own authority
// (CREATE ROLE) and the one migration that can run without ledger_owner's.
func applyBaseline(databaseURL string, run *migrateRun) error {
	connCfg, err := migrationConnConfig(databaseURL)
	if err != nil {
		return fmt.Errorf("postgres: migrate: %w", err)
	}

	// Built here rather than handed to golang-migrate as a URL for one reason:
	// a URL makes golang-migrate open the connection, and this one has to
	// carry migrationAppName like every other connection Migrate opens, or the
	// session guard downstream would count it as somebody else's.
	db := stdlib.OpenDB(*connCfg, afterConnectNoting(run, nil))
	db.SetMaxOpenConns(1)

	m, err := newMigrateOverDB(db, connCfg.Database)
	if err != nil {
		return err
	}
	// Close errors on a completed migration are non-actionable (errcheck excludes Close).
	defer m.Close()

	return migrateBaselineFirst(m)
}

// newMigrateOverDB wires the embedded migration set to a database/sql handle
// the caller owns, so that "which connection do these statements run on" is a
// decision this package makes rather than one golang-migrate makes from a URL.
// The returned *migrate.Migrate owns db: closing it closes the driver, the
// connection the driver pinned, and db itself.
func newMigrateOverDB(db *sql.DB, databaseName string) (*migrate.Migrate, error) {
	driver, err := migratepgx.WithInstance(db, &migratepgx.Config{DatabaseName: databaseName})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres: migrate: open migration connection: %w", err)
	}

	src, err := iofs.New(migrations, "sql/migrations")
	if err != nil {
		_ = driver.Close()
		return nil, fmt.Errorf("postgres: migrate: init source: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, databaseName, driver)
	if err != nil {
		_ = driver.Close()
		return nil, fmt.Errorf("postgres: migrate: init migrate: %w", err)
	}
	return m, nil
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
func applyRemainingMigrations(databaseURL string, run *migrateRun) (err error) {
	connCfg, cfgErr := migrationConnConfig(databaseURL)
	if cfgErr != nil {
		return fmt.Errorf("postgres: migrate: %w", cfgErr)
	}

	setRole, granted, err := prepareLedgerOwnerIdentity(databaseURL, run)
	if err != nil {
		return err
	}
	if granted != "" {
		// errors.Join, not "return the first one": a migration failure and a
		// failure to give the membership back are independent facts about
		// different things, and the second one is the one nobody else can see.
		defer func() { err = errors.Join(err, revokeLedgerOwner(databaseURL, granted, run)) }()
	}

	var switchRole func(context.Context, *pgx.Conn) error
	if setRole {
		switchRole = func(ctx context.Context, conn *pgx.Conn) error {
			if _, err := conn.Exec(ctx, "SET ROLE ledger_owner"); err != nil {
				return fmt.Errorf("postgres: migrate: set role ledger_owner: %w", err)
			}
			return nil
		}
	}

	db := stdlib.OpenDB(*connCfg, afterConnectNoting(run, switchRole))
	// One connection, never recycled: SET ROLE lives in the session, so a pool
	// that quietly opened a second one would run the next migration as the
	// runner again -- and the failure would be a permission error somewhere in
	// the middle of the chain, not here. AfterConnect covers that case too;
	// this makes it unreachable rather than merely handled.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	m, err := newMigrateOverDB(db, connCfg.Database)
	if err != nil {
		return err
	}
	// Closes the driver, the connection it switched to ledger_owner, and the
	// *sql.DB above -- which is why there is no RESET ROLE here: the session
	// that held the role stops existing, which is the same guarantee and one
	// fewer statement that has to be reached to be true. Close errors on a
	// completed migration are non-actionable (errcheck excludes Close).
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
// It also refuses outright, before arranging anything, when another session
// holds this credential -- see assertSoleSessionOnCredential for why that is
// this function's business and not the operator's alone.
//
// Failing here is deliberately fatal rather than "try anyway and see". A
// credential that cannot act as ledger_owner cannot run any migration after
// 001, so continuing only converts one actionable error into a dirty database
// and a 42501 from whichever statement happens to be first.
func prepareLedgerOwnerIdentity(databaseURL string, run *migrateRun) (setRole bool, granted string, err error) {
	ctx := context.Background()
	conn, err := connectForMigration(ctx, databaseURL, run)
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

	// Everything below this line arranges for a connection to become
	// ledger_owner, which is only a bounded thing to do if this credential is
	// not simultaneously serving traffic. Checked before anything is arranged,
	// so the refusal costs nothing and changes nothing.
	if err := assertSoleSessionOnCredential(ctx, conn, runner, run); err != nil {
		return false, "", err
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

	// And again, now that the membership exists: a pool that connected during
	// the two statements above would otherwise hold a credential that can
	// reach ledger_owner, with nothing having noticed. Revoked here rather
	// than left to the caller's defer, because the caller is being told the
	// run never started.
	if err := assertSoleSessionOnCredential(ctx, conn, runner, run); err != nil {
		return false, "", errors.Join(err, revokeLedgerOwner(databaseURL, runner, run))
	}

	return true, runner, nil
}

// assertSoleSessionOnCredential refuses to elevate anything while another
// session is holding the migration credential.
//
// The membership Migrate arranges is deliberately SET-only, so no session is
// given ledger_owner's privileges by it -- but a session on this credential
// can still switch to that role deliberately, and an application with a SQL
// injection is a session that does what it is told. A single-credential
// deployment is therefore not a configuration this library can make safe; it
// is one it can refuse to migrate on, which turns a silent risk into a failed
// deploy with an instruction in it (working-agreements.md §3: fail-closed,
// not fail-open).
//
// Our own connections are excluded by BACKEND PID -- every pid this run has
// opened, not just the probing one (migrateRun). Two properties are needed at
// once and only that combination has both:
//
//   - not self-reportable. The exclusion key was application_name until
//     2026-09-03 (install-roles.md M2), and application_name is a value the
//     session being audited sets for itself: an application pool that issued
//     `SET application_name = 'azex-ledger-migrate'` was not counted, so the
//     refusal below never fired and the whole run proceeded on a credential
//     the application was holding -- measured, and precisely the deployment
//     shape this guard exists for. A per-run random suffix would not have
//     fixed it either: a session on this credential can read
//     pg_stat_activity's rows for its own role and copy whatever it finds
//     there. A pid is assigned by the server; nothing a client sends changes
//     it.
//   - stable across our own churn. Migrate opens several connections in a
//     run (cluster lock on the maintenance database, readiness probe, 001's,
//     this one, 002..N's, the revoke) and a backend asked to close still
//     appears here for the moment it takes to exit. Excluding only
//     pg_backend_pid() would count those and refuse the run for no reason,
//     which is why the original chose a name over a pid; recording the run's
//     pids removes the choice.
//
// application_name is still stamped on every connection and still printed in
// the refusal, as the label that tells an operator which pool is in the way.
// It is simply no longer trusted to say who WE are.
//
// Non-superusers see the full row for sessions of their own role (Postgres
// masks only other roles' rows), so this reads the same on the CREATEROLE
// bootstrap credential as it does on a superuser -- verified on
// postgres:17.10.
func assertSoleSessionOnCredential(ctx context.Context, conn *pgx.Conn, runner string, run *migrateRun) error {
	var others int
	var names string
	if err := conn.QueryRow(ctx, `
		SELECT count(*),
		       coalesce(string_agg(DISTINCT coalesce(nullif(application_name, ''), '(unset)'), ', '), '')
		FROM pg_stat_activity
		WHERE usename = current_user
		  AND pid <> pg_backend_pid()
		  AND pid <> ALL($1::int[])
	`, run.ownPIDs()).Scan(&others, &names); err != nil {
		return fmt.Errorf("postgres: migrate: check for other sessions on the migration credential: %w", err)
	}
	if others == 0 {
		return nil
	}
	return fmt.Errorf("postgres: migrate: refusing to run: %d other session(s) are connected as %q "+
		"(pg_stat_activity, application_name: %s). Migrations need a connection that can act as "+
		"ledger_owner -- 001_baseline's closing ownership sweep included, on a cluster where these roles already exist "+
		"-- and every session holding this credential can reach that role deliberately for as long as the "+
		"run lasts, including an application pool. Give migrations their own credential (MIGRATE_DATABASE_URL, "+
		"separate from the application's DATABASE_URL), or stop the application before migrating. A superuser or "+
		"ledger_owner connection needs no arrangement and is not subject to this check",
		others, runner, names)
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
func revokeLedgerOwner(databaseURL, runner string, run *migrateRun) error {
	const remedy = "the migration credential %q is still a member of ledger_owner with the SET option this run gave it. " +
		"Revoke it by hand (REVOKE ledger_owner FROM %q) -- until then any session on that credential can SET ROLE ledger_owner and from " +
		"there ALTER, DROP and TRUNCATE every object in the schema, which is the standing authority 001_baseline asks operators not to " +
		"leave lying around: %w"

	ctx := context.Background()
	conn, err := connectForMigration(ctx, databaseURL, run)
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
func acquireClusterLock(ctx context.Context, databaseURL string, cfg migrateConfig, run *migrateRun) (unlock func(), err error) {
	lockURL, err := maintenanceDatabaseURL(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("derive maintenance database url: %w", err)
	}

	conn, err := connectForMigration(ctx, lockURL, run)
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

func waitForDatabase(databaseURL string, timeout time.Duration, run *migrateRun) error {
	ctx := context.Background()

	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := connectForMigration(ctx, databaseURL, run)
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
