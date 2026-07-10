package postgres_test

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

func TestIngestDeadLetterStore_RecordAndList(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	store := postgres.NewIngestDeadLetterStore(pool)

	sighting := core.DepositSighting{
		ChainID:  1,
		TxHash:   "0xdeadletter1",
		TxLogSeq: 0,
		Token:    "0xtoken",
		From:     "0xfrom",
		To:       "0xto",
		Amount:   decimal.RequireFromString("100"),
	}
	key := postgrestest.UniqueKey("deposit-1-0xdeadletter1-0")

	require.NoError(t, store.RecordDeadLetter(ctx, sighting, key, "amount mismatch"))

	letters, err := store.ListDeadLetters(ctx, 10)
	require.NoError(t, err)
	require.NotEmpty(t, letters)
	assert.Equal(t, key, letters[0].IdempotencyKey)
	assert.Equal(t, "amount mismatch", letters[0].Reason)
	assert.Equal(t, "0xdeadletter1", letters[0].TxHash)
}

func TestIngestDeadLetterStore_RecordDeadLetter_IdempotentOnKey(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	store := postgres.NewIngestDeadLetterStore(pool)

	sighting := core.DepositSighting{
		ChainID:  1,
		TxHash:   "0xdeadletter2",
		TxLogSeq: 0,
		Token:    "0xtoken",
		From:     "0xfrom",
		To:       "0xto",
		Amount:   decimal.RequireFromString("100"),
	}
	key := postgrestest.UniqueKey("deposit-1-0xdeadletter2-0")

	// Repeated conflicts on the same sighting (e.g. the watcher retrying
	// every scan) must not spam the table -- ON CONFLICT DO NOTHING.
	require.NoError(t, store.RecordDeadLetter(ctx, sighting, key, "amount mismatch"))
	require.NoError(t, store.RecordDeadLetter(ctx, sighting, key, "amount mismatch"))
	require.NoError(t, store.RecordDeadLetter(ctx, sighting, key, "amount mismatch"))

	letters, err := store.ListDeadLetters(ctx, 1000)
	require.NoError(t, err)
	count := 0
	for _, l := range letters {
		if l.IdempotencyKey == key {
			count++
		}
	}
	assert.Equal(t, 1, count)
}
