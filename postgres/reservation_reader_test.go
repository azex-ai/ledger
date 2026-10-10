package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

func TestReservationReader_GetAndTransactionVisibility(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	store := postgres.NewReserverStore(pool, ledger, postgres.NewVerifiedBalanceStore(pool, nil))
	var reader core.ReservationReader = store
	cur := postgrestest.SeedCurrency(t, pool, "READ", "Reservation Reader")
	seedReservableBalance(t, ctx, ledger, pool, 12001, cur, decimal.NewFromInt(100))

	for _, uid := range []string{"", "not-a-uid", "11111111-1111-4111-8111-111111111111"} {
		_, err := reader.GetReservation(ctx, uid)
		require.ErrorIs(t, err, core.ErrNotFound)
	}
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	bound := store.WithDB(tx, ledger.WithDB(tx))
	created, err := bound.Reserve(ctx, core.ReserveInput{
		AccountHolder: 12001, CurrencyUID: cur, Amount: decimal.NewFromInt(30),
		IdempotencyKey: "reader-reserve", ExpiresIn: time.Minute,
	})
	require.NoError(t, err)
	got, err := bound.GetReservation(ctx, created.UID)
	require.NoError(t, err)
	require.Equal(t, created.UID, got.UID)
	require.Equal(t, created.AccountHolder, got.AccountHolder)
	require.Equal(t, cur, got.CurrencyUID)
	require.True(t, got.ReservedAmount.Equal(decimal.NewFromInt(30)))
	require.Equal(t, core.ReservationStatusActive, got.Status)
	_, err = reader.GetReservation(ctx, created.UID)
	require.ErrorIs(t, err, core.ErrNotFound, "pool must not see caller's uncommitted reservation")
	require.NoError(t, tx.Commit(ctx))
	got, err = reader.GetReservation(ctx, created.UID)
	require.NoError(t, err)
	require.Equal(t, created.IdempotencyKey, got.IdempotencyKey)
	require.Equal(t, created.ExpiresAt, got.ExpiresAt)

	require.NoError(t, store.SettlePartial(ctx, core.SettlePartialInput{
		ReservationUID: created.UID, Amount: decimal.NewFromInt(10), IdempotencyKey: "reader-partial",
	}))
	got, err = reader.GetReservation(ctx, created.UID)
	require.NoError(t, err)
	require.Equal(t, core.ReservationStatusSettling, got.Status)
	require.NotNil(t, got.SettledAmount)
	require.True(t, got.SettledAmount.Equal(decimal.NewFromInt(10)))
}
