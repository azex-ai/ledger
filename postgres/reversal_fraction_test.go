package postgres_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// seedFractionFixture posts one 100.01 USDC (exponent 2) journal and returns
// everything a partial-reversal test needs. 100.01 does not divide evenly by
// 3, which is exactly the case the largest-remainder allocation must survive
// without losing a cent.
func seedFractionFixture(t *testing.T) (store *postgres.LedgerStore, ctx context.Context, journalUID, curID, clsWallet, clsCustodial string) {
	t.Helper()
	pool := postgrestest.SetupDB(t)
	store = postgres.NewLedgerStore(pool)
	ctx = context.Background()

	curID = postgrestest.SeedCurrencyWithExponent(t, pool, "USDC", "USD Coin", 2)
	jtID := postgrestest.SeedJournalType(t, pool, "transfer", "Transfer")
	clsWallet = postgrestest.SeedClassification(t, pool, "main_wallet", "Main Wallet", "credit", false)
	clsCustodial = postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "debit", true)

	amount := decimal.RequireFromString("100.01")
	j, err := store.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("frac-base"),
		Entries: []core.EntryInput{
			{AccountHolder: 7, CurrencyUID: curID, ClassificationUID: clsWallet, EntryType: core.EntryTypeCredit, Amount: amount},
			{AccountHolder: -7, CurrencyUID: curID, ClassificationUID: clsCustodial, EntryType: core.EntryTypeDebit, Amount: amount},
		},
	})
	require.NoError(t, err)
	return store, ctx, j.UID, curID, clsWallet, clsCustodial
}

// Pins I-2 (revised): fractional reversals conserve — cumulative reversed
// never exceeds the original — and the num==den remainder form completes a
// reversal exactly even when earlier fractional steps rounded up.
//
// Walkthrough at exponent 2: each 1/3 of 100.01 rounds (HalfUp) to 33.34.
// Two succeed (66.68 reversed); a third 1/3 would push the total to 100.02 >
// 100.01 and is rejected — 1/3 always means "a third of the ORIGINAL", not
// of the remainder. The exact remainder 33.33 is then only reachable via the
// 1/1 "reverse everything remaining" form.
func TestReverseJournalFraction_ConservationAndRemainderCompletion(t *testing.T) {
	store, ctx, jID, curID, clsWallet, _ := seedFractionFixture(t)

	totalReversed := decimal.Zero
	for i := 0; i < 2; i++ {
		rev, err := store.ReverseJournalFraction(ctx, jID, 1, 3, "partial refund", postgrestest.UniqueKey("frac-third"))
		require.NoError(t, err, "reversal %d/2", i+1)
		assert.Equal(t, jID, rev.ReversalOfUID)
		// Every partial reversal must itself balance per currency.
		assert.True(t, rev.TotalDebit.Equal(rev.TotalCredit), "reversal %d unbalanced: DR=%s CR=%s", i+1, rev.TotalDebit, rev.TotalCredit)
		assert.True(t, rev.TotalDebit.Equal(decimal.RequireFromString("33.34")), "each 1/3 of 100.01 rounds to 33.34, got %s", rev.TotalDebit)
		totalReversed = totalReversed.Add(rev.TotalDebit)
	}

	// A third 1/3 (another 33.34) would exceed the original — conservation
	// rejects it outright rather than silently clamping.
	_, err := store.ReverseJournalFraction(ctx, jID, 1, 3, "third third", postgrestest.UniqueKey("frac-3rd"))
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrConflict)

	// 1/1 reverses the exact remainder (33.33) and closes the journal out.
	rest, err := store.ReverseJournalFraction(ctx, jID, 1, 1, "close out", postgrestest.UniqueKey("frac-rest"))
	require.NoError(t, err)
	assert.True(t, rest.TotalDebit.Equal(decimal.RequireFromString("33.33")), "remainder must be exactly 33.33, got %s", rest.TotalDebit)
	totalReversed = totalReversed.Add(rest.TotalDebit)
	assert.True(t, totalReversed.Equal(decimal.RequireFromString("100.01")))

	// Balance is back to zero.
	bal, err := store.GetBalance(ctx, 7, curID, clsWallet)
	require.NoError(t, err)
	assert.True(t, bal.IsZero(), "expected 0 after full fractional reversal, got %s", bal)

	// Nothing left — remainder form on a fully-reversed journal is rejected.
	_, err = store.ReverseJournalFraction(ctx, jID, 1, 1, "over", postgrestest.UniqueKey("frac-over"))
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrConflict)
}

func TestReverseJournalFraction_OverReversalRejected(t *testing.T) {
	store, ctx, jID, _, _, _ := seedFractionFixture(t)

	_, err := store.ReverseJournalFraction(ctx, jID, 2, 3, "first", postgrestest.UniqueKey("frac-2-3"))
	require.NoError(t, err)

	// 2/3 already reversed; another 1/2 (> remaining 1/3) must be rejected
	// and must not partially apply.
	_, err = store.ReverseJournalFraction(ctx, jID, 1, 2, "too much", postgrestest.UniqueKey("frac-1-2"))
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrConflict)

	// The remaining 1/3 still goes through.
	_, err = store.ReverseJournalFraction(ctx, jID, 1, 3, "rest", postgrestest.UniqueKey("frac-rest"))
	require.NoError(t, err)
}

func TestReverseJournalFraction_IdempotentReplay(t *testing.T) {
	store, ctx, jID, _, _, _ := seedFractionFixture(t)

	key := postgrestest.UniqueKey("frac-replay")
	first, err := store.ReverseJournalFraction(ctx, jID, 1, 4, "refund", key)
	require.NoError(t, err)

	// Same key + same payload → the original reversal, no second posting.
	second, err := store.ReverseJournalFraction(ctx, jID, 1, 4, "refund", key)
	require.NoError(t, err)
	assert.Equal(t, first.UID, second.UID)

	// Same key + different fraction → conflict.
	_, err = store.ReverseJournalFraction(ctx, jID, 1, 2, "refund", key)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrConflict)
}

// Pins the concurrency half of I-2 (revised): two racing partial reversals
// serialize on the original journal's row lock; conservation holds no matter
// which wins.
func TestReverseJournalFraction_ConcurrentConservation(t *testing.T) {
	store, ctx, jID, _, _, _ := seedFractionFixture(t)

	const racers = 4 // 4 × 1/3 — at most two more may succeed after the first
	_, err := store.ReverseJournalFraction(ctx, jID, 1, 3, "seed", postgrestest.UniqueKey("frac-c0"))
	require.NoError(t, err)

	var wg sync.WaitGroup
	succeeded := make(chan decimal.Decimal, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			rev, err := store.ReverseJournalFraction(ctx, jID, 1, 3, "race", postgrestest.UniqueKey("frac-race"))
			if err == nil {
				succeeded <- rev.TotalDebit
			}
		}(i)
	}
	wg.Wait()
	close(succeeded)

	sum := decimal.RequireFromString("33.34") // the seeded first third
	for amt := range succeeded {
		sum = sum.Add(amt)
	}
	// Whatever subset of racers won, the cumulative total must never exceed
	// the original amount.
	assert.True(t, sum.LessThanOrEqual(decimal.RequireFromString("100.01")),
		"cumulative reversals exceed original: %s", sum)
}

func TestReverseJournalFraction_MultiCurrencyBalancesPerCurrency(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	store := postgres.NewLedgerStore(pool)
	ctx := context.Background()

	curA := postgrestest.SeedCurrencyWithExponent(t, pool, "USDC", "USD Coin", 2)
	curB := postgrestest.SeedCurrencyWithExponent(t, pool, "JPY", "Yen", 0)
	jtID := postgrestest.SeedJournalType(t, pool, "fx", "FX")
	clsWallet := postgrestest.SeedClassification(t, pool, "main_wallet", "Main Wallet", "credit", false)
	clsSettle := postgrestest.SeedClassification(t, pool, "settlement", "Settlement", "debit", true)

	j, err := store.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("frac-fx"),
		Entries: []core.EntryInput{
			{AccountHolder: 9, CurrencyUID: curA, ClassificationUID: clsWallet, EntryType: core.EntryTypeDebit, Amount: decimal.RequireFromString("10.01")},
			{AccountHolder: -9, CurrencyUID: curA, ClassificationUID: clsSettle, EntryType: core.EntryTypeCredit, Amount: decimal.RequireFromString("10.01")},
			{AccountHolder: 9, CurrencyUID: curB, ClassificationUID: clsWallet, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(1501)},
			{AccountHolder: -9, CurrencyUID: curB, ClassificationUID: clsSettle, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(1501)},
		},
	})
	require.NoError(t, err)

	rev, err := store.ReverseJournalFraction(ctx, j.UID, 1, 3, "fx partial", postgrestest.UniqueKey("frac-fx-rev"))
	require.NoError(t, err)

	// The DB's deferred per-currency balance trigger would have aborted the
	// insert if either currency skewed; reaching here with equal totals means
	// both legs balanced. JPY (exponent 0) third of 1501 must be whole.
	assert.True(t, rev.TotalDebit.Equal(rev.TotalCredit))
}

// Full-vs-partial exclusivity matrix (revised I-2 semantics).
func TestReverseJournal_MutualExclusionWithFraction(t *testing.T) {
	store, ctx, jID, _, _, _ := seedFractionFixture(t)

	// Partial first → full ReverseJournal is rejected (would double-count).
	_, err := store.ReverseJournalFraction(ctx, jID, 1, 4, "partial", postgrestest.UniqueKey("frac-mx"))
	require.NoError(t, err)
	_, err = store.ReverseJournal(ctx, jID, "full after partial")
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrConflict)

	// Reversing a reversal (full or fractional) stays blocked.
	rev, err := store.ReverseJournalFraction(ctx, jID, 1, 4, "partial2", postgrestest.UniqueKey("frac-mx2"))
	require.NoError(t, err)
	_, err = store.ReverseJournalFraction(ctx, rev.UID, 1, 2, "rev of rev", postgrestest.UniqueKey("frac-mx3"))
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrConflict)
}

// Pins the concurrency half of I-2 for FULL reversals: migration 029 dropped
// the at-most-once unique index on reversal_of, so without the row lock two
// concurrent ReverseJournal calls with different reasons (hence different
// idempotency keys) would both see "no reversal history" and together post a
// 200% reversal. Exactly one racer may win; every loser must get ErrConflict
// (or an idempotent replay of the winner's journal, never a second reversal).
func TestReverseJournal_ConcurrentFullReversals_OnlyOneWins(t *testing.T) {
	store, ctx, jID, _, _, _ := seedFractionFixture(t)

	const racers = 4
	var wg sync.WaitGroup
	reversalUIDs := make(chan string, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			rev, err := store.ReverseJournal(ctx, jID, fmt.Sprintf("race-reason-%d", n))
			if err == nil {
				reversalUIDs <- rev.UID
			} else {
				assert.ErrorIs(t, err, core.ErrConflict)
			}
		}(i)
	}
	wg.Wait()
	close(reversalUIDs)

	distinct := make(map[string]struct{})
	for uid := range reversalUIDs {
		distinct[uid] = struct{}{}
	}
	assert.LessOrEqual(t, len(distinct), 1, "more than one full reversal journal was posted: %v", distinct)
	assert.Equal(t, 1, len(distinct), "exactly one racer should have succeeded")
}
