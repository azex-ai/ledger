package core

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateBookingInput_Validate(t *testing.T) {
	valid := CreateBookingInput{
		ClassificationCode: "deposit",
		AccountHolder:      1001,
		CurrencyID:         1,
		Amount:             decimal.NewFromInt(100),
		IdempotencyKey:     "booking-1",
	}

	require.NoError(t, valid.Validate())

	cases := []struct {
		name  string
		input CreateBookingInput
	}{
		{
			name:  "missing classification code",
			input: valid,
		},
		{
			name: "zero holder",
			input: CreateBookingInput{
				ClassificationCode: "deposit",
				CurrencyID:         1,
				Amount:             decimal.NewFromInt(100),
				IdempotencyKey:     "booking-1",
			},
		},
		{
			name: "zero currency",
			input: CreateBookingInput{
				ClassificationCode: "deposit",
				AccountHolder:      1001,
				Amount:             decimal.NewFromInt(100),
				IdempotencyKey:     "booking-1",
			},
		},
		{
			name: "non-positive amount",
			input: CreateBookingInput{
				ClassificationCode: "deposit",
				AccountHolder:      1001,
				CurrencyID:         1,
				Amount:             decimal.Zero,
				IdempotencyKey:     "booking-1",
			},
		},
		{
			name: "missing idempotency key",
			input: CreateBookingInput{
				ClassificationCode: "deposit",
				AccountHolder:      1001,
				CurrencyID:         1,
				Amount:             decimal.NewFromInt(100),
			},
		},
	}

	cases[0].input = valid
	cases[0].input.ClassificationCode = ""

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.input.Validate()
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidInput)
		})
	}
}

func TestTransitionInput_Validate(t *testing.T) {
	valid := TransitionInput{
		BookingID: 1,
		ToStatus:  "confirmed",
		Amount:    decimal.NewFromInt(100),
	}
	require.NoError(t, valid.Validate())

	cases := []struct {
		name  string
		input TransitionInput
	}{
		{
			name: "invalid booking id",
			input: TransitionInput{
				ToStatus: "confirmed",
			},
		},
		{
			name: "missing status",
			input: TransitionInput{
				BookingID: 1,
			},
		},
		{
			name: "negative amount",
			input: TransitionInput{
				BookingID: 1,
				ToStatus:  "confirmed",
				Amount:    decimal.NewFromInt(-1),
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
