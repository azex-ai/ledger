package postgres_test

import (
	"context"
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
	store := postgres.NewReserverStore(pool, ledger)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	seedReservableBalance(t, ctx, ledger, pool, 1, curID, decimal.NewFromInt(100))

	res, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder:  1,
		CurrencyID:     curID,
		Amount:         decimal.NewFromInt(100),
		IdempotencyKey: postgrestest.UniqueKey("res-settle"),
		ExpiresIn:      10 * time.Minute,
	})
	require.NoError(t, err)
	assert.Equal(t, core.ReservationStatusActive, res.Status)
	assert.True(t, res.ReservedAmount.Equal(decimal.NewFromInt(100)))

	// Settle
	err = store.Settle(ctx, res.ID, decimal.NewFromInt(95))
	require.NoError(t, err)
}

func TestReserverStore_Reserve_Release(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	seedReservableBalance(t, ctx, ledger, pool, 2, curID, decimal.NewFromInt(50))

	res, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder:  2,
		CurrencyID:     curID,
		Amount:         decimal.NewFromInt(50),
		IdempotencyKey: postgrestest.UniqueKey("res-release"),
		ExpiresIn:      5 * time.Minute,
	})
	require.NoError(t, err)

	err = store.Release(ctx, res.ID)
	require.NoError(t, err)

	// Cannot settle after release
	err = store.Settle(ctx, res.ID, decimal.NewFromInt(50))
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidTransition)
}

func TestReserverStore_Reserve_Idempotent(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	seedReservableBalance(t, ctx, ledger, pool, 3, curID, decimal.NewFromInt(100))

	key := postgrestest.UniqueKey("res-idem")
	input := core.ReserveInput{
		AccountHolder:  3,
		CurrencyID:     curID,
		Amount:         decimal.NewFromInt(100),
		IdempotencyKey: key,
		ExpiresIn:      10 * time.Minute,
	}

	r1, err := store.Reserve(ctx, input)
	require.NoError(t, err)

	r2, err := store.Reserve(ctx, input)
	require.NoError(t, err)
	assert.Equal(t, r1.ID, r2.ID)
}

func TestReserverStore_Reserve_IdempotentPayloadMismatch(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT-RES-IDEM", "Tether USD")
	seedReservableBalance(t, ctx, ledger, pool, 31, curID, decimal.NewFromInt(100))

	key := postgrestest.UniqueKey("res-idem-mismatch")
	_, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder:  31,
		CurrencyID:     curID,
		Amount:         decimal.NewFromInt(40),
		IdempotencyKey: key,
		ExpiresIn:      10 * time.Minute,
	})
	require.NoError(t, err)

	_, err = store.Reserve(ctx, core.ReserveInput{
		AccountHolder:  31,
		CurrencyID:     curID,
		Amount:         decimal.NewFromInt(50),
		IdempotencyKey: key,
		ExpiresIn:      10 * time.Minute,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrConflict)
}

func TestReserverStore_Reserve_Concurrent(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger)
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
			CurrencyID:     curID,
			Amount:         decimal.NewFromInt(50),
			IdempotencyKey: postgrestest.UniqueKey("conc-a"),
			ExpiresIn:      10 * time.Minute,
		})
	}()
	go func() {
		defer wg.Done()
		res2, err2 = store.Reserve(ctx, core.ReserveInput{
			AccountHolder:  10,
			CurrencyID:     curID,
			Amount:         decimal.NewFromInt(30),
			IdempotencyKey: postgrestest.UniqueKey("conc-b"),
			ExpiresIn:      10 * time.Minute,
		})
	}()
	wg.Wait()

	require.NoError(t, err1)
	require.NoError(t, err2)
	assert.NotEqual(t, res1.ID, res2.ID)
}

func TestReserverStore_Settle_InvalidTransition(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	seedReservableBalance(t, ctx, ledger, pool, 5, curID, decimal.NewFromInt(100))

	res, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder:  5,
		CurrencyID:     curID,
		Amount:         decimal.NewFromInt(100),
		IdempotencyKey: postgrestest.UniqueKey("double-settle"),
		ExpiresIn:      10 * time.Minute,
	})
	require.NoError(t, err)

	// Settle once
	err = store.Settle(ctx, res.ID, decimal.NewFromInt(100))
	require.NoError(t, err)

	// Settle again should fail
	err = store.Settle(ctx, res.ID, decimal.NewFromInt(100))
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidTransition)
}

func TestReserverStore_HeldAmount(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	seedReservableBalance(t, ctx, ledger, pool, 7, curID, decimal.NewFromInt(100))

	held, err := store.HeldAmount(ctx, 7, curID)
	require.NoError(t, err)
	assert.True(t, held.IsZero(), "no reservations yet, held should be 0, got %s", held)

	r1, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder: 7, CurrencyID: curID, Amount: decimal.NewFromInt(30),
		IdempotencyKey: postgrestest.UniqueKey("held-a"), ExpiresIn: 10 * time.Minute,
	})
	require.NoError(t, err)

	_, err = store.Reserve(ctx, core.ReserveInput{
		AccountHolder: 7, CurrencyID: curID, Amount: decimal.NewFromInt(20),
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
	require.NoError(t, store.Release(ctx, r1.ID))
	held, err = store.HeldAmount(ctx, 7, curID)
	require.NoError(t, err)
	assert.True(t, held.Equal(decimal.NewFromInt(20)), "after release, want 20, got %s", held)
}

func seedReservableBalance(t *testing.T, ctx context.Context, ledger *postgres.LedgerStore, pool *pgxpool.Pool, holder, currencyID int64, amount decimal.Decimal) {
	t.Helper()

	journalTypeID := postgrestest.SeedJournalType(t, pool, "fund_account", "Fund Account")
	walletID := postgrestest.SeedClassification(t, pool, "main_wallet", "Main Wallet", "debit", false)
	custodialID := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)

	_, err := ledger.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  journalTypeID,
		IdempotencyKey: postgrestest.UniqueKey("seed-reserve-balance"),
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyID: currencyID, ClassificationID: walletID, EntryType: core.EntryTypeDebit, Amount: amount},
			{AccountHolder: -holder, CurrencyID: currencyID, ClassificationID: custodialID, EntryType: core.EntryTypeCredit, Amount: amount},
		},
		Source: "test",
	})
	require.NoError(t, err)
}

func TestReserverStore_Settle_ZeroAmountRejected(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	seedReservableBalance(t, ctx, ledger, pool, 20, curID, decimal.NewFromInt(100))

	res, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder:  20,
		CurrencyID:     curID,
		Amount:         decimal.NewFromInt(100),
		IdempotencyKey: postgrestest.UniqueKey("settle-zero"),
		ExpiresIn:      10 * time.Minute,
	})
	require.NoError(t, err)

	err = store.Settle(ctx, res.ID, decimal.Zero)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)
}

func TestReserverStore_Settle_NegativeAmountRejected(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	seedReservableBalance(t, ctx, ledger, pool, 21, curID, decimal.NewFromInt(100))

	res, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder:  21,
		CurrencyID:     curID,
		Amount:         decimal.NewFromInt(100),
		IdempotencyKey: postgrestest.UniqueKey("settle-negative"),
		ExpiresIn:      10 * time.Minute,
	})
	require.NoError(t, err)

	err = store.Settle(ctx, res.ID, decimal.NewFromInt(-1))
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
	store := postgres.NewReserverStore(pool, ledger)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	seedReservableBalance(t, ctx, ledger, pool, 22, curID, decimal.NewFromInt(100))

	res, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder:  22,
		CurrencyID:     curID,
		Amount:         decimal.NewFromInt(50),
		IdempotencyKey: postgrestest.UniqueKey("settle-oversettle"),
		ExpiresIn:      10 * time.Minute,
	})
	require.NoError(t, err)

	err = store.Settle(ctx, res.ID, decimal.NewFromInt(50).Add(decimal.NewFromInt(1)))
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
	store := postgres.NewReserverStore(pool, ledger)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	seedReservableBalance(t, ctx, ledger, pool, 23, curID, decimal.NewFromInt(100))

	res, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder:  23,
		CurrencyID:     curID,
		Amount:         decimal.NewFromInt(50),
		IdempotencyKey: postgrestest.UniqueKey("settle-exact"),
		ExpiresIn:      10 * time.Minute,
	})
	require.NoError(t, err)

	err = store.Settle(ctx, res.ID, decimal.NewFromInt(50))
	require.NoError(t, err)
}
