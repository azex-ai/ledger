package core

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReservationStatus_IsValid(t *testing.T) {
	assert.True(t, ReservationStatusActive.IsValid())
	assert.True(t, ReservationStatusSettling.IsValid())
	assert.True(t, ReservationStatusSettled.IsValid())
	assert.True(t, ReservationStatusReleased.IsValid())
	assert.False(t, ReservationStatus("bogus").IsValid())
}

func TestReservationStatus_CanTransition(t *testing.T) {
	assert.True(t, ReservationStatusActive.CanTransitionTo(ReservationStatusSettling))
	assert.True(t, ReservationStatusActive.CanTransitionTo(ReservationStatusSettled))
	assert.True(t, ReservationStatusActive.CanTransitionTo(ReservationStatusReleased))
	assert.True(t, ReservationStatusSettling.CanTransitionTo(ReservationStatusSettled))
	assert.True(t, ReservationStatusSettling.CanTransitionTo(ReservationStatusReleased))
	assert.False(t, ReservationStatusSettled.CanTransitionTo(ReservationStatusActive))
	assert.False(t, ReservationStatusReleased.CanTransitionTo(ReservationStatusActive))
}

func TestReserveInput_Validate(t *testing.T) {
	valid := ReserveInput{
		AccountHolder:  7,
		CurrencyID:     1,
		Amount:         decimal.NewFromInt(100),
		IdempotencyKey: "reserve-1",
	}
	require.NoError(t, valid.Validate())

	cases := []struct {
		name  string
		input ReserveInput
	}{
		{
			name: "zero holder",
			input: ReserveInput{
				CurrencyID:     1,
				Amount:         decimal.NewFromInt(100),
				IdempotencyKey: "reserve-1",
			},
		},
		{
			name: "zero currency",
			input: ReserveInput{
				AccountHolder:  7,
				Amount:         decimal.NewFromInt(100),
				IdempotencyKey: "reserve-1",
			},
		},
		{
			name: "non-positive amount",
			input: ReserveInput{
				AccountHolder:  7,
				CurrencyID:     1,
				Amount:         decimal.Zero,
				IdempotencyKey: "reserve-1",
			},
		},
		{
			name: "missing idempotency key",
			input: ReserveInput{
				AccountHolder: 7,
				CurrencyID:    1,
				Amount:        decimal.NewFromInt(100),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.input.Validate()
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidInput)
		})
	}
}
