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
