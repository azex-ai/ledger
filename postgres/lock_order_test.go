package postgres_test

// Pin tests for the advisory-lock ordering invariants (I-11 / I-39), against
// two genuine concurrent Postgres transactions -- so a real SQLSTATE 40P01 is
// what these tests observe, never a hand-built pgconn.PgError.
//
// Two shapes live here, and the difference matters (concurrency.md
// 2026-09-02, "gate-shape"):
//
//   - MECHANISM tests drive the unexported primitives directly
//     (acquireBalanceLocks / acquireIdempotencyLock / sortedUniquePairs,
//     reached via export_test.go's shim -- internal/postgrestest imports
//     postgres, so an internal test file importing it back would be an import
//     cycle). They prove a lock namespace or a lock order behaves as claimed.
//     They do NOT pin any caller, and must not be cited as one: the batch
//     lock-order pin used to be written this way, re-implemented the fix
//     inside the test, and stayed green with the fix deleted.
//   - CALLER tests drive the real exported entry point
//     (ExecuteTemplateBatch, in both pool and tx mode; PostJournal with an
//     event link) and use a probe transaction only to create the deadlock
//     opportunity. Deleting the fix inside the store turns these red. Each
//     one names its falsification step in its doc comment.
//
// Deleting the normalizeStoreError wiring in
// acquireBalanceLocks/acquireIdempotencyLock (bus #24) turns
// TestAcquireBalanceLocks_RealDeadlock_WrapsErrTransient red on its
// core.ErrTransient assertion even though the deadlock itself would still
// occur (working-agreements §3/§4).

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	ledger "github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/presets"
)

// TestAcquireBalanceLocks_RealDeadlock_WrapsErrTransient is a MECHANISM test
// (see this file's header): it constructs a genuine Postgres advisory-lock
// ABBA deadlock between two real transactions by calling acquireBalanceLocks
// out of global order -- the shape any caller that does not take a whole
// transaction's pairs in one canonical order would produce -- and asserts the
// losing side's error is classified as core.ErrTransient by
// normalizeStoreError (bus #24), not merely retryable-by-default. It pins the
// classification, not any caller's lock order; the callers are pinned by the
// ExecuteTemplateBatch and PostJournal tests below.
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
// AcquireBalanceLock and AcquireIdempotencyLock hashed the caller-supplied
// key directly through pg_advisory_xact_lock(hashtextextended(key, 0)), a
// single shared 64-bit space with no per-namespace prefix), this reliably
// deadlocked: Tx A's idempotency lock on "balance:2:1" IS Tx B's balance
// lock on (2,1), and vice versa. After the fix (each query now hashes a
// literal 'bal:'/'idem:' prefix concatenated onto the key -- see
// AcquireBalanceLock's comment in journals.sql for why the two prefixed
// string spaces are disjoint by construction, and M-6 of the 2026-08-26
// independent review for why this replaced an intermediate two-key
// pg_advisory_xact_lock(int4, int4) form that fixed this exact bug but
// narrowed the hash to 32 bits), neither call can block the other, so both
// sides must complete cleanly regardless of scheduling -- this test needs
// no interleaving control to be deterministic, because a working fix
// removes the possibility of contention entirely.
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

// seedBatchDeadlockFixture installs the default template presets (for
// `deposit_confirm`: DR main_wallet on the user holder, CR custodial on the
// system counterpart, so one request locks both `h` and `-h`) and returns the
// pieces the two ExecuteTemplateBatch lock-order tests below need.
func seedBatchDeadlockFixture(t *testing.T, currencyCode string) (pool *pgxpool.Pool, curUID string, currencyID int64) {
	t.Helper()
	pool = postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	tmplStore := postgres.NewTemplateStore(pool)
	require.NoError(t, presets.InstallDefaultTemplatePresets(ctx, classStore, classStore, tmplStore))

	curUID = postgrestest.SeedCurrency(t, pool, currencyCode, "Batch Lock Order Unit")
	currencyID = postgrestest.InternalID(t, pool, "currencies", curUID)
	return pool, curUID, currencyID
}

// batchRequests renders one `deposit_confirm` request per holder, in the
// order given -- the caller-controlled sequence whose effect on lock order is
// exactly what is under test.
func batchRequests(curUID string, keyPrefix string, holders ...int64) []core.TemplateExecutionRequest {
	reqs := make([]core.TemplateExecutionRequest, 0, len(holders))
	for _, h := range holders {
		reqs = append(reqs, core.TemplateExecutionRequest{
			TemplateCode: "deposit_confirm",
			Params: core.TemplateParams{
				HolderID:       h,
				CurrencyUID:    curUID,
				IdempotencyKey: postgrestest.UniqueKey(fmt.Sprintf("%s-%d", keyPrefix, h)),
				Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(10)},
				Source:         "lock-order-test",
			},
		})
	}
	return reqs
}

// runBatchLockOrderProbe is the shared body of the two tests below. It builds
// a deterministic deadlock opportunity around a REAL ExecuteTemplateBatch
// call (pool mode or tx mode, supplied by the caller as execBatch) instead of
// re-implementing the fix in the test:
//
//	probe tx P : holds bal(-h2)                      [taken first, uncontested]
//	batch      : ExecuteTemplateBatch([h1, h2])
//	probe tx P : then asks for bal(h1)
//
// Pre-fix, the batch takes each journal's locks as it posts it, in the
// caller's order: bal(-h1), bal(h1) for the first request, then it blocks on
// bal(-h2) for the second -- while HOLDING bal(h1). P then asks for bal(h1)
// and the cycle closes: SQLSTATE 40P01 for whichever side Postgres picks.
// Post-fix, the batch pre-acquires the union sorted once -- bal(-h2) is the
// smallest key, so the batch blocks on its FIRST acquisition holding no
// balance lock at all, P's bal(h1) is uncontested, and no cycle can form.
//
// The sleep is what makes this deterministic rather than a race: it gives the
// batch time to reach its blocking acquisition before P asks for its second
// lock. It cannot produce a false PASS -- if the batch were still ahead of
// where the comment says it is, P would simply take an uncontested lock,
// which is the same observation the assertion makes.
func runBatchLockOrderProbe(t *testing.T, pool *pgxpool.Pool, currencyID, h1, h2 int64, execBatch func() error) {
	t.Helper()
	ctx := context.Background()

	probeTx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = probeTx.Rollback(ctx) }()
	probeQ := postgres.NewQueriesForTest(probeTx)

	require.NoError(t, postgres.AcquireBalanceLocksForTest(ctx, probeQ,
		[]postgres.BalancePair{postgres.NewBalancePair(core.SystemAccountHolder(h2), currencyID)}),
		"probe must take the system counterpart of the batch's SECOND request uncontested")

	var wg sync.WaitGroup
	var batchErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		batchErr = execBatch()
	}()

	time.Sleep(1500 * time.Millisecond)

	probeErr := postgres.AcquireBalanceLocksForTest(ctx, probeQ,
		[]postgres.BalancePair{postgres.NewBalancePair(h1, currencyID)})

	// Release the probe's locks so the batch can finish either way; the
	// assertions below are about what already happened, not about the
	// batch's ability to complete afterwards.
	_ = probeTx.Rollback(ctx)
	wg.Wait()

	require.NoError(t, probeErr,
		"the probe asked for a lock the batch must not be holding while it waits; a 40P01 here means ExecuteTemplateBatch took its balance locks in the caller's request order instead of one canonical global order")
	require.NoError(t, batchErr,
		"ExecuteTemplateBatch must serialize behind the probe, not deadlock with it")
}

// TestExecuteTemplateBatch_GlobalLockOrder_PreventsCrossJournalDeadlock pins
// the pool-mode half of ExecuteTemplateBatch's cross-journal lock-order fix:
// pre-union every journal's balance pairs across the whole batch, sort ONCE,
// and acquire before posting any journal (preacquireBatchLocks in
// ledger_store.go).
//
// This test previously asserted the same property against the PRIMITIVES
// (sortedUniquePairs + acquireBalanceLocks called by the test itself) and
// never mentioned ExecuteTemplateBatch in its body -- it re-implemented the
// fix and would have stayed green with the fix deleted (concurrency.md
// 2026-09-02, "gate-shape"). It now drives the real call.
//
// Falsification (re-run before trusting this test): delete the
// preacquireBatchLocks call from ExecuteTemplateBatch's pool branch. The
// probe then loses a real 40P01.
func TestExecuteTemplateBatch_GlobalLockOrder_PreventsCrossJournalDeadlock(t *testing.T) {
	pool, curUID, currencyID := seedBatchDeadlockFixture(t, "BLO")
	ctx := context.Background()
	store := postgres.NewLedgerStore(pool)

	const h1, h2 = int64(705), int64(706)
	runBatchLockOrderProbe(t, pool, currencyID, h1, h2, func() error {
		_, err := store.ExecuteTemplateBatch(ctx, batchRequests(curUID, "blo-pool", h1, h2))
		return err
	})
}

// TestExecuteTemplateBatch_TxMode_GlobalLockOrder_PreventsCrossJournalDeadlock
// pins the same property for the tx-mode branch a consumer reaches through
// ledger.Service.RunInTx -- the branch the original fix never touched
// (concurrency.md 2026-09-02 Major: "the batch-level global lock order was
// only added to the pool path"). Driven through the facade, not through a
// hand-bound store, because RunInTx + TemplateBatchExecutor() is the entry a
// consumer actually has.
//
// Falsification: delete the preacquireBatchLocks call from
// executeTemplateBatchWithQueries.
func TestExecuteTemplateBatch_TxMode_GlobalLockOrder_PreventsCrossJournalDeadlock(t *testing.T) {
	pool, curUID, currencyID := seedBatchDeadlockFixture(t, "BLOTX")
	ctx := context.Background()
	svc, err := ledger.New(pool)
	require.NoError(t, err)

	const h1, h2 = int64(707), int64(708)
	runBatchLockOrderProbe(t, pool, currencyID, h1, h2, func() error {
		return svc.RunInTx(ctx, func(tx *ledger.Service) error {
			_, err := tx.TemplateBatchExecutor().ExecuteTemplateBatch(ctx, batchRequests(curUID, "blo-tx", h1, h2))
			return err
		})
	})
}

// TestAcquireBalanceLocks_HashCollisionCrossBatchDeadlock_Fixed pins M-6 of
// the 2026-08-26 independent review: AcquireBalanceLock narrowed from
// pg_advisory_xact_lock(hashtextextended(key, 0)) (64-bit) to
// pg_advisory_xact_lock(1::int4, hashtext(key)) (32-bit hashtext, inside a
// namespace int4) to fix the cross-namespace ABBA
// TestAcquireIdempotencyLock_NeverCollidesWithBalanceLock pins above. That
// migration's own comment claimed "hash collisions *within* this namespace
// ... only reduce concurrency, they do not affect correctness" -- true for
// a single contended pair, false across a whole batch: the batch-level
// defense (sortedUniquePairs, exercised by
// TestExecuteTemplateBatch_GlobalLockOrder_PreventsCrossJournalDeadlock
// above) sorts each transaction's OWN pairs by (holder, currency_id), which
// prevents an ABBA between two transactions that share pairs. It does
// nothing about two transactions with entirely DISJOINT holder sets whose
// pairs alias the same 32-bit hashtext() value -- the sort order is by
// holder id, not by the hash the lock is actually taken on, so two
// unrelated pairs colliding in hash space can still interleave into a
// classic ABBA even though both sides individually did everything the
// batch-sort fix requires.
//
// The four (holder, currency_id=1) pairs below are not synthetic: they are
// real hashtext() collisions found by an offline birthday-attack search
// over "balance:<holder>:1" (see this test's construction below for the
// exact search) --
//
//	hashtext("balance:120355:1") == hashtext("balance:149900:1")   == -1814105475
//	hashtext("balance:5483:1")   == hashtext("balance:209744:1")   == -745411079
//
// Tx A's batch is {120355, 5483}; sorted ascending by holder that's
// [5483, 120355] -- A grabs 5483's lock (hash -745411079) uncontested
// first, then wants 120355's lock (hash -1814105475).
// Tx B's batch is {149900, 209744}; sorted ascending that's
// [149900, 209744] -- B grabs 149900's lock (hash -1814105475) uncontested
// first, then wants 209744's lock (hash -745411079).
// A holds -745411079 and wants -1814105475; B holds -1814105475 and wants
// -745411079 -- genuine ABBA, despite both sides using the real
// sortedUniquePairs + acquireBalanceLocks path with their own pairs
// correctly pre-sorted.
//
// Falsification: reverting journals.sql's AcquireBalanceLock to
// pg_advisory_xact_lock(1::int4, hashtext(key)) reproduces a real 40P01
// here (confirmed by running this exact test against that query before
// applying the fix this test now pins).
func TestAcquireBalanceLocks_HashCollisionCrossBatchDeadlock_Fixed(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	// Confirm the search claim above against THIS Postgres instance rather
	// than trusting the hardcoded hash values to still hold on whatever
	// version/build runs CI -- hashtext() is documented immutable, but the
	// test's premise is falsifiable, so falsify it here first instead of
	// assuming.
	var h1a, h1b, h3a, h3b int32
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	require.NoError(t, conn.QueryRow(ctx, "SELECT hashtext('balance:120355:1')").Scan(&h1a))
	require.NoError(t, conn.QueryRow(ctx, "SELECT hashtext('balance:149900:1')").Scan(&h1b))
	require.NoError(t, conn.QueryRow(ctx, "SELECT hashtext('balance:5483:1')").Scan(&h3a))
	require.NoError(t, conn.QueryRow(ctx, "SELECT hashtext('balance:209744:1')").Scan(&h3b))
	conn.Release()
	require.Equal(t, h1a, h1b, "premise: (120355,1) and (149900,1) must collide under hashtext()")
	require.Equal(t, h3a, h3b, "premise: (5483,1) and (209744,1) must collide under hashtext()")
	require.NotEqual(t, h1a, h3a, "premise: the two collision groups must land in different buckets")

	pairHolder120355 := postgres.NewBalancePair(120355, 1)
	pairHolder5483 := postgres.NewBalancePair(5483, 1)
	pairHolder149900 := postgres.NewBalancePair(149900, 1)
	pairHolder209744 := postgres.NewBalancePair(209744, 1)

	txA, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = txA.Rollback(ctx) }()
	txB, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = txB.Rollback(ctx) }()

	qA := postgres.NewQueriesForTest(txA)
	qB := postgres.NewQueriesForTest(txB)

	// Each side acquires its own uncontested first pair (via the real
	// sortedUniquePairs order) sequentially, so both are guaranteed to hold
	// one lock each before the genuinely racy second acquisition below.
	require.NoError(t, postgres.AcquireBalanceLocksForTest(ctx, qA, postgres.SortedUniquePairsForTest([]postgres.BalancePair{pairHolder5483})))
	require.NoError(t, postgres.AcquireBalanceLocksForTest(ctx, qB, postgres.SortedUniquePairsForTest([]postgres.BalancePair{pairHolder149900})))

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs[0] = postgres.AcquireBalanceLocksForTest(ctx, qA, postgres.SortedUniquePairsForTest([]postgres.BalancePair{pairHolder120355}))
	}()
	go func() {
		defer wg.Done()
		errs[1] = postgres.AcquireBalanceLocksForTest(ctx, qB, postgres.SortedUniquePairsForTest([]postgres.BalancePair{pairHolder209744}))
	}()
	wg.Wait()

	require.NoError(t, errs[0], "hash-collision cross-batch ABBA must be impossible once AcquireBalanceLock uses the full 64-bit hashtextextended range")
	require.NoError(t, errs[1], "hash-collision cross-batch ABBA must be impossible once AcquireBalanceLock uses the full 64-bit hashtextextended range")
}

// TestPostJournal_EventLink_LocksBookingBeforeBalances pins B-m6: the two
// paths that touch a booking row and a balance lock in the same transaction
// used to take them in OPPOSITE orders.
//
//	Transition -> PostJournal (the Event-Journal atomicity recipe in
//	CLAUDE.md, and what service/onchain.go's deposit confirmation does):
//	    booking row lock -> balance advisory locks
//	PostJournal with a caller-supplied event_uid (a wire field on
//	POST /journals, typically a repair job attaching a journal to an
//	event some earlier transaction created):
//	    balance advisory locks -> booking row lock  (via LinkBookingJournal)
//
// Two ordinary calls, one cycle. The probe transaction below stands in for
// the Transition side, holding the booking row lock; post-fix, PostJournal
// takes that row FOR UPDATE while resolving event_uid -- before any balance
// lock -- so it blocks there holding nothing and the probe's later bal(H) is
// uncontested.
//
// Falsification: move the GetBookingForUpdate call in postJournalWithQueries
// back after acquireBalanceLocks (or drop it and let LinkBookingJournal take
// the row lock implicitly, as it did). The probe then loses a real 40P01.
func TestPostJournal_EventLink_LocksBookingBeforeBalances(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	bookingStore := postgres.NewBookingStore(pool)
	store := postgres.NewLedgerStore(pool)

	cls, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code:       "booking_lock_order",
		Name:       "Booking Lock Order",
		NormalSide: core.NormalSideCredit,
		IsSystem:   true,
		Lifecycle: &core.Lifecycle{
			Initial:     "pending",
			Terminal:    []core.Status{"confirmed"},
			Transitions: map[core.Status][]core.Status{"pending": {"confirmed"}},
		},
	})
	require.NoError(t, err)

	curUID := postgrestest.SeedCurrency(t, pool, "BLKORD", "Booking Lock Order Unit")
	currencyID := postgrestest.InternalID(t, pool, "currencies", curUID)
	jtUID := postgrestest.SeedJournalType(t, pool, "transfer", "Transfer")
	wallet := postgrestest.SeedClassification(t, pool, "main_wallet", "Main Wallet", "debit", false)
	custodial := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)

	const holder = int64(911)

	booking, err := bookingStore.CreateBooking(ctx, core.CreateBookingInput{
		ClassificationCode: cls.Code,
		AccountHolder:      holder,
		CurrencyUID:        curUID,
		Amount:             decimal.NewFromInt(100),
		IdempotencyKey:     postgrestest.UniqueKey("blkord-booking"),
		ChannelName:        "test",
	})
	require.NoError(t, err)

	evt, err := bookingStore.Transition(ctx, core.TransitionInput{
		BookingUID:     booking.UID,
		ToStatus:       "confirmed",
		IdempotencyKey: postgrestest.UniqueKey("blkord-transition"),
	})
	require.NoError(t, err)

	// Probe = the Transition side: booking row lock first.
	probeTx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = probeTx.Rollback(ctx) }()
	_, err = probeTx.Exec(ctx, "SELECT id FROM bookings WHERE uid = $1::uuid FOR UPDATE", booking.UID)
	require.NoError(t, err)

	var wg sync.WaitGroup
	var postErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, postErr = store.PostJournal(ctx, core.JournalInput{
			JournalTypeUID: jtUID,
			IdempotencyKey: postgrestest.UniqueKey("blkord-post"),
			EventUID:       evt.UID,
			Entries: []core.EntryInput{
				{AccountHolder: holder, CurrencyUID: curUID, ClassificationUID: wallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
				{AccountHolder: -holder, CurrencyUID: curUID, ClassificationUID: custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
			},
		})
	}()

	time.Sleep(1500 * time.Millisecond)

	probeQ := postgres.NewQueriesForTest(probeTx)
	probeErr := postgres.AcquireBalanceLocksForTest(ctx, probeQ,
		[]postgres.BalancePair{postgres.NewBalancePair(holder, currencyID)})

	_ = probeTx.Rollback(ctx)
	wg.Wait()

	require.NoError(t, probeErr,
		"a 40P01 here means PostJournal held the balance locks while waiting for the booking row -- the reverse of the Transition path's order")
	require.NoError(t, postErr, "PostJournal must serialize behind the booking row lock, not deadlock with it")
}
