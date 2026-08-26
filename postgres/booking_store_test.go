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
			"pending": {"failed", "expired", "confirmed"},
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
		CurrencyUID:        curID,
		Amount:             decimal.NewFromInt(100),
		IdempotencyKey:     postgrestest.UniqueKey("booking-failed-expiry"),
		ChannelName:        "test",
		ExpiresAt:          time.Now().Add(-time.Hour),
	})
	require.NoError(t, err)

	_, err = bookingStore.Transition(ctx, core.TransitionInput{
		BookingUID: booking.UID,
		ToStatus:   "failed",
	})
	require.NoError(t, err)

	expired, err := bookingStore.ListExpiredBookings(ctx, 10)
	require.NoError(t, err)
	require.Len(t, expired, 1)
	assert.Equal(t, booking.UID, expired[0].UID)
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
		CurrencyUID:        curID,
		Amount:             decimal.NewFromInt(100),
		IdempotencyKey:     postgrestest.UniqueKey("booking-done-expiry"),
		ChannelName:        "test",
		ExpiresAt:          time.Now().Add(-time.Hour),
	})
	require.NoError(t, err)

	_, err = bookingStore.Transition(ctx, core.TransitionInput{
		BookingUID: booking.UID,
		ToStatus:   "done",
	})
	require.NoError(t, err)

	expired, err := bookingStore.ListExpiredBookings(ctx, 10)
	require.NoError(t, err)
	for _, item := range expired {
		assert.NotEqual(t, booking.UID, item.UID)
	}
}

func TestBookingStore_CreateBooking_IdempotentPayloadMismatch(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	bookingStore := postgres.NewBookingStore(pool)

	lifecycle := &core.Lifecycle{
		Initial:  "pending",
		Terminal: []core.Status{"confirmed"},
		Transitions: map[core.Status][]core.Status{
			"pending": {"confirmed"},
		},
	}

	cls, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code:       "booking_idem_mismatch",
		Name:       "Booking Idem Mismatch",
		NormalSide: core.NormalSideCredit,
		Lifecycle:  lifecycle,
	})
	require.NoError(t, err)

	curID := postgrestest.SeedCurrency(t, pool, "USDT-BOOK-IDEM", "Tether USD")
	key := postgrestest.UniqueKey("booking-idem")

	_, err = bookingStore.CreateBooking(ctx, core.CreateBookingInput{
		ClassificationCode: cls.Code,
		AccountHolder:      51,
		CurrencyUID:        curID,
		Amount:             decimal.NewFromInt(100),
		IdempotencyKey:     key,
		ChannelName:        "test",
	})
	require.NoError(t, err)

	_, err = bookingStore.CreateBooking(ctx, core.CreateBookingInput{
		ClassificationCode: cls.Code,
		AccountHolder:      51,
		CurrencyUID:        curID,
		Amount:             decimal.NewFromInt(200),
		IdempotencyKey:     key,
		ChannelName:        "test",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrConflict)
}

// Pins I-3's opt-in path for Transition (core.TransitionInput.IdempotencyKey).
//
// Before this fix, TransitionInput had no idempotency key at all, and the
// only replay protection was comparing the booking's CURRENT status against
// the call's ToStatus (idempotentTransitionEvent) -- a path that only works
// when nothing else has moved the booking since. This test's whole point is
// the case that path cannot cover: a retry of an EARLIER transition arriving
// after a LATER, legitimate transition has already moved the booking past
// ToStatus. Without a durable key, that retry hits
// lifecycle.CanTransition(current, ToStatus) == false and returns
// ErrInvalidTransition -- indistinguishable from a genuine invalid request.
func TestBookingStore_Transition_IdempotencyKey_SurvivesForwardProgress(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	bookingStore := postgres.NewBookingStore(pool)

	lifecycle := &core.Lifecycle{
		Initial:  "pending",
		Terminal: []core.Status{"confirmed", "failed"},
		Transitions: map[core.Status][]core.Status{
			"pending":    {"confirming", "failed"},
			"confirming": {"confirmed", "failed"},
		},
	}

	cls, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code:       "booking_transition_idem",
		Name:       "Booking Transition Idem",
		NormalSide: core.NormalSideCredit,
		Lifecycle:  lifecycle,
	})
	require.NoError(t, err)

	curID := postgrestest.SeedCurrency(t, pool, "USDT-BOOK-TRANS-IDEM", "Tether USD")

	booking, err := bookingStore.CreateBooking(ctx, core.CreateBookingInput{
		ClassificationCode: cls.Code,
		AccountHolder:      52,
		CurrencyUID:        curID,
		Amount:             decimal.NewFromInt(100),
		IdempotencyKey:     postgrestest.UniqueKey("booking-transition-idem"),
		ChannelName:        "test",
	})
	require.NoError(t, err)

	confirmingKey := postgrestest.UniqueKey("transition-confirming")
	event1, err := bookingStore.Transition(ctx, core.TransitionInput{
		BookingUID:     booking.UID,
		ToStatus:       "confirming",
		IdempotencyKey: confirmingKey,
	})
	require.NoError(t, err)

	// Legitimate forward progress: something else advances the booking past
	// "confirming" before the retry below arrives.
	_, err = bookingStore.Transition(ctx, core.TransitionInput{
		BookingUID: booking.UID,
		ToStatus:   "confirmed",
	})
	require.NoError(t, err)

	// The retry of the FIRST transition arrives late. Without IdempotencyKey,
	// lifecycle.CanTransition("confirmed", "confirming") is false and this
	// would be ErrInvalidTransition; with it, it must return the original
	// event unchanged.
	event2, err := bookingStore.Transition(ctx, core.TransitionInput{
		BookingUID:     booking.UID,
		ToStatus:       "confirming",
		IdempotencyKey: confirmingKey,
	})
	require.NoError(t, err)
	assert.Equal(t, event1.UID, event2.UID)

	// The booking itself was NOT reopened back to "confirming" -- the replay
	// returned the historical event, it did not re-apply the transition.
	current, err := bookingStore.GetBooking(ctx, booking.UID)
	require.NoError(t, err)
	assert.Equal(t, core.Status("confirmed"), current.Status)

	// Reusing the same key for a different to_status is a payload mismatch,
	// not a silent success.
	_, err = bookingStore.Transition(ctx, core.TransitionInput{
		BookingUID:     booking.UID,
		ToStatus:       "failed",
		IdempotencyKey: confirmingKey,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrConflict)
}
