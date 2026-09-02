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
	return acquireClusterLock(context.Background(), databaseURL, newMigrateConfig(nil))
}

// WithLedgerOwnerForTest is withLedgerOwner, exported for tests only.
//
// Migrate has no seam for "make migration 014 raise": the migration set is
// embedded, and the failure this needs to reproduce is a statement inside it
// failing halfway through the elevated window. Driving withLedgerOwner with a
// body that returns an error reproduces exactly that window and that exit
// path, without a test that has to corrupt a real migration to get there.
var WithLedgerOwnerForTest = withLedgerOwner

// RevokeLedgerOwnerForTest is revokeLedgerOwner, exported for tests only.
var RevokeLedgerOwnerForTest = revokeLedgerOwner

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
