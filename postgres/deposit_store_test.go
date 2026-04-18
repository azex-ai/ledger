package postgres_test

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres"
)

func TestDepositStore_HappyPath(t *testing.T) {
	pool := setupTestDB(t)
	store := postgres.NewDepositStore(pool)
	ctx := context.Background()

	seedCurrency(t, pool, "USDT", "Tether USD")

	// Init
	dep, err := store.InitDeposit(ctx, core.DepositInput{
		AccountHolder:  1,
		CurrencyID:     1,
		ExpectedAmount: decimal.NewFromInt(100),
		ChannelName:    "evm",
		IdempotencyKey: uniqueKey("dep-happy"),
	})
	require.NoError(t, err)
	assert.Equal(t, core.DepositStatusPending, dep.Status)

	// Confirming
	err = store.ConfirmingDeposit(ctx, dep.ID, "0xabc123")
	require.NoError(t, err)

	// Confirm
	err = store.ConfirmDeposit(ctx, core.ConfirmDepositInput{
		DepositID:    dep.ID,
		ActualAmount: decimal.NewFromInt(100),
		ChannelRef:   "0xabc123",
	})
	require.NoError(t, err)
}

func TestDepositStore_AmountMismatch(t *testing.T) {
	pool := setupTestDB(t)
	store := postgres.NewDepositStore(pool)
	ctx := context.Background()

	seedCurrency(t, pool, "USDT", "Tether USD")

	dep, err := store.InitDeposit(ctx, core.DepositInput{
		AccountHolder:  2,
		CurrencyID:     1,
		ExpectedAmount: decimal.NewFromInt(100),
		ChannelName:    "evm",
		IdempotencyKey: uniqueKey("dep-mismatch"),
	})
	require.NoError(t, err)

	err = store.ConfirmingDeposit(ctx, dep.ID, "0xdef456")
	require.NoError(t, err)

	// Confirm with different actual amount (95 instead of 100)
	err = store.ConfirmDeposit(ctx, core.ConfirmDepositInput{
		DepositID:    dep.ID,
		ActualAmount: decimal.NewFromInt(95),
		ChannelRef:   "0xdef456",
	})
	require.NoError(t, err) // Store layer accepts it; suspense adjustment is service layer concern
}

func TestDepositStore_ConfirmIdempotent(t *testing.T) {
	pool := setupTestDB(t)
	store := postgres.NewDepositStore(pool)
	ctx := context.Background()

	seedCurrency(t, pool, "USDT", "Tether USD")

	dep, err := store.InitDeposit(ctx, core.DepositInput{
		AccountHolder:  3,
		CurrencyID:     1,
		ExpectedAmount: decimal.NewFromInt(100),
		ChannelName:    "evm",
		IdempotencyKey: uniqueKey("dep-idem"),
	})
	require.NoError(t, err)

	err = store.ConfirmingDeposit(ctx, dep.ID, "0x111")
	require.NoError(t, err)

	// First confirm
	err = store.ConfirmDeposit(ctx, core.ConfirmDepositInput{
		DepositID:    dep.ID,
		ActualAmount: decimal.NewFromInt(100),
		ChannelRef:   "0x111",
	})
	require.NoError(t, err)

	// Second confirm — idempotent, should succeed
	err = store.ConfirmDeposit(ctx, core.ConfirmDepositInput{
		DepositID:    dep.ID,
		ActualAmount: decimal.NewFromInt(100),
		ChannelRef:   "0x111",
	})
	require.NoError(t, err)
}

func TestDepositStore_StateMachineEnforcement(t *testing.T) {
	pool := setupTestDB(t)
	store := postgres.NewDepositStore(pool)
	ctx := context.Background()

	seedCurrency(t, pool, "USDT", "Tether USD")

	dep, err := store.InitDeposit(ctx, core.DepositInput{
		AccountHolder:  4,
		CurrencyID:     1,
		ExpectedAmount: decimal.NewFromInt(100),
		ChannelName:    "evm",
		IdempotencyKey: uniqueKey("dep-state"),
	})
	require.NoError(t, err)

	// Confirm and then try to go back to pending
	err = store.ConfirmingDeposit(ctx, dep.ID, "0xfoo")
	require.NoError(t, err)

	err = store.ConfirmDeposit(ctx, core.ConfirmDepositInput{
		DepositID:    dep.ID,
		ActualAmount: decimal.NewFromInt(100),
		ChannelRef:   "0xfoo",
	})
	require.NoError(t, err)

	// Cannot fail an already confirmed deposit
	err = store.FailDeposit(ctx, dep.ID, "test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid transition")
}

func TestDepositStore_Fail(t *testing.T) {
	pool := setupTestDB(t)
	store := postgres.NewDepositStore(pool)
	ctx := context.Background()

	seedCurrency(t, pool, "USDT", "Tether USD")

	dep, err := store.InitDeposit(ctx, core.DepositInput{
		AccountHolder:  5,
		CurrencyID:     1,
		ExpectedAmount: decimal.NewFromInt(100),
		ChannelName:    "evm",
		IdempotencyKey: uniqueKey("dep-fail"),
	})
	require.NoError(t, err)

	err = store.FailDeposit(ctx, dep.ID, "invalid tx")
	require.NoError(t, err)
}

func TestDepositStore_Expire(t *testing.T) {
	pool := setupTestDB(t)
	store := postgres.NewDepositStore(pool)
	ctx := context.Background()

	seedCurrency(t, pool, "USDT", "Tether USD")

	dep, err := store.InitDeposit(ctx, core.DepositInput{
		AccountHolder:  6,
		CurrencyID:     1,
		ExpectedAmount: decimal.NewFromInt(100),
		ChannelName:    "evm",
		IdempotencyKey: uniqueKey("dep-expire"),
	})
	require.NoError(t, err)

	err = store.ExpireDeposit(ctx, dep.ID)
	require.NoError(t, err)
}
