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

// Regression test: existing.Metadata is Postgres's raw jsonb column value,
// which the server re-serializes in its own canonical form (keys ordered by
// length-then-lexicographic, ": " with a space) -- never byte-identical to
// Go's encoding/json compact output, even when both sides hold identical
// key/value pairs. A naive string compare in ensureBookingMatchesInput would
// spuriously ErrConflict every genuine idempotent CreateBooking replay whose
// metadata has more than one key (discovered via crypto-deposit's
// IngestDeposit dual-path idempotency test, which always sets >=2 metadata
// keys — see docs/plans/2026-07-11-crypto-deposit-sweep-design.md §3).
func TestBookingStore_CreateBooking_IdempotentReplay_MultiKeyMetadata(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	code := postgrestest.UniqueKey("meta-replay-class")
	classUID := postgrestest.SeedClassification(t, pool, code, "Meta Replay", "credit", false)
	_, err := pool.Exec(ctx, "UPDATE classifications SET lifecycle = $1 WHERE uid = $2::uuid",
		[]byte(`{"initial":"pending","terminal":["confirmed"],"transitions":{"pending":["confirmed"]}}`), classUID)
	require.NoError(t, err)

	curUID := postgrestest.SeedCurrency(t, pool, postgrestest.UniqueKey("USDT"), "Tether USD")
	bookingStore := postgres.NewBookingStore(pool)

	input := core.CreateBookingInput{
		ClassificationCode: code,
		AccountHolder:      12345,
		CurrencyUID:        curUID,
		Amount:             decimal.RequireFromString("100"),
		IdempotencyKey:     postgrestest.UniqueKey("meta-replay"),
		ChannelName:        "onchain",
		Metadata: map[string]string{
			"chain_id":     "1",
			"tx_hash":      "0xabc",
			"token":        "0xusdt",
			"block_number": "100",
		},
	}

	first, err := bookingStore.CreateBooking(ctx, input)
	require.NoError(t, err)

	// The exact same call must be a pure idempotent no-op -- NOT ErrConflict.
	second, err := bookingStore.CreateBooking(ctx, input)
	require.NoError(t, err)
	assert.Equal(t, first.UID, second.UID)
	assert.Equal(t, input.Metadata, second.Metadata)

	// A genuinely different metadata value under the SAME key must still be
	// correctly detected as a conflict (the fix must not make the comparison
	// vacuously true).
	mutated := input
	mutated.Metadata = map[string]string{
		"chain_id":     "1",
		"tx_hash":      "0xabc",
		"token":        "0xDIFFERENT",
		"block_number": "100",
	}
	_, err = bookingStore.CreateBooking(ctx, mutated)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrConflict)
}
