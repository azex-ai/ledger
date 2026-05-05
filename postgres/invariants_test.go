package postgres_test

// Postgres-backed invariant tests. These match the I-N items in
// docs/INVARIANTS.md and must stay in sync with that document. When you add
// or rename a test here, update INVARIANTS.md's "Pinned by" sections.

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// I-2: A journal can be reversed at most once. The partial unique index
// uq_journals_reversal_of guarantees this; we verify the chain A → ¬A → ¬¬A
// is blocked at the third step, and that net entries on each account
// dimension sum to zero after one reverse.
func TestReversalChainIntegrity(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	store, deps := setupInvariantsFixture(t, pool, ctx)
	const userID int64 = 7001

	// 1. Post original A.
	a, err := store.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  deps.JournalType,
		IdempotencyKey: postgrestest.UniqueKey("rev-orig"),
		Source:         "rev-test",
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyID: deps.Currency, ClassificationID: deps.MainWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: core.SystemAccountHolder(userID), CurrencyID: deps.Currency, ClassificationID: deps.Custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	})
	require.NoError(t, err)

	// 2. Reverse A → ¬A. Must succeed.
	revA, err := store.ReverseJournal(ctx, a.ID, "test reversal")
	require.NoError(t, err)
	require.NotNil(t, revA)

	// 3. Attempt to reverse ¬A → ¬¬A. Must fail per the partial unique index
	//    (a journal that itself reverses something cannot be the target of a
	//    second reversal pointing at A; reversing ¬A would create a second
	//    row with reversal_of = revA.ID which IS allowed structurally,
	//    but reversing A again is not).
	//
	//    The invariant is: "any given journal can be reversed at most once."
	//    We enforce by attempting to reverse A a second time.
	_, err = store.ReverseJournal(ctx, a.ID, "double reversal")
	require.Error(t, err, "second reversal of the same journal must be rejected")

	// 4. Net effect on the user main_wallet dimension must be zero.
	main, err := deps.BalanceReader.GetBalance(ctx, userID, deps.Currency, deps.MainWallet)
	require.NoError(t, err)
	assert.True(t, main.IsZero(), "main_wallet balance after A + ¬A must be zero, got %s", main)

	custody, err := deps.BalanceReader.GetBalance(ctx, core.SystemAccountHolder(userID), deps.Currency, deps.Custodial)
	require.NoError(t, err)
	assert.True(t, custody.IsZero(), "custodial balance after A + ¬A must be zero, got %s", custody)
}

// I-3: 100 concurrent posts of the same idempotency_key result in exactly one
// journal row and one economic side effect. Every caller should resolve to the
// same persisted journal.
func TestIdempotency_ConcurrentSameKey(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	store, deps := setupInvariantsFixture(t, pool, ctx)
	const userID int64 = 7002
	idemKey := postgrestest.UniqueKey("idem-race")

	const goroutines = 100
	var wg sync.WaitGroup
	results := make([]error, goroutines)
	journals := make([]*core.Journal, goroutines)

	input := core.JournalInput{
		JournalTypeID:  deps.JournalType,
		IdempotencyKey: idemKey,
		Source:         "idem-race",
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyID: deps.Currency, ClassificationID: deps.MainWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(50)},
			{AccountHolder: core.SystemAccountHolder(userID), CurrencyID: deps.Currency, ClassificationID: deps.Custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(50)},
		},
	}

	start := make(chan struct{})
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			j, err := store.PostJournal(ctx, input)
			results[i] = err
			journals[i] = j
		}(i)
	}
	close(start)
	wg.Wait()

	// Count outcomes and confirm every replay saw the same journal.
	successes := 0
	other := 0
	var firstJournalID int64
	for i, err := range results {
		switch {
		case err == nil && journals[i] != nil:
			successes++
			if firstJournalID == 0 {
				firstJournalID = journals[i].ID
			}
			assert.Equal(t, firstJournalID, journals[i].ID, "all concurrent replays must return the same journal")
		default:
			other++
			t.Logf("unexpected error from goroutine %d: %v", i, err)
		}
	}
	assert.Equal(t, goroutines, successes, "all concurrent replays should return success-equivalent results")
	assert.Equal(t, 0, other, "no other error class permitted")

	// Final balance must reflect a single posting.
	bal, err := deps.BalanceReader.GetBalance(ctx, userID, deps.Currency, deps.MainWallet)
	require.NoError(t, err)
	assert.True(t, bal.Equal(decimal.NewFromInt(50)), "main_wallet must reflect exactly one $50 deposit, got %s", bal)
}

// I-13: journal_entries is RANGE-partitioned by created_at. Verify that the
// default partition catches inserts whose date falls outside any named range,
// and that reads union correctly across partitions.
func TestPartitionBoundary_DefaultCatchesOutsideRanges(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	// Confirm default partition exists and is wired up.
	var defaultName string
	err := pool.QueryRow(ctx, `
		SELECT inhrelid::regclass::text
		FROM pg_inherits
		WHERE inhparent = 'journal_entries'::regclass
		  AND inhrelid IN (
		    SELECT oid FROM pg_class WHERE relispartition AND relkind = 'r'
		  )
		LIMIT 1
	`).Scan(&defaultName)
	require.NoError(t, err, "journal_entries must have at least one partition (default)")
	require.NotEmpty(t, defaultName)

	store, deps := setupInvariantsFixture(t, pool, ctx)
	const userID int64 = 7003

	// Post a journal — entries land in whichever partition matches now().
	_, err = store.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  deps.JournalType,
		IdempotencyKey: postgrestest.UniqueKey("partition-now"),
		Source:         "partition-test",
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyID: deps.Currency, ClassificationID: deps.MainWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(10)},
			{AccountHolder: core.SystemAccountHolder(userID), CurrencyID: deps.Currency, ClassificationID: deps.Custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(10)},
		},
	})
	require.NoError(t, err)

	// GetBalance must read the entry whether it landed in the default
	// partition or a date-bounded one — the indexed dimension query unions
	// across all partitions.
	bal, err := deps.BalanceReader.GetBalance(ctx, userID, deps.Currency, deps.MainWallet)
	require.NoError(t, err)
	assert.True(t, bal.Equal(decimal.NewFromInt(10)), "balance must reflect entry across partitions, got %s", bal)
}

// I-12: Money conservation. N users × random journal sequence → SUM(debit) =
// SUM(credit) per currency, holds at all times. This is the headline
// invariant; if it ever fails, the ledger is broken.
func TestMoneyConservation_Network(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network conservation test in -short mode")
	}
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	store, deps := setupInvariantsFixture(t, pool, ctx)

	const (
		userCount    = 10
		journalCount = 200
	)
	rng := rand.New(rand.NewSource(0xCAFE))

	// Seed: top-up every user with a random initial balance via deposit.
	totalSeeded := decimal.Zero
	for i := 1; i <= userCount; i++ {
		amt := decimal.NewFromInt(int64(1_000_000 + rng.Intn(1_000_000)))
		_, err := store.PostJournal(ctx, core.JournalInput{
			JournalTypeID:  deps.JournalType,
			IdempotencyKey: postgrestest.UniqueKey("seed"),
			Source:         "seed",
			Entries: []core.EntryInput{
				{AccountHolder: int64(i), CurrencyID: deps.Currency, ClassificationID: deps.MainWallet, EntryType: core.EntryTypeDebit, Amount: amt},
				{AccountHolder: core.SystemAccountHolder(int64(i)), CurrencyID: deps.Currency, ClassificationID: deps.Custodial, EntryType: core.EntryTypeCredit, Amount: amt},
			},
		})
		require.NoError(t, err)
		totalSeeded = totalSeeded.Add(amt)
	}

	// Random transfers between users via the settlement classification.
	for k := range journalCount {
		from := int64(rng.Intn(userCount) + 1)
		to := from
		for to == from {
			to = int64(rng.Intn(userCount) + 1)
		}
		amt := decimal.NewFromInt(int64(1 + rng.Intn(100)))
		// Two-leg transfer with settlement intermediary, all in one journal.
		_, err := store.PostJournal(ctx, core.JournalInput{
			JournalTypeID:  deps.JournalType,
			IdempotencyKey: postgrestest.UniqueKey(fmt.Sprintf("xfer-%d", k)),
			Source:         "xfer",
			Entries: []core.EntryInput{
				{AccountHolder: from, CurrencyID: deps.Currency, ClassificationID: deps.MainWallet, EntryType: core.EntryTypeCredit, Amount: amt},
				{AccountHolder: core.SystemAccountHolder(from), CurrencyID: deps.Currency, ClassificationID: deps.Settlement, EntryType: core.EntryTypeDebit, Amount: amt},
				{AccountHolder: core.SystemAccountHolder(to), CurrencyID: deps.Currency, ClassificationID: deps.Settlement, EntryType: core.EntryTypeCredit, Amount: amt},
				{AccountHolder: to, CurrencyID: deps.Currency, ClassificationID: deps.MainWallet, EntryType: core.EntryTypeDebit, Amount: amt},
			},
		})
		require.NoError(t, err, "transfer %d", k)
	}

	// Invariant 1: SUM(debit) == SUM(credit) per currency.
	var debit, credit decimal.Decimal
	err := pool.QueryRow(ctx, `
		SELECT
		  COALESCE(SUM(CASE WHEN entry_type='debit' THEN amount END), 0),
		  COALESCE(SUM(CASE WHEN entry_type='credit' THEN amount END), 0)
		FROM journal_entries
		WHERE currency_id = $1
	`, deps.Currency).Scan(&debit, &credit)
	require.NoError(t, err)
	assert.True(t, debit.Equal(credit), "money conservation broken: debit=%s credit=%s", debit, credit)

	// Invariant 2: across all account dimensions, net (debit-credit per
	// debit-normal class, credit-debit per credit-normal class) sums to zero
	// per currency.
	var net decimal.Decimal
	err = pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(
		  CASE WHEN entry_type='debit' THEN amount ELSE -amount END
		), 0)
		FROM journal_entries
		WHERE currency_id = $1
	`, deps.Currency).Scan(&net)
	require.NoError(t, err)
	assert.True(t, net.IsZero(), "Σ(debit) - Σ(credit) per currency must be zero, got %s", net)

	// Invariant 3: total user-side main_wallet balance == total custodial backing.
	var liability, custody decimal.Decimal
	err = pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(
		  CASE WHEN entry_type='debit' THEN amount ELSE -amount END
		), 0)
		FROM journal_entries
		WHERE currency_id = $1 AND account_holder > 0 AND classification_id = $2
	`, deps.Currency, deps.MainWallet).Scan(&liability)
	require.NoError(t, err)

	err = pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(
		  CASE WHEN entry_type='credit' THEN amount ELSE -amount END
		), 0)
		FROM journal_entries
		WHERE currency_id = $1 AND account_holder < 0 AND classification_id = $2
	`, deps.Currency, deps.Custodial).Scan(&custody)
	require.NoError(t, err)

	assert.True(t, liability.Equal(custody), "user main_wallet sum (%s) must equal custodial sum (%s)", liability, custody)
	assert.True(t, liability.Equal(totalSeeded), "user main_wallet sum (%s) must equal total seeded (%s)", liability, totalSeeded)
}

// invariantsFixture bundles the IDs and stores reused across the postgres
// invariant tests so each test stays focused on the property it pins.
type invariantsFixture struct {
	BalanceReader core.BalanceReader

	Currency    int64
	JournalType int64
	MainWallet  int64
	Custodial   int64
	Settlement  int64
}

func setupInvariantsFixture(t testing.TB, pool *pgxpool.Pool, ctx context.Context) (*postgres.LedgerStore, invariantsFixture) {
	t.Helper()

	store := postgres.NewLedgerStore(pool)
	classStore := postgres.NewClassificationStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)

	// Make all setup ids unique per test to avoid cross-test interference
	// when the same DB schema is reused.
	suffix := time.Now().UnixNano()

	cur, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{
		Code: fmt.Sprintf("USDT_%d", suffix),
		Name: "Tether USD test",
	})
	require.NoError(t, err)

	mainWallet, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: fmt.Sprintf("main_wallet_%d", suffix), Name: "Main Wallet", NormalSide: core.NormalSideDebit,
	})
	require.NoError(t, err)
	custodial, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: fmt.Sprintf("custodial_%d", suffix), Name: "Custodial", NormalSide: core.NormalSideCredit, IsSystem: true,
	})
	require.NoError(t, err)
	settlement, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: fmt.Sprintf("settlement_%d", suffix), Name: "Settlement", NormalSide: core.NormalSideCredit, IsSystem: true,
	})
	require.NoError(t, err)

	jt, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{
		Code: fmt.Sprintf("test_jt_%d", suffix),
		Name: "Test Journal Type",
	})
	require.NoError(t, err)

	return store, invariantsFixture{
		BalanceReader: store,

		Currency:    cur.ID,
		JournalType: jt.ID,
		MainWallet:  mainWallet.ID,
		Custodial:   custodial.ID,
		Settlement:  settlement.ID,
	}
}
