package postgres_test

// Pin tests for concurrency.md's advisory-lock-namespace Major and
// ExecuteTemplateBatch's cross-journal lock-order Major (D-lock, board §2).
// These drive the real unexported locking primitives (acquireBalanceLocks,
// acquireIdempotencyLock, sortedUniquePairs -- reached via export_test.go's
// test-only shim, since internal/postgrestest itself imports postgres and a
// package-postgres internal test file importing postgrestest back would be
// an import cycle) against two genuine concurrent Postgres transactions, so
// a real SQLSTATE 40P01 is what these tests observe -- not a hand-built
// pgconn.PgError. Falsifying any of these fixes (reverting journals.sql's
// namespace separation, or reverting ExecuteTemplateBatch's batch-wide
// pre-lock) reproduces the deadlock these tests assert is now impossible;
// deleting the normalizeStoreError wiring in
// acquireBalanceLocks/acquireIdempotencyLock (bus #24) turns
// TestAcquireBalanceLocks_RealDeadlock_WrapsErrTransient red on its
// core.ErrTransient assertion even though the deadlock itself would still
// occur (working-agreements §3/§4).

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// TestAcquireBalanceLocks_RealDeadlock_WrapsErrTransient constructs a
// genuine Postgres advisory-lock ABBA deadlock between two real transactions
// by calling acquireBalanceLocks out of global order -- exactly the shape a
// caller that does NOT pre-sort a whole batch's pairs (the pre-fix
// ExecuteTemplateBatch behavior; see
// TestExecuteTemplateBatch_GlobalLockOrder_PreventsCrossJournalDeadlock
// below for the fixed comparison) would produce. Asserts the losing side's
// error is classified as core.ErrTransient by normalizeStoreError (bus #24),
// not merely retryable-by-default.
func TestAcquireBalanceLocks_RealDeadlock_WrapsErrTransient(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	pairA := postgres.NewBalancePair(701, 1)
	pairB := postgres.NewBalancePair(702, 1)

	txA, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = txA.Rollback(ctx) }()
	txB, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = txB.Rollback(ctx) }()

	qA := postgres.NewQueriesForTest(txA)
	qB := postgres.NewQueriesForTest(txB)

	// Each side takes its own first pair uncontested (sequential, no race),
	// so both transactions are guaranteed to hold exactly one lock each
	// before either attempts its second -- then the second acquisition is
	// raced in goroutines, which is where the real ABBA happens.
	require.NoError(t, postgres.AcquireBalanceLocksForTest(ctx, qA, []postgres.BalancePair{pairA}))
	require.NoError(t, postgres.AcquireBalanceLocksForTest(ctx, qB, []postgres.BalancePair{pairB}))

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs[0] = postgres.AcquireBalanceLocksForTest(ctx, qA, []postgres.BalancePair{pairB})
	}()
	go func() {
		defer wg.Done()
		errs[1] = postgres.AcquireBalanceLocksForTest(ctx, qB, []postgres.BalancePair{pairA})
	}()
	wg.Wait()

	nilCount, deadlockErr := 0, error(nil)
	for _, e := range errs {
		if e == nil {
			nilCount++
			continue
		}
		deadlockErr = e
	}
	require.Equal(t, 1, nilCount, "exactly one side should have proceeded once Postgres aborted the other")
	require.NotNil(t, deadlockErr, "exactly one side should have been the deadlock victim")

	require.True(t, errors.Is(deadlockErr, core.ErrTransient),
		"deadlock error must be classified as core.ErrTransient by normalizeStoreError, got: %v", deadlockErr)
	require.True(t, core.IsRetryable(deadlockErr))
}

// TestAcquireIdempotencyLock_NeverCollidesWithBalanceLock pins the exact
// scenario concurrency.md describes: a caller-controlled idempotency_key
// crafted to look like a balance-lock key ("balance:<holder>:<currency_id>")
// used to construct an ABBA deadlock against a real balance-locking
// transaction. Before the namespace fix in journals.sql (both
// AcquireBalanceLock and AcquireIdempotencyLock hashed through
// pg_advisory_xact_lock(hashtextextended(key, 0)), a single shared 64-bit
// space), this reliably deadlocked: Tx A's idempotency lock on
// "balance:2:1" IS Tx B's balance lock on (2,1), and vice versa. After the
// fix (two-key form, disjoint namespaces 1 vs 2), neither call can block the
// other, so both sides must complete cleanly regardless of scheduling --
// this test needs no interleaving control to be deterministic, because a
// working fix removes the possibility of contention entirely.
func TestAcquireIdempotencyLock_NeverCollidesWithBalanceLock(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	// Tx A's journal: idempotency_key "balance:2:1", entries touch (1,1).
	// Tx B's journal: idempotency_key "balance:1:1", entries touch (2,1).
	// Mirrors concurrency.md's exact minimal repro.
	txA, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = txA.Rollback(ctx) }()
	txB, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = txB.Rollback(ctx) }()

	qA := postgres.NewQueriesForTest(txA)
	qB := postgres.NewQueriesForTest(txB)

	// Each side takes its idempotency lock uncontested first (the two keys
	// differ, so this never races), THEN both race their balance lock --
	// each targeting the pair that WOULD alias the other side's held
	// idempotency lock key under the old shared namespace.
	require.NoError(t, postgres.AcquireIdempotencyLockForTest(ctx, qA, "balance:2:1"))
	require.NoError(t, postgres.AcquireIdempotencyLockForTest(ctx, qB, "balance:1:1"))

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs[0] = postgres.AcquireBalanceLocksForTest(ctx, qA, []postgres.BalancePair{postgres.NewBalancePair(1, 1)})
	}()
	go func() {
		defer wg.Done()
		errs[1] = postgres.AcquireBalanceLocksForTest(ctx, qB, []postgres.BalancePair{postgres.NewBalancePair(2, 1)})
	}()
	wg.Wait()

	require.NoError(t, errs[0], "idempotency lock namespace must never block a balance lock")
	require.NoError(t, errs[1], "idempotency lock namespace must never block a balance lock")
}

// TestExecuteTemplateBatch_GlobalLockOrder_PreventsCrossJournalDeadlock pins
// the fix for ExecuteTemplateBatch's cross-journal lock-order Major:
// pre-union every journal's balance pairs across the whole batch, sort ONCE,
// and acquire before posting any journal (see ExecuteTemplateBatch's doc
// comment in ledger_store.go). Drives the exact primitives that call site
// now uses (sortedUniquePairs + acquireBalanceLocks) with two real
// transactions modeling "two batches whose journals list the same two
// holders in opposite order" -- ordinary calling behavior for e.g. two
// batch settlements, not an adversarial input.
//
// Falsification: replacing the body of each goroutine below with the
// PRE-FIX per-journal pattern -- acquireBalanceLocks(ctx, q,
// []BalancePair{p}) called once per pair in the batch's given order,
// instead of once for the pre-sorted union -- reproduces a real deadlock
// here (this is exactly what
// TestAcquireBalanceLocks_RealDeadlock_WrapsErrTransient above already pins
// for the same two pairs in that same per-journal shape).
func TestExecuteTemplateBatch_GlobalLockOrder_PreventsCrossJournalDeadlock(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	holderA := postgres.NewBalancePair(703, 1)
	holderB := postgres.NewBalancePair(704, 1)

	// Batch A's journals are given in order [holderA, holderB]; batch B's in
	// the opposite order [holderB, holderA] -- concurrency.md's exact shape
	// (two batches whose journals touch the same two holders in reverse
	// order). Both batches touch the SAME pairs, so even with the fix they
	// genuinely contend and one must wait for the other -- the fix's
	// guarantee is that this is plain serialization (no cycle), not an
	// ABBA. Each side opens and commits its own transaction inside the
	// goroutine (not held open across the whole test) so a real, brief
	// wait resolves normally instead of blocking on a lock the test itself
	// would otherwise hold until an outer defer -- pg_advisory_xact_lock
	// only releases at COMMIT/ROLLBACK, and there is no deadlock cycle here
	// for Postgres's detector to break automatically the way the ABBA test
	// above relies on.
	runBatch := func(batch []postgres.BalancePair) error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		q := postgres.NewQueriesForTest(tx)
		// sortedUniquePairs(batchA) == sortedUniquePairs(batchB): the fix's
		// whole point is that the caller's input order stops mattering.
		if err := postgres.AcquireBalanceLocksForTest(ctx, q, postgres.SortedUniquePairsForTest(batch)); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs[0] = runBatch([]postgres.BalancePair{holderA, holderB})
	}()
	go func() {
		defer wg.Done()
		errs[1] = runBatch([]postgres.BalancePair{holderB, holderA})
	}()
	wg.Wait()

	require.NoError(t, errs[0], "global batch lock order must eliminate cross-journal ABBA regardless of scheduling")
	require.NoError(t, errs[1], "global batch lock order must eliminate cross-journal ABBA regardless of scheduling")
}
