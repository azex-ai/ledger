package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres"
)

func TestReserverStore_Reserve_Settle(t *testing.T) {
	pool := setupTestDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger)
	ctx := context.Background()

	seedCurrency(t, pool, "USDT", "Tether USD")

	res, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder:  1,
		CurrencyID:     1,
		Amount:         decimal.NewFromInt(100),
		IdempotencyKey: uniqueKey("res-settle"),
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
	pool := setupTestDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger)
	ctx := context.Background()

	seedCurrency(t, pool, "USDT", "Tether USD")

	res, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder:  2,
		CurrencyID:     1,
		Amount:         decimal.NewFromInt(50),
		IdempotencyKey: uniqueKey("res-release"),
		ExpiresIn:      5 * time.Minute,
	})
	require.NoError(t, err)

	err = store.Release(ctx, res.ID)
	require.NoError(t, err)

	// Cannot settle after release
	err = store.Settle(ctx, res.ID, decimal.NewFromInt(50))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid transition")
}

func TestReserverStore_Reserve_Idempotent(t *testing.T) {
	pool := setupTestDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger)
	ctx := context.Background()

	seedCurrency(t, pool, "USDT", "Tether USD")

	key := uniqueKey("res-idem")
	input := core.ReserveInput{
		AccountHolder:  3,
		CurrencyID:     1,
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

func TestReserverStore_Reserve_Concurrent(t *testing.T) {
	pool := setupTestDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger)
	ctx := context.Background()

	seedCurrency(t, pool, "USDT", "Tether USD")

	// Both should succeed (advisory lock serializes)
	var wg sync.WaitGroup
	var res1, res2 *core.Reservation
	var err1, err2 error

	wg.Add(2)
	go func() {
		defer wg.Done()
		res1, err1 = store.Reserve(ctx, core.ReserveInput{
			AccountHolder:  10,
			CurrencyID:     1,
			Amount:         decimal.NewFromInt(50),
			IdempotencyKey: uniqueKey("conc-a"),
			ExpiresIn:      10 * time.Minute,
		})
	}()
	go func() {
		defer wg.Done()
		res2, err2 = store.Reserve(ctx, core.ReserveInput{
			AccountHolder:  10,
			CurrencyID:     1,
			Amount:         decimal.NewFromInt(30),
			IdempotencyKey: uniqueKey("conc-b"),
			ExpiresIn:      10 * time.Minute,
		})
	}()
	wg.Wait()

	require.NoError(t, err1)
	require.NoError(t, err2)
	assert.NotEqual(t, res1.ID, res2.ID)
}

func TestReserverStore_Settle_InvalidTransition(t *testing.T) {
	pool := setupTestDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger)
	ctx := context.Background()

	seedCurrency(t, pool, "USDT", "Tether USD")

	res, err := store.Reserve(ctx, core.ReserveInput{
		AccountHolder:  5,
		CurrencyID:     1,
		Amount:         decimal.NewFromInt(100),
		IdempotencyKey: uniqueKey("double-settle"),
		ExpiresIn:      10 * time.Minute,
	})
	require.NoError(t, err)

	// Settle once
	err = store.Settle(ctx, res.ID, decimal.NewFromInt(100))
	require.NoError(t, err)

	// Settle again should fail
	err = store.Settle(ctx, res.ID, decimal.NewFromInt(100))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid transition")
}
