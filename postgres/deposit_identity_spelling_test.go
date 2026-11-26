package postgres_test

import (
	"context"
	"fmt"
	"strings"
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
// "+1" -- used to slip past uq_bookings_deposit_identity (raw text) and the
// sibling lookup, while the chain re-read accepted it as the same log, so
// the same deposit could be credited again. The store now writes one
// spelling: such variants are normalized before the idempotency comparison
// and the write, so they collide with the index; values that are not a
// valid identity are refused.
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

	identity := func(tx, seq, chain string) map[string]string {
		return map[string]string{"chain_id": chain, "tx_hash": tx, "txlog_seq": seq, "block_number": "100", "token": "0xt"}
	}
	// createKey uses key verbatim so a retry can repeat it.
	createKey := func(key string, md map[string]string) (*core.Booking, error) {
		return bookingStore.CreateBooking(ctx, core.CreateBookingInput{
			ClassificationCode: cls.Code,
			AccountHolder:      7,
			CurrencyUID:        curUID,
			Amount:             decimal.NewFromInt(50),
			IdempotencyKey:     key,
			ChannelName:        "manual",
			Metadata:           md,
		})
	}
	create := func(key string, md map[string]string) (*core.Booking, error) {
		return createKey(postgrestest.UniqueKey(key), md)
	}
	// hashN is a distinct canonical hash per n; the leading "a" makes case
	// matter for every n.
	hashN := func(n int) string { return fmt.Sprintf("0xa%063x", n) }

	// Each variant respells one canonical identity {chain "1", hash, seq "1"}.
	variants := map[string]func(tx string) (string, string, string){ // -> tx, seq, chain
		"upper-case hash":    func(tx string) (string, string, string) { return "0x" + strings.ToUpper(tx[2:]), "1", "1" },
		"mixed-case hash":    func(tx string) (string, string, string) { return "0xA" + tx[3:], "1", "1" },
		"0X prefix":          func(tx string) (string, string, string) { return "0X" + tx[2:], "1", "1" },
		"leading zero seq":   func(tx string) (string, string, string) { return tx, "01", "1" },
		"plus seq":           func(tx string) (string, string, string) { return tx, "+1", "1" },
		"leading zero chain": func(tx string) (string, string, string) { return tx, "1", "01" },
		"plus chain":         func(tx string) (string, string, string) { return tx, "1", "+1" },
	}

	t.Run("create normalizes, retries resolve, respellings collide", func(t *testing.T) {
		n := 0
		for name, spell := range variants {
			n++
			h := hashN(n)
			tx, seq, chain := spell(h)
			in := identity(tx, seq, chain)
			key := postgrestest.UniqueKey("create-" + name)

			b, err := createKey(key, in)
			require.NoError(t, err, name)
			assert.Equal(t, identity(h, "1", "1"), b.Metadata, "%s: stored canonical", name)
			assert.Equal(t, tx, in["tx_hash"], "%s: caller's map untouched", name)

			again, err := createKey(key, identity(tx, seq, chain))
			require.NoError(t, err, "%s: idempotent retry in the same spelling", name)
			assert.Equal(t, b.UID, again.UID, name)

			_, err = create("create-dup-"+name, identity(tx, seq, chain))
			assert.ErrorIs(t, err, core.ErrConflict, "%s: same spelling, new key", name)
			_, err = create("create-canon-"+name, identity(h, "1", "1"))
			assert.ErrorIs(t, err, core.ErrConflict, "%s: canonical spelling, new key", name)
		}
	})

	t.Run("create refuses invalid values", func(t *testing.T) {
		h := hashN(100)
		for name, md := range map[string]map[string]string{
			"no prefix":         identity(h[2:], "1", "1"),
			"non-numeric chain": identity(h, "1", "ethereum"),
			"non-numeric seq":   identity(h, "one", "1"),
		} {
			_, err := create("create-invalid-"+name, md)
			assert.ErrorIs(t, err, core.ErrInvalidInput, name)
		}
	})

	t.Run("transition merge normalizes", func(t *testing.T) {
		n := 200
		for name, spell := range variants {
			n++
			b, err := create("transition-base-"+name, identity(hashN(n), "0", "1"))
			require.NoError(t, err, name)

			n++
			h := hashN(n)
			tx, seq, chain := spell(h)
			_, err = bookingStore.Transition(ctx, core.TransitionInput{
				BookingUID:     b.UID,
				ToStatus:       "confirming",
				IdempotencyKey: postgrestest.UniqueKey("transition-" + name),
				Metadata:       map[string]string{"tx_hash": tx, "txlog_seq": seq, "chain_id": chain},
			})
			require.NoError(t, err, name)
			got, err := bookingStore.GetBooking(ctx, b.UID)
			require.NoError(t, err)
			assert.Equal(t, identity(h, "1", "1"), got.Metadata, "%s: merged identity stored canonical", name)
		}
	})

	t.Run("transition respelling an existing identity collides", func(t *testing.T) {
		held := hashN(300)
		_, err := create("transition-held", identity(held, "1", "1"))
		require.NoError(t, err)
		b, err := create("transition-mover", identity(hashN(301), "1", "1"))
		require.NoError(t, err)

		_, err = bookingStore.Transition(ctx, core.TransitionInput{
			BookingUID:     b.UID,
			ToStatus:       "confirming",
			IdempotencyKey: postgrestest.UniqueKey("transition-collide"),
			Metadata:       map[string]string{"tx_hash": "0X" + strings.ToUpper(held[2:]), "txlog_seq": "+1"},
		})
		assert.ErrorIs(t, err, core.ErrConflict)
		got, err := bookingStore.GetBooking(ctx, b.UID)
		require.NoError(t, err)
		assert.Equal(t, core.Status("pending"), got.Status)
	})

	t.Run("transition merge refuses invalid values", func(t *testing.T) {
		other := hashN(400)
		b, err := create("transition-invalid-base", identity(other, "0", "1"))
		require.NoError(t, err)
		for name, md := range map[string]map[string]string{
			"no prefix":         {"tx_hash": other[2:]},
			"non-numeric chain": {"chain_id": "ethereum"},
			"non-numeric seq":   {"txlog_seq": "one"},
		} {
			_, err := bookingStore.Transition(ctx, core.TransitionInput{
				BookingUID:     b.UID,
				ToStatus:       "confirming",
				IdempotencyKey: postgrestest.UniqueKey("transition-invalid-" + name),
				Metadata:       md,
			})
			assert.ErrorIs(t, err, core.ErrInvalidInput, name)
		}
		got, err := bookingStore.GetBooking(ctx, b.UID)
		require.NoError(t, err)
		assert.Equal(t, core.Status("pending"), got.Status, "every refused transition left the booking untouched")
		assert.Equal(t, other, got.Metadata["tx_hash"])
	})
}
