package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

func TestBookingStore_ListExpiredBookings_ExcludesFailed(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	bookingStore := postgres.NewBookingStore(pool)

	lifecycle := &core.Lifecycle{
		Initial:  "pending",
		Terminal: []core.Status{"confirmed", "expired"},
		Transitions: map[core.Status][]core.Status{
			"pending": {"failed", "expired"},
			"failed":  {"pending", "expired"},
		},
	}

	cls, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code:       "withdraw_expiry_test",
		Name:       "Withdraw Expiry Test",
		NormalSide: core.NormalSideCredit,
		Lifecycle:  lifecycle,
	})
	require.NoError(t, err)

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")

	booking, err := bookingStore.CreateBooking(ctx, core.CreateBookingInput{
		ClassificationCode: cls.Code,
		AccountHolder:      42,
		CurrencyID:         curID,
		Amount:             decimal.NewFromInt(100),
		IdempotencyKey:     postgrestest.UniqueKey("booking-failed-expiry"),
		ChannelName:        "test",
		ExpiresAt:          time.Now().Add(-time.Hour),
	})
	require.NoError(t, err)

	_, err = bookingStore.Transition(ctx, core.TransitionInput{
		BookingID: booking.ID,
		ToStatus:  "failed",
	})
	require.NoError(t, err)

	expired, err := bookingStore.ListExpiredBookings(ctx, 10)
	require.NoError(t, err)
	require.Len(t, expired, 1)
	assert.Equal(t, booking.ID, expired[0].ID)
}

func TestBookingStore_ListExpiredBookings_ExcludesCustomTerminalState(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	bookingStore := postgres.NewBookingStore(pool)

	lifecycle := &core.Lifecycle{
		Initial:  "pending",
		Terminal: []core.Status{"done", "expired"},
		Transitions: map[core.Status][]core.Status{
			"pending": {"done", "expired"},
		},
	}

	cls, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code:       "booking_terminal_done",
		Name:       "Booking Terminal Done",
		NormalSide: core.NormalSideCredit,
		Lifecycle:  lifecycle,
	})
	require.NoError(t, err)

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")

	booking, err := bookingStore.CreateBooking(ctx, core.CreateBookingInput{
		ClassificationCode: cls.Code,
		AccountHolder:      43,
		CurrencyID:         curID,
		Amount:             decimal.NewFromInt(100),
		IdempotencyKey:     postgrestest.UniqueKey("booking-done-expiry"),
		ChannelName:        "test",
		ExpiresAt:          time.Now().Add(-time.Hour),
	})
	require.NoError(t, err)

	_, err = bookingStore.Transition(ctx, core.TransitionInput{
		BookingID: booking.ID,
		ToStatus:  "done",
	})
	require.NoError(t, err)

	expired, err := bookingStore.ListExpiredBookings(ctx, 10)
	require.NoError(t, err)
	for _, item := range expired {
		assert.NotEqual(t, booking.ID, item.ID)
	}
}
