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
	"github.com/azex-ai/ledger/presets"
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
		IsSystem:   true,
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
		BookingUID:     booking.UID,
		ToStatus:       "failed",
		IdempotencyKey: postgrestest.UniqueKey("booking-failed-expiry-transition"),
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
		IsSystem:   true,
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
		BookingUID:     booking.UID,
		ToStatus:       "done",
		IdempotencyKey: postgrestest.UniqueKey("booking-done-expiry-transition"),
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
		IsSystem:   true,
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

// Pins I-3's mandatory idempotency key for Transition
// (core.TransitionInput.IdempotencyKey).
//
// Before W1-A, TransitionInput had no idempotency key at all, and the only
// replay protection was comparing the booking's CURRENT status against the
// call's ToStatus (idempotentTransitionEvent) -- a path that only works when
// nothing else has moved the booking since. This test's whole point is the
// case that path cannot cover: a retry of an EARLIER transition arriving
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
		IsSystem:   true,
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
		BookingUID:     booking.UID,
		ToStatus:       "confirmed",
		IdempotencyKey: postgrestest.UniqueKey("transition-confirmed"),
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

// TestBookingStore_Transition_RevisitingSameStatus_DistinctKeysDoNotCollide
// pins W15-A's core hazard (docs/plans/2026-08-26-audit-remediation-contracts.md
// §7): now that IdempotencyKey is mandatory on every Transition call, a
// caller that derives it as just "<booking_uid>-<to_status>" breaks the
// moment a booking's lifecycle legitimately revisits the same status more
// than once -- exactly what presets.WithdrawalLifecycle's failed -> reserved
// retry edge does (a withdrawal that fails processing is retried by moving
// it back to "reserved"). Two distinct failure+retry cycles would derive the
// IDENTICAL key under that naive scheme, so the second retry would collide
// with the first cycle's receipt instead of actually applying.
//
// This test drives a withdrawal-shaped booking through TWO independent
// failed -> reserved cycles using two DIFFERENT keys (as a correct caller
// must -- e.g. server/handler_bookings.go's client-supplied
// Idempotency-Key header, a fresh value per submission) and asserts both
// apply for real: two distinct events, and the booking is not stuck on the
// first cycle's outcome.
func TestBookingStore_Transition_RevisitingSameStatus_DistinctKeysDoNotCollide(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	bookingStore := postgres.NewBookingStore(pool)

	cls, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code:       "withdrawal_revisit_test",
		Name:       "Withdrawal Revisit Test",
		NormalSide: core.NormalSideDebit,
		IsSystem:   true,
		Lifecycle:  presets.WithdrawalLifecycle,
	})
	require.NoError(t, err)

	curID := postgrestest.SeedCurrency(t, pool, postgrestest.UniqueKey("USDT-WD-REVISIT"), "Tether USD")

	booking, err := bookingStore.CreateBooking(ctx, core.CreateBookingInput{
		ClassificationCode: cls.Code,
		AccountHolder:      61,
		CurrencyUID:        curID,
		Amount:             decimal.NewFromInt(100),
		IdempotencyKey:     postgrestest.UniqueKey("withdrawal-revisit-booking"),
		ChannelName:        "test",
	})
	require.NoError(t, err)
	require.Equal(t, core.Status("locked"), booking.Status)

	// Drive to "reserved", then into a first failure.
	_, err = bookingStore.Transition(ctx, core.TransitionInput{
		BookingUID: booking.UID, ToStatus: "reserved",
		IdempotencyKey: postgrestest.UniqueKey("wd-revisit-to-reserved-initial"),
	})
	require.NoError(t, err)
	_, err = bookingStore.Transition(ctx, core.TransitionInput{
		BookingUID: booking.UID, ToStatus: "processing",
		IdempotencyKey: postgrestest.UniqueKey("wd-revisit-to-processing-1"),
	})
	require.NoError(t, err)
	_, err = bookingStore.Transition(ctx, core.TransitionInput{
		BookingUID: booking.UID, ToStatus: "failed", ChannelRef: "attempt-1",
		IdempotencyKey: postgrestest.UniqueKey("wd-revisit-to-failed-1"),
	})
	require.NoError(t, err)

	// First retry: failed -> reserved, key R1.
	retryKey1 := postgrestest.UniqueKey("wd-revisit-retry")
	retryEvent1, err := bookingStore.Transition(ctx, core.TransitionInput{
		BookingUID: booking.UID, ToStatus: "reserved", ChannelRef: "retry-1",
		IdempotencyKey: retryKey1,
	})
	require.NoError(t, err)
	afterRetry1, err := bookingStore.GetBooking(ctx, booking.UID)
	require.NoError(t, err)
	assert.Equal(t, core.Status("reserved"), afterRetry1.Status, "first retry must actually apply")

	// Second, independent failure+retry cycle.
	_, err = bookingStore.Transition(ctx, core.TransitionInput{
		BookingUID: booking.UID, ToStatus: "processing",
		IdempotencyKey: postgrestest.UniqueKey("wd-revisit-to-processing-2"),
	})
	require.NoError(t, err)
	_, err = bookingStore.Transition(ctx, core.TransitionInput{
		BookingUID: booking.UID, ToStatus: "failed", ChannelRef: "attempt-2",
		IdempotencyKey: postgrestest.UniqueKey("wd-revisit-to-failed-2"),
	})
	require.NoError(t, err)

	// Second retry: failed -> reserved AGAIN, key R2 -- deliberately DIFFERENT
	// from R1 (the correct behavior). Under the naive "<uid>-reserved" scheme
	// this would instead be the SAME key as retryKey1's derivation, and this
	// call would incorrectly resolve against retryEvent1 instead of applying.
	retryKey2 := postgrestest.UniqueKey("wd-revisit-retry")
	retryEvent2, err := bookingStore.Transition(ctx, core.TransitionInput{
		BookingUID: booking.UID, ToStatus: "reserved", ChannelRef: "retry-2",
		IdempotencyKey: retryKey2,
	})
	require.NoError(t, err)
	assert.NotEqual(t, retryKey1, retryKey2, "test setup sanity: the two retries must use distinct keys")
	assert.NotEqual(t, retryEvent1.UID, retryEvent2.UID, "the second retry must produce its OWN event, not replay the first retry's")

	afterRetry2, err := bookingStore.GetBooking(ctx, booking.UID)
	require.NoError(t, err)
	assert.Equal(t, core.Status("reserved"), afterRetry2.Status, "second retry must actually apply, not silently short-circuit")
	assert.Equal(t, "retry-2", afterRetry2.ChannelRef, "booking state must reflect the second retry's own payload, proving it really re-applied")
}
