package postgres

// Test-only exports (idiomatic Go export_test.go pattern) so the external
// postgres_test package can drive the unexported advisory-lock primitives
// directly against real concurrent transactions. postgres_test needs
// internal/postgrestest for a real *pgxpool.Pool, and internal/postgrestest
// itself imports postgres -- an internal (package postgres) test file that
// also imported postgrestest would be a genuine import cycle, so these
// tests must live in postgres_test and reach the unexported symbols through
// this shim instead. See lock_order_test.go.

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

// BalancePair is balancePair, exported for tests only.
type BalancePair = balancePair

// NewBalancePair constructs a BalancePair for tests only.
func NewBalancePair(holder, currencyID int64) BalancePair {
	return balancePair{holder: holder, currencyID: currencyID}
}

// AcquireBalanceLocksForTest is acquireBalanceLocks, exported for tests only.
var AcquireBalanceLocksForTest = acquireBalanceLocks

// AcquireIdempotencyLockForTest is acquireIdempotencyLock, exported for
// tests only.
var AcquireIdempotencyLockForTest = acquireIdempotencyLock

// SortedUniquePairsForTest is sortedUniquePairs, exported for tests only.
var SortedUniquePairsForTest = sortedUniquePairs

// NewQueriesForTest is sqlcgen.New, re-exported so tests do not need their
// own import of postgres/sqlcgen just to build a *sqlcgen.Queries bound to a
// pgx.Tx for the two primitives above.
var NewQueriesForTest = sqlcgen.New

// AcquireClusterLockForTest is acquireClusterLock, exported for tests only.
//
// Role names, role attributes and role membership are cluster-wide catalogs,
// not per-database ones, so postgrestest's database-level isolation does not
// reach them: a test that has to put ledger_app into an elevated state to
// prove a migration takes it back away is mutating a row every other package's
// test binary can see, and every Migrate() call can rewrite. That is precisely
// what this lock already serializes -- Migrate holds it for its whole run --
// so a test that holds it too becomes mutually exclusive with them rather than
// racing them. See roles_test.go's TestRoleAttributeHardening... and I-47.
func AcquireClusterLockForTest(databaseURL string) (func(), error) {
	return acquireClusterLock(context.Background(), databaseURL, newMigrateConfig(nil), newMigrateRun())
}

// PrepareLedgerOwnerIdentityForTest is prepareLedgerOwnerIdentity, exported
// for tests only.
//
// What it returns is invisible from the outside once a run has finished --
// Migrate revokes what it granted -- so the test that pins "the narrowest
// membership, and no more" has to look at the halfway state. Driving the
// function directly is the only seam for that which does not involve breaking
// a real migration file.
// Each call gets its own migrateRun (the set of backend pids belonging to one
// Migrate run -- see assertSoleSessionOnCredential): driving one phase of a
// run in isolation is one run as far as the session guard is concerned.
func PrepareLedgerOwnerIdentityForTest(databaseURL string) (setRole bool, granted string, err error) {
	return prepareLedgerOwnerIdentity(databaseURL, newMigrateRun())
}

// RevokeLedgerOwnerForTest is revokeLedgerOwner, exported for tests only.
func RevokeLedgerOwnerForTest(databaseURL, runner string) error {
	return revokeLedgerOwner(databaseURL, runner, newMigrateRun())
}

// --- P6 (W3 adversarial review of the gates): the two mechanisms behind the
// idempotent-replay label, each reachable on its own ---
//
// TestPostJournal_IdempotentReplayNeverInsertsUnsignedRow asserted the
// OUTCOME of a replay (no new row, stored status still `signed`, Authorize
// reports `signed`). The reviewer deleted `replay: true` from attestJournal
// and then neutered postJournalWithQueries' fail-closed refusal, and the pin
// -- and the whole postgres package -- stayed green: every one of those
// outcomes already held BEFORE the m-6 fix, because the locked recheck
// short-circuits first. The pin was bound to a property of one caller, which
// is the exact thing m-6 was about.
//
// These two shims expose the mechanisms themselves, since both are
// unexported and postgrestest cannot be imported from inside package
// postgres (see this file's header note on the import cycle).

// AttestJournalReplayVerdictForTest runs attestJournal and reports whether it
// flagged the input as an already-posted replay, and with what status.
func (s *LedgerStore) AttestJournalReplayVerdictForTest(ctx context.Context, input core.JournalInput) (replay bool, status core.AuthStatus, err error) {
	auth, err := s.attestJournal(ctx, input, resolveEffectiveAt(input.EffectiveAt))
	if err != nil {
		return false, "", err
	}
	return auth.replay, auth.status, nil
}

// PostJournalWithReplayFlaggedAuthForTest drives postJournalWithQueries with
// a replay-flagged journalAuth for an input whose idempotency key has NOT
// been posted -- the state that can only arise if a future caller flags a
// replay without the locked recheck finding the journal. The insert path
// must refuse it rather than write an unsigned row under a key that is
// supposed to resolve to a signed one. Rolls back either way, so the caller
// can assert on the row count.
func (s *LedgerStore) PostJournalWithReplayFlaggedAuthForTest(ctx context.Context, input core.JournalInput, status core.AuthStatus) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, err = s.postJournalWithQueries(ctx, s.q.WithTx(tx), input, resolveEffectiveAt(input.EffectiveAt),
		journalAuth{replay: true, status: status})
	if err != nil {
		return err
	}
	// The refusal is the contract; if the insert went through, commit it so
	// the caller's row-count assertion sees what a real caller would have
	// written.
	return tx.Commit(ctx)
}

// AcquireClusterLockSharedForTest takes the cluster migration lock in SHARED
// mode: many holders at once, but mutually exclusive with the EXCLUSIVE
// holders (postgres.Migrate itself, and the two tests that elevate a
// cluster-wide role attribute through AcquireClusterLockForTest).
//
// m-3 (W3 adversarial review of the gates): TestRoleAttributeHardening...
// sets `ALTER ROLE ledger_app SUPERUSER` to prove migration 021 takes it
// back. pg_authid is cluster-wide and postgrestest isolates by DATABASE, so
// during that window every OTHER test asserting a permission-denied against
// ledger_app can see a superuser and fail. The exclusive lock it holds makes
// it mutually exclusive with concurrent Migrates -- which is what its comment
// claims -- but nothing made it mutually exclusive with those ACL assertions,
// and six files make them. The comment said "Both are fixed"; one was.
//
// Shared rather than exclusive so the ACL tests still run concurrently with
// each other -- they only need to exclude the elevation window, not one
// another. try_ + poll rather than the blocking variant, because
// TestNoBlockingSessionAdvisoryLocks pins that this repository contains no
// blocking session-level advisory lock (the property AcquireBalanceLock's
// residual-risk note depends on).
func AcquireClusterLockSharedForTest(databaseURL string) (func(), error) {
	ctx := context.Background()
	lockURL, err := maintenanceDatabaseURL(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("derive maintenance database url: %w", err)
	}
	conn, err := pgx.Connect(ctx, lockURL)
	if err != nil {
		return nil, fmt.Errorf("connect to maintenance database: %w", err)
	}
	release := func() { _ = conn.Close(context.WithoutCancel(ctx)) }

	cfg := newMigrateConfig(nil)
	deadline := time.Now().Add(cfg.lockBudget)
	for {
		var got bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock_shared($1)", int64(clusterMigrationLockKey)).Scan(&got); err != nil {
			release()
			return nil, fmt.Errorf("pg_try_advisory_lock_shared: %w", err)
		}
		if got {
			return release, nil
		}
		if time.Now().After(deadline) {
			release()
			return nil, fmt.Errorf(
				"cluster migration lock (advisory key %d) held EXCLUSIVELY for longer than %s: a Migrate, or a test elevating a cluster-wide role attribute, has not finished",
				clusterMigrationLockKey, cfg.lockBudget)
		}
		time.Sleep(clusterLockPollInterval)
	}
}
