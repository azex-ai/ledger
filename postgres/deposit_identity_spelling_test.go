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

// TestBookingStore_DepositIdentitySpelling pins I-71's spelling half at the
// store boundary every booking-metadata writer passes: CreateBooking (HTTP
// POST /bookings and the library) and Transition's metadata merge. A second
// spelling of a real transfer's identity -- hash case, a 0X prefix, "01" or
// "+1" -- slipped past uq_bookings_deposit_identity (raw text) and the
// sibling lookup, while the chain re-read accepted it as the same log, so
// the same deposit could be credited again.
func TestBookingStore_DepositIdentitySpelling(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	bookingStore := postgres.NewBookingStore(pool)
	cls, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code:       "deposit_spelling_test",
		Name:       "Deposit Spelling Test",
		NormalSide: core.NormalSideDebit,
		IsSystem:   true,
		Lifecycle: &core.Lifecycle{
			Initial:     "pending",
			Terminal:    []core.Status{"confirmed"},
			Transitions: map[core.Status][]core.Status{"pending": {"confirming"}, "confirming": {"confirmed"}},
		},
	})
	require.NoError(t, err)
	curUID := postgrestest.SeedCurrency(t, pool, "USDS", "Spelling USD")

	const h = "0xabababababababababababababababababababababababababababababababab"
	identity := func(tx, seq, chain string) map[string]string {
		return map[string]string{"chain_id": chain, "tx_hash": tx, "txlog_seq": seq, "block_number": "100", "token": "0xt"}
	}
	create := func(key string, md map[string]string) (*core.Booking, error) {
		return bookingStore.CreateBooking(ctx, core.CreateBookingInput{
			ClassificationCode: cls.Code,
			AccountHolder:      7,
			CurrencyUID:        curUID,
			Amount:             decimal.NewFromInt(50),
			IdempotencyKey:     postgrestest.UniqueKey(key),
			ChannelName:        "manual-" + key,
			Metadata:           md,
		})
	}

	variants := map[string][2]string{ // name -> {tx_hash, txlog_seq}; chain_id "1" unless noted
		"upper-case hash": {"0xABABABABABABABABABABABABABABABABABABABABABABABABABABABABABABABAB", "1"},
		"mixed-case hash": {"0xAb" + h[4:], "1"},
		"0X prefix":       {"0X" + h[2:], "1"},
		"leading zero":    {h, "01"},
		"plus sign":       {h, "+1"},
	}

	t.Run("create refuses non-canonical spellings", func(t *testing.T) {
		for name, v := range variants {
			_, err := create("create-"+name, identity(v[0], v[1], "1"))
			assert.ErrorIs(t, err, core.ErrInvalidInput, name)
		}
		_, err := create("create-chain01", identity(h, "1", "01"))
		assert.ErrorIs(t, err, core.ErrInvalidInput, "chain_id 01")
		_, err = create("create-chainplus", identity(h, "1", "+1"))
		assert.ErrorIs(t, err, core.ErrInvalidInput, "chain_id +1")
	})

	t.Run("canonical accepted", func(t *testing.T) {
		b, err := create("canonical", identity(h, "1", "1"))
		require.NoError(t, err)
		assert.Equal(t, h, b.Metadata["tx_hash"])
	})

	t.Run("transition merge refuses non-canonical spellings", func(t *testing.T) {
		// A booking created canonically for a different log, then respelled
		// by the merge: the merged identity is what gets stored.
		const other = "0xcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
		b, err := create("transition-base", identity(other, "0", "1"))
		require.NoError(t, err)

		for name, v := range variants {
			_, err := bookingStore.Transition(ctx, core.TransitionInput{
				BookingUID:     b.UID,
				ToStatus:       "confirming",
				IdempotencyKey: postgrestest.UniqueKey("transition-" + name),
				Metadata:       map[string]string{"tx_hash": v[0], "txlog_seq": v[1]},
			})
			assert.ErrorIs(t, err, core.ErrInvalidInput, name)
		}
		_, err = bookingStore.Transition(ctx, core.TransitionInput{
			BookingUID:     b.UID,
			ToStatus:       "confirming",
			IdempotencyKey: postgrestest.UniqueKey("transition-chain01"),
			Metadata:       map[string]string{"chain_id": "01"},
		})
		assert.ErrorIs(t, err, core.ErrInvalidInput, "chain_id 01")

		got, err := bookingStore.GetBooking(ctx, b.UID)
		require.NoError(t, err)
		assert.Equal(t, core.Status("pending"), got.Status, "every refused transition left the booking untouched")
		assert.Equal(t, other, got.Metadata["tx_hash"])

		// Canonical merge still goes through.
		_, err = bookingStore.Transition(ctx, core.TransitionInput{
			BookingUID:     b.UID,
			ToStatus:       "confirming",
			IdempotencyKey: postgrestest.UniqueKey("transition-canonical"),
			Metadata:       map[string]string{"tx_hash": other, "txlog_seq": "0"},
		})
		require.NoError(t, err)
	})
}
