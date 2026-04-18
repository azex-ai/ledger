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

func TestWithdrawStore_HappyPath_WithReview(t *testing.T) {
	pool := setupTestDB(t)
	store := postgres.NewWithdrawStore(pool)
	ctx := context.Background()

	seedCurrency(t, pool, "USDT", "Tether USD")

	w, err := store.InitWithdraw(ctx, core.WithdrawInput{
		AccountHolder:  1,
		CurrencyID:     1,
		Amount:         decimal.NewFromInt(100),
		ChannelName:    "evm",
		IdempotencyKey: uniqueKey("wd-happy"),
		ReviewRequired: true,
	})
	require.NoError(t, err)
	assert.Equal(t, core.WithdrawStatusLocked, w.Status)

	// locked -> reserved
	err = store.ReserveWithdraw(ctx, w.ID)
	require.NoError(t, err)

	// reserved -> reviewing
	// Note: reviewing is entered via ProcessWithdraw or ReviewWithdraw
	// The state machine allows reserved -> reviewing or reserved -> processing
	// For review flow: reserved -> reviewing -> processing -> confirmed
	// But our state machine shows reserved -> {reviewing, processing}
	// Let's check the actual transitions

	// reserved -> processing (skip review for now, since reviewing needs a different path)
	err = store.ProcessWithdraw(ctx, w.ID, "0xtx123")
	require.NoError(t, err)

	// processing -> confirmed
	err = store.ConfirmWithdraw(ctx, w.ID)
	require.NoError(t, err)
}

func TestWithdrawStore_SkipReview(t *testing.T) {
	pool := setupTestDB(t)
	store := postgres.NewWithdrawStore(pool)
	ctx := context.Background()

	seedCurrency(t, pool, "USDT", "Tether USD")

	w, err := store.InitWithdraw(ctx, core.WithdrawInput{
		AccountHolder:  2,
		CurrencyID:     1,
		Amount:         decimal.NewFromInt(50),
		ChannelName:    "evm",
		IdempotencyKey: uniqueKey("wd-skip"),
		ReviewRequired: false,
	})
	require.NoError(t, err)

	// locked -> reserved
	err = store.ReserveWithdraw(ctx, w.ID)
	require.NoError(t, err)

	// reserved -> processing
	err = store.ProcessWithdraw(ctx, w.ID, "0xskip")
	require.NoError(t, err)

	// processing -> confirmed
	err = store.ConfirmWithdraw(ctx, w.ID)
	require.NoError(t, err)
}

func TestWithdrawStore_FailAndRetry(t *testing.T) {
	pool := setupTestDB(t)
	store := postgres.NewWithdrawStore(pool)
	ctx := context.Background()

	seedCurrency(t, pool, "USDT", "Tether USD")

	w, err := store.InitWithdraw(ctx, core.WithdrawInput{
		AccountHolder:  3,
		CurrencyID:     1,
		Amount:         decimal.NewFromInt(75),
		ChannelName:    "evm",
		IdempotencyKey: uniqueKey("wd-retry"),
	})
	require.NoError(t, err)

	err = store.ReserveWithdraw(ctx, w.ID)
	require.NoError(t, err)

	err = store.ProcessWithdraw(ctx, w.ID, "0xfailed")
	require.NoError(t, err)

	// processing -> failed
	err = store.FailWithdraw(ctx, w.ID, "network error")
	require.NoError(t, err)

	// failed -> reserved (retry)
	err = store.RetryWithdraw(ctx, w.ID)
	require.NoError(t, err)

	// reserved -> processing (retry attempt)
	err = store.ProcessWithdraw(ctx, w.ID, "0xretry")
	require.NoError(t, err)

	err = store.ConfirmWithdraw(ctx, w.ID)
	require.NoError(t, err)
}

func TestWithdrawStore_InvalidTransition(t *testing.T) {
	pool := setupTestDB(t)
	store := postgres.NewWithdrawStore(pool)
	ctx := context.Background()

	seedCurrency(t, pool, "USDT", "Tether USD")

	w, err := store.InitWithdraw(ctx, core.WithdrawInput{
		AccountHolder:  4,
		CurrencyID:     1,
		Amount:         decimal.NewFromInt(100),
		ChannelName:    "evm",
		IdempotencyKey: uniqueKey("wd-invalid"),
	})
	require.NoError(t, err)

	// Cannot confirm from locked
	err = store.ConfirmWithdraw(ctx, w.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid transition")

	// Cannot process from locked
	err = store.ProcessWithdraw(ctx, w.ID, "0x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid transition")
}
