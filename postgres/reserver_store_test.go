package postgres_test

import (
	"context"
	"fmt"
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

func TestReserverStore_Reserve_Settle(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger, postgres.NewVerifiedBalanceStore(pool, nil))
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	seedReservableBalance(t, ctx, ledger, pool, 1, curID, decimal.NewFromInt(100))

	res, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder:  1,
		CurrencyUID:    curID,
		Amount:         decimal.NewFromInt(100),
		IdempotencyKey: postgrestest.UniqueKey("res-settle"),
		ExpiresIn:      10 * time.Minute,
	})
	require.NoError(t, err)
	assert.Equal(t, core.ReservationStatusActive, res.Status)
	assert.True(t, res.ReservedAmount.Equal(decimal.NewFromInt(100)))

	// Settle for less than the full reserved amount (95 of 100 reserved) --
	// the unreserved remainder (5) is implicitly released, same as
	// FinalizeSettlement's semantics.
	err = store.Settle(ctx, core.SettleInput{ReservationUID: res.UID, Amount: decimal.NewFromInt(95), IdempotencyKey: postgrestest.UniqueKey("res-settle-op")})
	require.NoError(t, err)

	// require.NoError alone proves nothing about what Settle actually did --
	// verify the row really transitioned and really recorded 95, not (say) a
	// no-op that silently returned nil.
	status, settledAmount := reservationRow(t, ctx, pool, res.UID)
	assert.Equal(t, string(core.ReservationStatusSettled), status, "reservation must transition to settled")
	assert.True(t, settledAmount.Equal(decimal.NewFromInt(95)), "settled_amount must record the actual settled amount, want 95 got %s", settledAmount)

	// A settled reservation must drop out of the holder's held total --
	// otherwise the unused 5 (and the settled 95) would stay locked forever.
	held, err := store.HeldAmount(ctx, 1, curID)
	require.NoError(t, err)
	assert.True(t, held.IsZero(), "settled reservation must no longer count toward held amount, got %s", held)
}

func TestReserverStore_Reserve_Release(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger, postgres.NewVerifiedBalanceStore(pool, nil))
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	seedReservableBalance(t, ctx, ledger, pool, 2, curID, decimal.NewFromInt(50))

	res, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder:  2,
		CurrencyUID:    curID,
		Amount:         decimal.NewFromInt(50),
		IdempotencyKey: postgrestest.UniqueKey("res-release"),
		ExpiresIn:      5 * time.Minute,
	})
	require.NoError(t, err)

	err = store.Release(ctx, core.ReleaseInput{ReservationUID: res.UID, IdempotencyKey: postgrestest.UniqueKey("res-release-op")})
	require.NoError(t, err)

	// Cannot settle after release
	err = store.Settle(ctx, core.SettleInput{ReservationUID: res.UID, Amount: decimal.NewFromInt(50), IdempotencyKey: postgrestest.UniqueKey("res-settle-after-release")})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidTransition)
}

func TestReserverStore_Reserve_Idempotent(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger, postgres.NewVerifiedBalanceStore(pool, nil))
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	seedReservableBalance(t, ctx, ledger, pool, 3, curID, decimal.NewFromInt(100))

	key := postgrestest.UniqueKey("res-idem")
	input := core.ReserveInput{
		AccountHolder:  3,
		CurrencyUID:    curID,
		Amount:         decimal.NewFromInt(100),
		IdempotencyKey: key,
		ExpiresIn:      10 * time.Minute,
	}

	r1, err := store.Reserve(ctx, input)
	require.NoError(t, err)

	r2, err := store.Reserve(ctx, input)
	require.NoError(t, err)
	assert.Equal(t, r1.UID, r2.UID)
}

func TestReserverStore_Reserve_IdempotentPayloadMismatch(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger, postgres.NewVerifiedBalanceStore(pool, nil))
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT-RES-IDEM", "Tether USD")
	seedReservableBalance(t, ctx, ledger, pool, 31, curID, decimal.NewFromInt(100))

	key := postgrestest.UniqueKey("res-idem-mismatch")
	_, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder:  31,
		CurrencyUID:    curID,
		Amount:         decimal.NewFromInt(40),
		IdempotencyKey: key,
		ExpiresIn:      10 * time.Minute,
	})
	require.NoError(t, err)

	_, err = store.Reserve(ctx, core.ReserveInput{
		AccountHolder:  31,
		CurrencyUID:    curID,
		Amount:         decimal.NewFromInt(50),
		IdempotencyKey: key,
		ExpiresIn:      10 * time.Minute,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrConflict)
}

// TestReserverStore_Reserve_Concurrent only proves two concurrent Reserve
// calls don't crash / deadlock / collide on idempotency when their combined
// amount (50+30=80) stays under the funded balance (100) -- both requests
// SHOULD succeed regardless of whether the advisory lock exists, so this
// test passes even if the lock in postgres.ReserverStore.Reserve is deleted
// entirely (verified: see TestReserverStore_Reserve_Concurrent_RejectsOverCommit's
// doc comment for the mutation-testing evidence). It does not exercise I-4/
// I-11's actual TOCTOU claim ("two concurrent reserves that together exceed
// the available balance must not both succeed") -- that is
// TestReserverStore_Reserve_Concurrent_RejectsOverCommit below.
func TestReserverStore_Reserve_Concurrent(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger, postgres.NewVerifiedBalanceStore(pool, nil))
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	seedReservableBalance(t, ctx, ledger, pool, 10, curID, decimal.NewFromInt(100))

	// Both should succeed (advisory lock serializes)
	var wg sync.WaitGroup
	var res1, res2 *core.Reservation
	var err1, err2 error

	wg.Add(2)
	go func() {
		defer wg.Done()
		res1, err1 = store.Reserve(ctx, core.ReserveInput{
			AccountHolder:  10,
			CurrencyUID:    curID,
			Amount:         decimal.NewFromInt(50),
			IdempotencyKey: postgrestest.UniqueKey("conc-a"),
			ExpiresIn:      10 * time.Minute,
		})
	}()
	go func() {
		defer wg.Done()
		res2, err2 = store.Reserve(ctx, core.ReserveInput{
			AccountHolder:  10,
			CurrencyUID:    curID,
			Amount:         decimal.NewFromInt(30),
			IdempotencyKey: postgrestest.UniqueKey("conc-b"),
			ExpiresIn:      10 * time.Minute,
		})
	}()
	wg.Wait()

	require.NoError(t, err1)
	require.NoError(t, err2)
	assert.NotEqual(t, res1.UID, res2.UID)
}

// TestReserverStore_Reserve_Concurrent_RejectsOverCommit pins I-4/I-11's
// actual claim (docs/INVARIANTS.md: "Two concurrent reserve calls can each
// read 'balance is enough', then both insert reservations, leaving the
// holder over-committed"): fund a holder with exactly 100, fire many
// concurrent Reserve calls of 15 each (10 x 15 = 150, well over the funded
// balance), and require the combined reserved amount never exceeds the
// funded balance -- i.e. Reserve must actually serialize against itself,
// not just avoid crashing.
//
// Why 10 goroutines and not 2: a 2-goroutine version of this same assertion
// (balance=100, two concurrent Reserve(60)) was tried first and did NOT
// reliably go red when the lock was removed -- with only two short local
// round-trips to a real (fast, low-latency Dockerized) Postgres, the Go
// scheduler and network timing happened to serialize the two calls often
// enough that the race window was frequently missed by luck, not caught by
// the code. Widening to 10 concurrent goroutines against the same balance
// closes that gap: verified locally (see below) that with the app-level
// advisory lock removed, 10-way concurrency overcommits every single run.
//
// Mutation-tested: with the pg_advisory_xact_lock acquisition in
// postgres.ReserverStore.reserveWithQueries (the acquireBalanceLocks call)
// temporarily replaced with `if false { ... }`, this test goes red on every
// run (3/3 trials observed locally: successes=8/sum=120, successes=10/
// sum=150, successes=7/sum=105 -- all exceeding the funded balance of 100).
// Restoring the lock line makes it pass again, with the combined reserved
// amount capped at the largest multiple of 15 that is <= 100 (i.e. 90,
// 6 winners). TestReserverStore_Reserve_Concurrent above does NOT go red
// under the same mutation with only 2 participants (50+30=80 never exceeds
// the funded balance regardless of locking, and even at higher amounts 2
// participants don't reliably hit the window) -- that gap in what a
// concurrent test can actually prove is exactly what this test closes.
func TestReserverStore_Reserve_Concurrent_RejectsOverCommit(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger, postgres.NewVerifiedBalanceStore(pool, nil))
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	const holder = int64(11)
	const funded = 100
	const perAttempt = 15
	const attempts = 10 // 10 x 15 = 150 > funded balance of 100
	seedReservableBalance(t, ctx, ledger, pool, holder, curID, decimal.NewFromInt(funded))

	results := make([]*core.Reservation, attempts)
	errs := make([]error, attempts)

	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start // align start so every goroutine races the same window
			results[i], errs[i] = store.Reserve(ctx, core.ReserveInput{
				AccountHolder:  holder,
				CurrencyUID:    curID,
				Amount:         decimal.NewFromInt(perAttempt),
				IdempotencyKey: postgrestest.UniqueKey(fmt.Sprintf("overcommit-%d", i)),
				ExpiresIn:      10 * time.Minute,
			})
		}()
	}
	close(start)
	wg.Wait()

	successes := 0
	sumReserved := decimal.Zero
	for i := 0; i < attempts; i++ {
		if errs[i] == nil {
			successes++
			sumReserved = sumReserved.Add(results[i].ReservedAmount)
		} else {
			assert.ErrorIs(t, errs[i], core.ErrInsufficientBalance, "attempt %d: the loser must fail with insufficient balance, got %v", i, errs[i])
		}
	}

	assert.True(t, sumReserved.LessThanOrEqual(decimal.NewFromInt(funded)),
		"combined reserved amount %s (from %d/%d successful concurrent Reserve(15) calls) must never exceed the funded balance %d",
		sumReserved, successes, attempts, funded)
	assert.True(t, sumReserved.Equal(decimal.NewFromInt(int64(successes*perAttempt))),
		"sumReserved must equal successes*perAttempt exactly (no partial/corrupted reservation amounts)")

	held, err := store.HeldAmount(ctx, holder, curID)
	require.NoError(t, err)
	assert.True(t, held.Equal(sumReserved), "held amount must match the sum of winning reservations, want %s got %s", sumReserved, held)
}

// Pins I-3 for Settle. Before this fix, SettleInput carried no idempotency
// key at all, and this test (then named TestReserverStore_Settle_InvalidTransition)
// asserted that calling Settle a second time with the exact same payload
// FAILED with ErrInvalidTransition -- i.e. it certified the bug: a
// lost-response retry of a successful Settle was indistinguishable from a
// genuine conflict (someone else settling a different amount, or the
// reservation having been released out from under the caller). This is
// inverted, not deleted, so what changed stays visible: a replay (same key,
// same amount) now returns the original success; a genuinely new attempt
// (a never-before-seen key) against an already-terminal reservation is
// still ErrInvalidTransition, because that IS a real conflict, not a
// replay; and a replayed key with a different amount is ErrConflict, never
// a silent success.
func TestReserverStore_Settle_IdempotentReplay(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger, postgres.NewVerifiedBalanceStore(pool, nil))
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	seedReservableBalance(t, ctx, ledger, pool, 5, curID, decimal.NewFromInt(100))

	res, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder:  5,
		CurrencyUID:    curID,
		Amount:         decimal.NewFromInt(100),
		IdempotencyKey: postgrestest.UniqueKey("double-settle"),
		ExpiresIn:      10 * time.Minute,
	})
	require.NoError(t, err)

	key := postgrestest.UniqueKey("double-settle-op")

	// Settle once.
	err = store.Settle(ctx, core.SettleInput{ReservationUID: res.UID, Amount: decimal.NewFromInt(100), IdempotencyKey: key})
	require.NoError(t, err)

	status, settledAmount := reservationRow(t, ctx, pool, res.UID)
	assert.Equal(t, string(core.ReservationStatusSettled), status, "first Settle must actually transition the row to settled")
	assert.True(t, settledAmount.Equal(decimal.NewFromInt(100)), "want settled_amount=100 after the first Settle, got %s", settledAmount)

	// Lost-response retry: same key, same amount -- must return the
	// original success, NOT ErrInvalidTransition, AND must not double-apply
	// (settled_amount must stay 100, not become 200).
	err = store.Settle(ctx, core.SettleInput{ReservationUID: res.UID, Amount: decimal.NewFromInt(100), IdempotencyKey: key})
	require.NoError(t, err)

	statusAfterReplay, settledAmountAfterReplay := reservationRow(t, ctx, pool, res.UID)
	assert.Equal(t, string(core.ReservationStatusSettled), statusAfterReplay)
	assert.True(t, settledAmountAfterReplay.Equal(decimal.NewFromInt(100)), "replay must be a true no-op, not re-apply -- want settled_amount still 100, got %s", settledAmountAfterReplay)

	// Same key, different amount -- payload mismatch is ErrConflict.
	err = store.Settle(ctx, core.SettleInput{ReservationUID: res.UID, Amount: decimal.NewFromInt(50), IdempotencyKey: key})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrConflict)

	// A genuinely new attempt (never-before-seen key) against an
	// already-settled reservation is a real conflict, distinct from a
	// replay, and is still rejected.
	err = store.Settle(ctx, core.SettleInput{ReservationUID: res.UID, Amount: decimal.NewFromInt(100), IdempotencyKey: postgrestest.UniqueKey("double-settle-op-new")})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidTransition)
}

func TestReserverStore_HeldAmount(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger, postgres.NewVerifiedBalanceStore(pool, nil))
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	seedReservableBalance(t, ctx, ledger, pool, 7, curID, decimal.NewFromInt(100))

	held, err := store.HeldAmount(ctx, 7, curID)
	require.NoError(t, err)
	assert.True(t, held.IsZero(), "no reservations yet, held should be 0, got %s", held)

	r1, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder: 7, CurrencyUID: curID, Amount: decimal.NewFromInt(30),
		IdempotencyKey: postgrestest.UniqueKey("held-a"), ExpiresIn: 10 * time.Minute,
	})
	require.NoError(t, err)

	_, err = store.Reserve(ctx, core.ReserveInput{
		AccountHolder: 7, CurrencyUID: curID, Amount: decimal.NewFromInt(20),
		IdempotencyKey: postgrestest.UniqueKey("held-b"), ExpiresIn: 10 * time.Minute,
	})
	require.NoError(t, err)

	held, err = store.HeldAmount(ctx, 7, curID)
	require.NoError(t, err)
	assert.True(t, held.Equal(decimal.NewFromInt(50)), "two active reservations, want 50, got %s", held)

	// A different holder's total is unaffected (WHERE account_holder isolation).
	other, err := store.HeldAmount(ctx, 8, curID)
	require.NoError(t, err)
	assert.True(t, other.IsZero(), "holder 8 has no reservations, want 0, got %s", other)

	// Releasing one active reservation drops it out of the held total.
	require.NoError(t, store.Release(ctx, core.ReleaseInput{ReservationUID: r1.UID, IdempotencyKey: postgrestest.UniqueKey("held-release")}))
	held, err = store.HeldAmount(ctx, 7, curID)
	require.NoError(t, err)
	assert.True(t, held.Equal(decimal.NewFromInt(20)), "after release, want 20, got %s", held)
}

func seedReservableBalance(t *testing.T, ctx context.Context, ledger *postgres.LedgerStore, pool *pgxpool.Pool, holder int64, currencyUID string, amount decimal.Decimal) {
	t.Helper()

	journalTypeID := postgrestest.SeedJournalType(t, pool, "fund_account", "Fund Account")
	// main_wallet must carry balance_role='available' — Reserve's availability
	// base sums role=available classifications only.
	walletID := postgrestest.SeedClassificationWithRole(t, pool, "main_wallet", "Main Wallet", "debit", false, "available")
	custodialID := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)

	_, err := ledger.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: journalTypeID,
		IdempotencyKey: postgrestest.UniqueKey("seed-reserve-balance"),
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: currencyUID, ClassificationUID: walletID, EntryType: core.EntryTypeDebit, Amount: amount},
			{AccountHolder: -holder, CurrencyUID: currencyUID, ClassificationUID: custodialID, EntryType: core.EntryTypeCredit, Amount: amount},
		},
		Source: "test",
	})
	require.NoError(t, err)
}

// reservationRow reads the reservation's status and settled_amount directly
// off the reservations table -- ReserverStore exposes no reader for this, so
// tests that need to verify Settle's actual DB-level effect (not just its
// error return) query the row the same way the other pin tests in this file
// query journals/events directly via pool.
func reservationRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, reservationUID string) (status string, settledAmount decimal.Decimal) {
	t.Helper()
	var settledStr string
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT status, settled_amount FROM reservations WHERE uid=$1", reservationUID,
	).Scan(&status, &settledStr))
	settledAmount, err := decimal.NewFromString(settledStr)
	require.NoError(t, err)
	return status, settledAmount
}

func TestReserverStore_Settle_ZeroAmountRejected(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger, postgres.NewVerifiedBalanceStore(pool, nil))
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	seedReservableBalance(t, ctx, ledger, pool, 20, curID, decimal.NewFromInt(100))

	res, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder:  20,
		CurrencyUID:    curID,
		Amount:         decimal.NewFromInt(100),
		IdempotencyKey: postgrestest.UniqueKey("settle-zero"),
		ExpiresIn:      10 * time.Minute,
	})
	require.NoError(t, err)

	err = store.Settle(ctx, core.SettleInput{ReservationUID: res.UID, Amount: decimal.Zero, IdempotencyKey: postgrestest.UniqueKey("settle-zero-op")})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)
}

func TestReserverStore_Settle_NegativeAmountRejected(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger, postgres.NewVerifiedBalanceStore(pool, nil))
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	seedReservableBalance(t, ctx, ledger, pool, 21, curID, decimal.NewFromInt(100))

	res, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder:  21,
		CurrencyUID:    curID,
		Amount:         decimal.NewFromInt(100),
		IdempotencyKey: postgrestest.UniqueKey("settle-negative"),
		ExpiresIn:      10 * time.Minute,
	})
	require.NoError(t, err)

	err = store.Settle(ctx, core.SettleInput{ReservationUID: res.UID, Amount: decimal.NewFromInt(-1), IdempotencyKey: postgrestest.UniqueKey("settle-negative-op")})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)
}

// Over-settlement (actualAmount > reserved_amount) is rejected: settling more
// than was reserved would let a caller debit funds that were never locked,
// breaking the TOCTOU-safe budget-hold guarantee Reserve provides. No
// shipped example or test depends on over-settlement being allowed, and the
// DB already enforces this via chk_settled_lte_reserved — this test pins the
// Go-level fail-fast check added in front of that constraint.
func TestReserverStore_Settle_ExceedsReservedRejected(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger, postgres.NewVerifiedBalanceStore(pool, nil))
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	seedReservableBalance(t, ctx, ledger, pool, 22, curID, decimal.NewFromInt(100))

	res, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder:  22,
		CurrencyUID:    curID,
		Amount:         decimal.NewFromInt(50),
		IdempotencyKey: postgrestest.UniqueKey("settle-oversettle"),
		ExpiresIn:      10 * time.Minute,
	})
	require.NoError(t, err)

	err = store.Settle(ctx, core.SettleInput{ReservationUID: res.UID, Amount: decimal.NewFromInt(50).Add(decimal.NewFromInt(1)), IdempotencyKey: postgrestest.UniqueKey("settle-oversettle-op")})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)
	assert.Contains(t, err.Error(), "exceeds reserved amount")

	// The reservation must remain active — a rejected settle must not
	// partially apply.
	got, err := store.HeldAmount(ctx, 22, curID)
	require.NoError(t, err)
	assert.True(t, got.Equal(decimal.NewFromInt(50)), "reservation should remain active with full hold, got %s", got)
}

// Settling for exactly the reserved amount (the boundary, not over) must
// still succeed.
func TestReserverStore_Settle_ExactReservedAmountAccepted(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger, postgres.NewVerifiedBalanceStore(pool, nil))
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	seedReservableBalance(t, ctx, ledger, pool, 23, curID, decimal.NewFromInt(100))

	res, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder:  23,
		CurrencyUID:    curID,
		Amount:         decimal.NewFromInt(50),
		IdempotencyKey: postgrestest.UniqueKey("settle-exact"),
		ExpiresIn:      10 * time.Minute,
	})
	require.NoError(t, err)

	err = store.Settle(ctx, core.SettleInput{ReservationUID: res.UID, Amount: decimal.NewFromInt(50), IdempotencyKey: postgrestest.UniqueKey("settle-exact-op")})
	require.NoError(t, err)

	status, settledAmount := reservationRow(t, ctx, pool, res.UID)
	assert.Equal(t, string(core.ReservationStatusSettled), status)
	assert.True(t, settledAmount.Equal(decimal.NewFromInt(50)), "want settled_amount=50, got %s", settledAmount)

	held, err := store.HeldAmount(ctx, 23, curID)
	require.NoError(t, err)
	assert.True(t, held.IsZero(), "fully settled reservation must drop out of held amount, got %s", held)
}

// Pins I-3 for Release: before ReleaseInput carried an IdempotencyKey,
// Release took a bare reservationUID and a lost-response retry of a
// successful call landed on ErrInvalidTransition (released is terminal),
// indistinguishable from a genuine conflict. A replay (same key) now
// returns the original success; the same key reused against a DIFFERENT
// reservation is ErrConflict.
func TestReserverStore_Release_IdempotentReplay(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger, postgres.NewVerifiedBalanceStore(pool, nil))
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	seedReservableBalance(t, ctx, ledger, pool, 24, curID, decimal.NewFromInt(100))

	res, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder:  24,
		CurrencyUID:    curID,
		Amount:         decimal.NewFromInt(50),
		IdempotencyKey: postgrestest.UniqueKey("release-idem-rsv"),
		ExpiresIn:      10 * time.Minute,
	})
	require.NoError(t, err)

	key := postgrestest.UniqueKey("release-idem-op")

	require.NoError(t, store.Release(ctx, core.ReleaseInput{ReservationUID: res.UID, IdempotencyKey: key}))

	// Lost-response retry: same key -- must succeed, not ErrInvalidTransition.
	require.NoError(t, store.Release(ctx, core.ReleaseInput{ReservationUID: res.UID, IdempotencyKey: key}))

	// The same key reused against a different reservation is a real
	// conflict, not a replay.
	res2, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder:  24,
		CurrencyUID:    curID,
		Amount:         decimal.NewFromInt(20),
		IdempotencyKey: postgrestest.UniqueKey("release-idem-rsv-2"),
		ExpiresIn:      10 * time.Minute,
	})
	require.NoError(t, err)
	err = store.Release(ctx, core.ReleaseInput{ReservationUID: res2.UID, IdempotencyKey: key})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrConflict)
}

// Pins I-3 for FinalizeSettlement, mirroring TestReserverStore_Settle_IdempotentReplay
// and TestReserverStore_Release_IdempotentReplay: before
// FinalizeSettlementInput carried an IdempotencyKey, FinalizeSettlement took
// a bare reservationUID and a lost-response retry of a successful call
// landed on ErrInvalidTransition (settled is terminal).
func TestReserverStore_FinalizeSettlement_IdempotentReplay(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger, postgres.NewVerifiedBalanceStore(pool, nil))
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	seedReservableBalance(t, ctx, ledger, pool, 25, curID, decimal.NewFromInt(100))

	res, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder:  25,
		CurrencyUID:    curID,
		Amount:         decimal.NewFromInt(50),
		IdempotencyKey: postgrestest.UniqueKey("finalize-idem-rsv"),
		ExpiresIn:      10 * time.Minute,
	})
	require.NoError(t, err)
	require.NoError(t, store.SettlePartial(ctx, core.SettlePartialInput{
		ReservationUID: res.UID,
		Amount:         decimal.NewFromInt(30),
		IdempotencyKey: postgrestest.UniqueKey("finalize-idem-leg"),
	}))

	key := postgrestest.UniqueKey("finalize-idem-op")
	require.NoError(t, store.FinalizeSettlement(ctx, core.FinalizeSettlementInput{ReservationUID: res.UID, IdempotencyKey: key}))

	// Lost-response retry: same key -- must succeed, not ErrInvalidTransition.
	require.NoError(t, store.FinalizeSettlement(ctx, core.FinalizeSettlementInput{ReservationUID: res.UID, IdempotencyKey: key}))

	// The same key reused against a different (settling) reservation is a
	// real conflict, not a replay.
	res2, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder:  25,
		CurrencyUID:    curID,
		Amount:         decimal.NewFromInt(10),
		IdempotencyKey: postgrestest.UniqueKey("finalize-idem-rsv-2"),
		ExpiresIn:      10 * time.Minute,
	})
	require.NoError(t, err)
	require.NoError(t, store.SettlePartial(ctx, core.SettlePartialInput{
		ReservationUID: res2.UID,
		Amount:         decimal.NewFromInt(5),
		IdempotencyKey: postgrestest.UniqueKey("finalize-idem-leg-2"),
	}))
	err = store.FinalizeSettlement(ctx, core.FinalizeSettlementInput{ReservationUID: res2.UID, IdempotencyKey: key})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrConflict)
}
