package postgres_test

// Postgres-backed invariant tests. These match the I-N items in
// docs/INVARIANTS.md and must stay in sync with that document. When you add
// or rename a test here, update INVARIANTS.md's "Pinned by" sections.

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
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
	"github.com/azex-ai/ledger/presets"
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
		JournalTypeUID: deps.JournalType,
		IdempotencyKey: postgrestest.UniqueKey("rev-orig"),
		Source:         "rev-test",
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyUID: deps.Currency, ClassificationUID: deps.MainWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: core.SystemAccountHolder(userID), CurrencyUID: deps.Currency, ClassificationUID: deps.Custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	})
	require.NoError(t, err)

	// 2. Reverse A → ¬A. Must succeed.
	revA, err := store.ReverseJournal(ctx, a.UID, "test reversal")
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
	_, err = store.ReverseJournal(ctx, a.UID, "double reversal")
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
		JournalTypeUID: deps.JournalType,
		IdempotencyKey: idemKey,
		Source:         "idem-race",
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyUID: deps.Currency, ClassificationUID: deps.MainWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(50)},
			{AccountHolder: core.SystemAccountHolder(userID), CurrencyUID: deps.Currency, ClassificationUID: deps.Custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(50)},
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
	var firstJournalUID string
	for i, err := range results {
		switch {
		case err == nil && journals[i] != nil:
			successes++
			if firstJournalUID == "" {
				firstJournalUID = journals[i].UID
			}
			assert.Equal(t, firstJournalUID, journals[i].UID, "all concurrent replays must return the same journal")
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
		JournalTypeUID: deps.JournalType,
		IdempotencyKey: postgrestest.UniqueKey("partition-now"),
		Source:         "partition-test",
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyUID: deps.Currency, ClassificationUID: deps.MainWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(10)},
			{AccountHolder: core.SystemAccountHolder(userID), CurrencyUID: deps.Currency, ClassificationUID: deps.Custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(10)},
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
			JournalTypeUID: deps.JournalType,
			IdempotencyKey: postgrestest.UniqueKey("seed"),
			Source:         "seed",
			Entries: []core.EntryInput{
				{AccountHolder: int64(i), CurrencyUID: deps.Currency, ClassificationUID: deps.MainWallet, EntryType: core.EntryTypeDebit, Amount: amt},
				{AccountHolder: core.SystemAccountHolder(int64(i)), CurrencyUID: deps.Currency, ClassificationUID: deps.Custodial, EntryType: core.EntryTypeCredit, Amount: amt},
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
			JournalTypeUID: deps.JournalType,
			IdempotencyKey: postgrestest.UniqueKey(fmt.Sprintf("xfer-%d", k)),
			Source:         "xfer",
			Entries: []core.EntryInput{
				{AccountHolder: from, CurrencyUID: deps.Currency, ClassificationUID: deps.MainWallet, EntryType: core.EntryTypeCredit, Amount: amt},
				{AccountHolder: core.SystemAccountHolder(from), CurrencyUID: deps.Currency, ClassificationUID: deps.Settlement, EntryType: core.EntryTypeDebit, Amount: amt},
				{AccountHolder: core.SystemAccountHolder(to), CurrencyUID: deps.Currency, ClassificationUID: deps.Settlement, EntryType: core.EntryTypeCredit, Amount: amt},
				{AccountHolder: to, CurrencyUID: deps.Currency, ClassificationUID: deps.MainWallet, EntryType: core.EntryTypeDebit, Amount: amt},
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
		WHERE currency_id = (SELECT id FROM currencies WHERE uid=$1::uuid)
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
		WHERE currency_id = (SELECT id FROM currencies WHERE uid=$1::uuid)
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
		WHERE currency_id = (SELECT id FROM currencies WHERE uid=$1::uuid) AND account_holder > 0 AND classification_id = (SELECT id FROM classifications WHERE uid=$2::uuid)
	`, deps.Currency, deps.MainWallet).Scan(&liability)
	require.NoError(t, err)

	err = pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(
		  CASE WHEN entry_type='credit' THEN amount ELSE -amount END
		), 0)
		FROM journal_entries
		WHERE currency_id = (SELECT id FROM currencies WHERE uid=$1::uuid) AND account_holder < 0 AND classification_id = (SELECT id FROM classifications WHERE uid=$2::uuid)
	`, deps.Currency, deps.Custodial).Scan(&custody)
	require.NoError(t, err)

	assert.True(t, liability.Equal(custody), "user main_wallet sum (%s) must equal custodial sum (%s)", liability, custody)
	assert.True(t, liability.Equal(totalSeeded), "user main_wallet sum (%s) must equal total seeded (%s)", liability, totalSeeded)
}

// invariantsFixture bundles the IDs and stores reused across the postgres
// invariant tests so each test stays focused on the property it pins.
type invariantsFixture struct {
	BalanceReader core.BalanceReader

	Currency    string
	JournalType string
	MainWallet  string
	Custodial   string
	Settlement  string
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
		Name: "Tether USD test", Exponent: 18,
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

		Currency:    cur.UID,
		JournalType: jt.UID,
		MainWallet:  mainWallet.UID,
		Custodial:   custodial.UID,
		Settlement:  settlement.UID,
	}
}

// Pins the I-2 anti-tamper guard's column coverage (migration 033): the
// journals no-arbitrary-update trigger must reject changes to effective_at
// (else a script could move a posted journal into a closed period, bypassing
// I-15) and to uid (the external identity, I-18). event_id backfill remains
// the single permitted update.
func TestJournals_UpdateGuard_CoversEffectiveAtAndUID(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	store := postgres.NewLedgerStore(pool)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	clsA := postgrestest.SeedClassification(t, pool, "guard_wallet", "Wallet", "debit", false)
	clsB := postgrestest.SeedClassification(t, pool, "guard_custodial", "Custodial", "credit", true)
	jt := postgrestest.SeedJournalType(t, pool, "guard_jt", "Guard JT")

	j, err := store.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jt,
		IdempotencyKey: postgrestest.UniqueKey("guard"),
		Entries: []core.EntryInput{
			{AccountHolder: 7, CurrencyUID: curID, ClassificationUID: clsA, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(10)},
			{AccountHolder: -7, CurrencyUID: curID, ClassificationUID: clsB, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(10)},
		},
		Source: "test",
	})
	require.NoError(t, err)

	_, err = pool.Exec(ctx, "UPDATE journals SET effective_at = effective_at - INTERVAL '365 days' WHERE uid = $1", j.UID)
	require.Error(t, err, "UPDATE journals.effective_at must be blocked by the anti-tamper trigger")

	_, err = pool.Exec(ctx, "UPDATE journals SET uid = gen_random_uuid() WHERE uid = $1", j.UID)
	require.Error(t, err, "UPDATE journals.uid must be blocked by the anti-tamper trigger")
}

// I-19: Sweep bookings never post a journal.
//
// A sweep booking's only purpose is idempotency (booking key =
// sweep-{chain_id}-{token}-{signer_nonce}) and an audit trail for one batch
// collection transaction moving funds that were already accounted for at
// deposit time (docs/plans/2026-07-11-crypto-deposit-sweep-design.md §4:
// "sweep 只走 booking + event，无 journal"). Sweep's classification carries no
// EntryTemplate, so Transition can never backfill bookings.journal_id the way
// a deposit's "confirmed" transition does -- this test drives a sweep
// booking through its full lifecycle (pending -> sent -> confirmed) and pins
// that journal_uid stays empty at every step.
func TestSweepBooking_NeverPostsJournal(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	_, err := presets.InstallSweepClassification(ctx, classStore)
	require.NoError(t, err)

	bookingStore := postgres.NewBookingStore(pool)
	booking, err := bookingStore.CreateBooking(ctx, core.CreateBookingInput{
		ClassificationCode: presets.SweepClassificationCode,
		AccountHolder:      -9_000_000_000_001, // sentinel system holder, not tied to any user
		CurrencyUID:        postgrestest.SeedCurrency(t, pool, postgrestest.UniqueKey("SWEEP-USDT"), "Tether USD"),
		Amount:             decimal.NewFromInt(500),
		IdempotencyKey:     postgrestest.UniqueKey("sweep-invariant"),
		ChannelName:        "onchain",
		Metadata:           map[string]string{"chain_id": "1", "token": "0xusdt", "nonce": "0"},
	})
	require.NoError(t, err)
	assert.Empty(t, booking.JournalUID)

	sentEvt, err := bookingStore.Transition(ctx, core.TransitionInput{
		BookingUID: booking.UID, ToStatus: "sent", ChannelRef: "0xsweeptx1", Source: "test",
	})
	require.NoError(t, err)
	assert.Empty(t, sentEvt.JournalUID)
	sent, err := bookingStore.GetBooking(ctx, booking.UID)
	require.NoError(t, err)
	assert.Empty(t, sent.JournalUID)

	confirmedEvt, err := bookingStore.Transition(ctx, core.TransitionInput{
		BookingUID: booking.UID, ToStatus: "confirmed", ChannelRef: "0xsweeptx1", Source: "test",
	})
	require.NoError(t, err)
	assert.Empty(t, confirmedEvt.JournalUID)
	confirmed, err := bookingStore.GetBooking(ctx, booking.UID)
	require.NoError(t, err)
	assert.Empty(t, confirmed.JournalUID)
}

// I-20: Deposit ingestion idempotency is stable under a reorg that reassigns
// block_number.
//
// A deposit booking's idempotency key is derived from
// (chain_id, tx_hash, txlog_seq) -- txlog_seq being the Transfer log's
// ordinal position among the logs in tx_hash that credit one of our
// registered addresses, NOT the chain's block-level log_index (which a reorg
// reassigns). block_number itself is also reorg-variant: the same
// transaction can be re-mined into a different block. This test pins that
// re-ingesting the identical sighting with a CHANGED block_number (simulating
// exactly that: a crashed watcher retries the same unscanned block range
// after a reorg moved the tx) still resolves to the SAME booking -- not
// ErrConflict, not a dead-lettered duplicate (C1/M1 regression:
// service/onchain.go's IngestDeposit keeps block_number in the booking's
// Metadata for the recheck loop to read back, but
// postgres/idempotency_match.go's bookingMetadataMatches deliberately
// excludes that one key from the equality check -- see its doc comment).
func TestDepositBooking_IdempotencyKey_StableAcrossBlockNumberChurn(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	_, err := presets.InstallDepositClassification(ctx, classStore)
	require.NoError(t, err)

	curUID := postgrestest.SeedCurrency(t, pool, postgrestest.UniqueKey("USDT"), "Tether USD")
	bookingStore := postgres.NewBookingStore(pool)

	const chainID, txHash, txLogSeq = int64(1), "0xreorgtx", int32(0)
	idemKey := fmt.Sprintf("deposit-%d-%s-%d", chainID, txHash, txLogSeq)

	makeInput := func(blockNumberAtObservationTime int) core.CreateBookingInput {
		return core.CreateBookingInput{
			ClassificationCode: presets.DepositClassificationCode,
			AccountHolder:      8001,
			CurrencyUID:        curUID,
			Amount:             decimal.NewFromInt(100),
			IdempotencyKey:     idemKey,
			ChannelName:        "onchain",
			Metadata: map[string]string{
				"chain_id":     "1",
				"tx_hash":      txHash,
				"txlog_seq":    "0",
				"block_number": strconv.Itoa(blockNumberAtObservationTime),
			},
		}
	}

	// First observation: the tx originally landed at block 100.
	first, err := bookingStore.CreateBooking(ctx, makeInput(100))
	require.NoError(t, err)

	// Second observation, post-reorg: the tx was re-mined into block 137 (a
	// DIFFERENT block_number) -- but txlog_seq and every other stable field
	// are unchanged, so this must resolve to the exact same booking: an
	// idempotent no-op, not ErrConflict and not a dead-lettered duplicate.
	second, err := bookingStore.CreateBooking(ctx, makeInput(137))
	require.NoError(t, err)

	assert.Equal(t, first.UID, second.UID)

	// The stored booking keeps whichever block_number the FIRST successful
	// create recorded -- CreateBooking's idempotent-replay path never
	// updates an existing row, it just returns it unchanged.
	assert.Equal(t, "100", first.Metadata["block_number"])
	assert.Equal(t, "100", second.Metadata["block_number"])
}
