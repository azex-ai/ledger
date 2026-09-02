package postgres_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ledger "github.com/azex-ai/ledger"
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

// TestReverseJournalFraction_RepeatedDimensionCompletesFully pins the case the
// remainder form used to get wrong: a journal carrying more than one entry on
// the same (holder, currency, classification, entry_type).
//
// JournalInput.Validate checks per-currency balance and does not deduplicate,
// so such a journal is legal. Prior reversals are tracked per dimension, but
// the remainder was computed per entry -- each entry subtracted the whole
// dimension's prior total. On 60 + 40 reversed half then "all the rest", the
// two entries yielded 10 and -10; the negative was skipped as non-positive and
// the surviving 10/10 balanced, so Validate accepted it and the call returned
// success with 40 still on the books.
//
// That shape is why this needs pinning at the balance level. The failure was
// invisible to every structural check available: the reversal balanced, it
// conserved (10 < 60), it wrote a valid journal, and it returned nil. Only the
// holder's remaining balance shows it.
func TestReverseJournalFraction_RepeatedDimensionCompletesFully(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	store := postgres.NewLedgerStore(pool)
	ctx := context.Background()

	curID := postgrestest.SeedCurrencyWithExponent(t, pool, "RPT", "Repeat Unit", 2)
	jtID := postgrestest.SeedJournalType(t, pool, "transfer", "Transfer")
	wallet := postgrestest.SeedClassification(t, pool, "main_wallet", "Main Wallet", "debit", false)
	custodial := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)

	const holder = int64(4401)
	d := decimal.RequireFromString

	// Two debits and two credits, each pair sharing one dimension.
	j, err := store.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("repeat-dim"),
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: wallet, EntryType: core.EntryTypeDebit, Amount: d("60")},
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: wallet, EntryType: core.EntryTypeDebit, Amount: d("40")},
			{AccountHolder: -holder, CurrencyUID: curID, ClassificationUID: custodial, EntryType: core.EntryTypeCredit, Amount: d("60")},
			{AccountHolder: -holder, CurrencyUID: curID, ClassificationUID: custodial, EntryType: core.EntryTypeCredit, Amount: d("40")},
		},
	})
	require.NoError(t, err)

	balance := func() decimal.Decimal {
		t.Helper()
		b, err := store.GetBalance(ctx, holder, curID, wallet)
		require.NoError(t, err)
		return b
	}
	require.True(t, balance().Equal(d("100")), "fixture must start at 100, got %s", balance())

	_, err = store.ReverseJournalFraction(ctx, j.UID, 1, 2, "half", postgrestest.UniqueKey("rev-half"))
	require.NoError(t, err)
	require.True(t, balance().Equal(d("50")), "half of 100 must leave 50, got %s", balance())

	_, err = store.ReverseJournalFraction(ctx, j.UID, 1, 1, "rest", postgrestest.UniqueKey("rev-rest"))
	require.NoError(t, err)
	require.True(t, balance().IsZero(),
		"reversing the rest must leave nothing, got %s -- a non-zero balance here means the remainder was computed per entry against a per-dimension total again, and the caller was told the journal was fully reversed while it was not", balance())
}

// TestReverseJournalFraction_RepeatedDimensionFractionalSteps pins the
// fractional-branch half of the same repeated-dimension defect the test above
// pins for the remainder branch: the overshoot check compared the DIMENSION's
// cumulative reversed total against each individual entry's ORIGINAL amount.
// On 60 + 40 debits sharing one dimension, a legal second 1/2 reversal saw
// already=50 (dimension-wide) + 30 (this entry's share) > 60 (that one
// entry's original) and returned ErrConflict for an overshoot that does not
// exist. Fail-closed, so no money was ever lost -- but a legitimate partial
// reversal was permanently rejected with an error message pointing at a
// phantom excess.
func TestReverseJournalFraction_RepeatedDimensionFractionalSteps(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	store := postgres.NewLedgerStore(pool)
	ctx := context.Background()

	curID := postgrestest.SeedCurrencyWithExponent(t, pool, "RPF", "Repeat Fraction Unit", 2)
	jtID := postgrestest.SeedJournalType(t, pool, "transfer", "Transfer")
	wallet := postgrestest.SeedClassification(t, pool, "main_wallet", "Main Wallet", "debit", false)
	custodial := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)

	const holder = int64(4402)
	d := decimal.RequireFromString

	j, err := store.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("repeat-dim-frac"),
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: wallet, EntryType: core.EntryTypeDebit, Amount: d("60")},
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: wallet, EntryType: core.EntryTypeDebit, Amount: d("40")},
			{AccountHolder: -holder, CurrencyUID: curID, ClassificationUID: custodial, EntryType: core.EntryTypeCredit, Amount: d("60")},
			{AccountHolder: -holder, CurrencyUID: curID, ClassificationUID: custodial, EntryType: core.EntryTypeCredit, Amount: d("40")},
		},
	})
	require.NoError(t, err)

	balance := func() decimal.Decimal {
		t.Helper()
		b, err := store.GetBalance(ctx, holder, curID, wallet)
		require.NoError(t, err)
		return b
	}
	require.True(t, balance().Equal(d("100")), "fixture must start at 100, got %s", balance())

	_, err = store.ReverseJournalFraction(ctx, j.UID, 1, 2, "first half", postgrestest.UniqueKey("rev-frac-h1"))
	require.NoError(t, err)
	require.True(t, balance().Equal(d("50")), "half of 100 must leave 50, got %s", balance())

	// The second legal half. With the per-entry comparison this was rejected
	// as ErrConflict (50 dimension-wide + 30 share > 60 single-entry original).
	_, err = store.ReverseJournalFraction(ctx, j.UID, 1, 2, "second half", postgrestest.UniqueKey("rev-frac-h2"))
	require.NoError(t, err,
		"a second 1/2 reversal of a half-reversed journal is legal; ErrConflict here means the overshoot check compared a per-dimension cumulative against a single entry's original amount")
	require.True(t, balance().IsZero(), "two halves must leave nothing, got %s", balance())

	// A third half now genuinely overshoots and must still be rejected.
	_, err = store.ReverseJournalFraction(ctx, j.UID, 1, 2, "over", postgrestest.UniqueKey("rev-frac-h3"))
	require.ErrorIs(t, err, core.ErrConflict, "reversing beyond the original must stay rejected")
}

// --- A-C2 (2026-09-02 deep audit): the ReversalOfUID input gate ------------
//
// `ReverseJournal*` derive their entries themselves and are guarded on both
// ends (the referenced journal may not itself be a reversal; the cumulative
// per-dimension amount may not exceed the original). `PostJournal` accepts
// the same `reversal_of` link from a caller and, before this gate existed,
// checked only that the uid resolved to a row. Everything downstream --
// `cumulativeReversedByDimension` above all -- reads every journal carrying
// `reversal_of = J` as "a reversal of J worth this much", so an unvalidated
// link is enough to make the ledger believe J has already been reversed by
// an amount that never left any account.
//
// These three tests drive the facade entry a library consumer actually uses
// (`ledger.New(pool).JournalWriter()`), because that -- not the HTTP surface,
// which never accepted the field -- is where the field is reachable.

// TestPostJournal_ReversalOfUID_RejectsNonReversingEntries is the audit's
// minimal repro, asserted at the balance rather than at any internal shape.
// A perfectly legal, per-currency-balanced, NET-ZERO journal tagged
// `reversal_of = J` moves no money at all, yet its four legs register as 50
// already reversed on two of J's dimensions (the credit legs invert back to
// the debit key and vice versa). `ReverseJournalFraction(J, 1, 1)` -- "reverse
// everything remaining" -- then reverses 100 - 50 = 50, returns nil, and
// leaves 50 on the books while telling the caller the journal is fully
// reversed. Every reconciliation check stays green throughout, because the
// books really do balance; what is broken is the reversal chain, not the
// double entry.
func TestPostJournal_ReversalOfUID_RejectsNonReversingEntries(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	svc, err := ledger.New(pool)
	require.NoError(t, err)
	writer := svc.JournalWriter()
	ctx := context.Background()

	curID := postgrestest.SeedCurrencyWithExponent(t, pool, "ACT", "Audit C2 Unit", 2)
	jtID := postgrestest.SeedJournalType(t, pool, "transfer", "Transfer")
	wallet := postgrestest.SeedClassification(t, pool, "main_wallet", "Main Wallet", "debit", false)
	custodial := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)

	const holder = int64(4501)
	d := decimal.RequireFromString

	balance := func() decimal.Decimal {
		t.Helper()
		b, err := svc.BalanceReader().GetBalance(ctx, holder, curID, wallet)
		require.NoError(t, err)
		return b
	}

	j, err := writer.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("ac2-base"),
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: wallet, EntryType: core.EntryTypeDebit, Amount: d("100")},
			{AccountHolder: -holder, CurrencyUID: curID, ClassificationUID: custodial, EntryType: core.EntryTypeCredit, Amount: d("100")},
		},
	})
	require.NoError(t, err)
	require.True(t, balance().Equal(d("100")), "fixture must start at 100, got %s", balance())

	// Net zero on every dimension, balanced per currency, and every leg is a
	// dimension J really touches -- only the DIRECTIONS make it not a
	// reversal. Two of the four legs (the ones matching J's own direction)
	// have no counterpart in J once inverted, which is what makes this a
	// non-subset rather than a small over-reversal.
	_, err = writer.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("ac2-forged"),
		ReversalOfUID:  j.UID,
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: wallet, EntryType: core.EntryTypeCredit, Amount: d("50")},
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: wallet, EntryType: core.EntryTypeDebit, Amount: d("50")},
			{AccountHolder: -holder, CurrencyUID: curID, ClassificationUID: custodial, EntryType: core.EntryTypeDebit, Amount: d("50")},
			{AccountHolder: -holder, CurrencyUID: curID, ClassificationUID: custodial, EntryType: core.EntryTypeCredit, Amount: d("50")},
		},
	})
	require.Error(t, err, "a net-zero journal is not a reversal of anything; tagging it reversal_of must be refused at the input gate")
	require.ErrorIs(t, err, core.ErrInvalidInput)

	// The gate must reject WITHOUT side effects, and the reversal chain must
	// still be intact afterwards: reversing everything remaining has to bring
	// the holder to exactly zero.
	require.True(t, balance().Equal(d("100")), "a rejected post must leave the balance untouched, got %s", balance())

	_, err = writer.ReverseJournalFraction(ctx, j.UID, 1, 1, "close out", postgrestest.UniqueKey("ac2-rev-all"))
	require.NoError(t, err)
	require.True(t, balance().IsZero(),
		"after reversing everything remaining the balance must be 0, got %s -- a non-zero balance means a journal that moved no money was counted as reversal history", balance())
}

// TestPostJournal_ReversalOfUID_RejectsReversalOfAReversal pins the guard
// `ReverseJournal` (ledger_store.go) and `ReverseJournalFraction`
// (reversal_fraction_store.go) have always had and `PostJournal` did not:
// a reversal is a leaf of the chain. Without it, `reversal_of` chains can be
// arbitrarily long and `cumulativeReversedByDimension` -- which only ever
// looks one level down -- silently stops describing reality.
func TestPostJournal_ReversalOfUID_RejectsReversalOfAReversal(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	svc, err := ledger.New(pool)
	require.NoError(t, err)
	writer := svc.JournalWriter()
	ctx := context.Background()

	curID := postgrestest.SeedCurrencyWithExponent(t, pool, "ACR", "Audit C2 Chain Unit", 2)
	jtID := postgrestest.SeedJournalType(t, pool, "transfer", "Transfer")
	wallet := postgrestest.SeedClassification(t, pool, "main_wallet", "Main Wallet", "debit", false)
	custodial := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)

	const holder = int64(4502)
	d := decimal.RequireFromString

	j, err := writer.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("ac2-chain-base"),
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: wallet, EntryType: core.EntryTypeDebit, Amount: d("100")},
			{AccountHolder: -holder, CurrencyUID: curID, ClassificationUID: custodial, EntryType: core.EntryTypeCredit, Amount: d("100")},
		},
	})
	require.NoError(t, err)

	rev, err := writer.ReverseJournal(ctx, j.UID, "mistake")
	require.NoError(t, err)

	// Re-reversing the reversal by hand: the legs are a perfectly shaped
	// inversion of `rev`, so only the "the target is itself a reversal" rule
	// can catch it.
	_, err = writer.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("ac2-chain-rerev"),
		ReversalOfUID:  rev.UID,
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: wallet, EntryType: core.EntryTypeDebit, Amount: d("100")},
			{AccountHolder: -holder, CurrencyUID: curID, ClassificationUID: custodial, EntryType: core.EntryTypeCredit, Amount: d("100")},
		},
	})
	require.Error(t, err, "a reversal may not itself be reversed; PostJournal must refuse the same link ReverseJournal refuses")
	require.ErrorIs(t, err, core.ErrConflict)
}

// TestPostJournal_ReversalOfUID_RejectsAmountBeyondRemaining pins the ceiling
// half of the gate: a correctly shaped hand-written reversal is still bounded
// by what is left to reverse, per dimension, exactly as
// `ReverseJournalFraction`'s own overshoot check is. Without it the two APIs
// disagree on the same journal -- the fraction API refuses to over-reverse
// while `PostJournal` posts the identical entries.
func TestPostJournal_ReversalOfUID_RejectsAmountBeyondRemaining(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	svc, err := ledger.New(pool)
	require.NoError(t, err)
	writer := svc.JournalWriter()
	ctx := context.Background()

	curID := postgrestest.SeedCurrencyWithExponent(t, pool, "ACC", "Audit C2 Ceiling Unit", 2)
	jtID := postgrestest.SeedJournalType(t, pool, "transfer", "Transfer")
	wallet := postgrestest.SeedClassification(t, pool, "main_wallet", "Main Wallet", "debit", false)
	custodial := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)

	const holder = int64(4503)
	d := decimal.RequireFromString

	j, err := writer.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("ac2-ceil-base"),
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: wallet, EntryType: core.EntryTypeDebit, Amount: d("100")},
			{AccountHolder: -holder, CurrencyUID: curID, ClassificationUID: custodial, EntryType: core.EntryTypeCredit, Amount: d("100")},
		},
	})
	require.NoError(t, err)

	_, err = writer.ReverseJournalFraction(ctx, j.UID, 1, 2, "half", postgrestest.UniqueKey("ac2-ceil-half"))
	require.NoError(t, err)

	// 60 > the 50 that is left. Shape is right, amount is not.
	_, err = writer.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("ac2-ceil-over"),
		ReversalOfUID:  j.UID,
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: wallet, EntryType: core.EntryTypeCredit, Amount: d("60")},
			{AccountHolder: -holder, CurrencyUID: curID, ClassificationUID: custodial, EntryType: core.EntryTypeDebit, Amount: d("60")},
		},
	})
	require.Error(t, err, "cumulative reversed must not exceed the original, whichever API posts the reversal")
	require.ErrorIs(t, err, core.ErrConflict)

	// The remaining 50 through the same hand-written path is legal and must
	// still be accepted -- the gate is a bound, not a ban.
	_, err = writer.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("ac2-ceil-rest"),
		ReversalOfUID:  j.UID,
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: wallet, EntryType: core.EntryTypeCredit, Amount: d("50")},
			{AccountHolder: -holder, CurrencyUID: curID, ClassificationUID: custodial, EntryType: core.EntryTypeDebit, Amount: d("50")},
		},
	})
	require.NoError(t, err, "a correctly shaped reversal within the remaining amount must still post")

	bal, err := svc.BalanceReader().GetBalance(ctx, holder, curID, wallet)
	require.NoError(t, err)
	require.True(t, bal.IsZero(), "half by API + half by hand must leave nothing, got %s", bal)
}
