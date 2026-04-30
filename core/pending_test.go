package core

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAddPendingInput_Validate(t *testing.T) {
	valid := AddPendingInput{
		AccountHolder:  42,
		CurrencyID:     1,
		Amount:         decimal.NewFromInt(100),
		IdempotencyKey: "pending-add-1",
	}
	require.NoError(t, valid.Validate())

	cases := []struct {
		name    string
		mutate  func(*AddPendingInput)
		errSnip string
	}{
		{"zero holder", func(i *AddPendingInput) { i.AccountHolder = 0 }, "account_holder"},
		{"negative holder accepted (system side)", func(i *AddPendingInput) { i.AccountHolder = -42 }, ""},
		{"zero currency", func(i *AddPendingInput) { i.CurrencyID = 0 }, "currency_id"},
		{"negative currency", func(i *AddPendingInput) { i.CurrencyID = -1 }, "currency_id"},
		{"zero amount", func(i *AddPendingInput) { i.Amount = decimal.Zero }, "amount"},
		{"negative amount", func(i *AddPendingInput) { i.Amount = decimal.NewFromInt(-1) }, "amount"},
		{"empty idempotency", func(i *AddPendingInput) { i.IdempotencyKey = "" }, "idempotency_key"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := valid
			tc.mutate(&input)
			err := input.Validate()
			if tc.errSnip == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errSnip)
			assert.True(t, errors.Is(err, ErrInvalidInput))
		})
	}
}

func TestConfirmPendingInput_Validate(t *testing.T) {
	valid := ConfirmPendingInput{
		AccountHolder:  42,
		CurrencyID:     1,
		Amount:         decimal.NewFromInt(100),
		IdempotencyKey: "pending-confirm-1",
	}
	require.NoError(t, valid.Validate())

	cases := []struct {
		name    string
		mutate  func(*ConfirmPendingInput)
		errSnip string
	}{
		{"zero holder", func(i *ConfirmPendingInput) { i.AccountHolder = 0 }, "account_holder"},
		{"zero currency", func(i *ConfirmPendingInput) { i.CurrencyID = 0 }, "currency_id"},
		{"zero amount", func(i *ConfirmPendingInput) { i.Amount = decimal.Zero }, "amount"},
		{"negative amount", func(i *ConfirmPendingInput) { i.Amount = decimal.NewFromInt(-5) }, "amount"},
		{"empty idempotency", func(i *ConfirmPendingInput) { i.IdempotencyKey = "" }, "idempotency_key"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := valid
			tc.mutate(&input)
			err := input.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errSnip)
			assert.True(t, errors.Is(err, ErrInvalidInput))
		})
	}
}

func TestCancelPendingInput_Validate(t *testing.T) {
	valid := CancelPendingInput{
		AccountHolder:  42,
		CurrencyID:     1,
		Amount:         decimal.NewFromInt(100),
		IdempotencyKey: "pending-cancel-1",
		Reason:         "tx reverted on-chain",
	}
	require.NoError(t, valid.Validate())

	// Reason is intentionally not validated by core — it's metadata.
	noReason := valid
	noReason.Reason = ""
	require.NoError(t, noReason.Validate())

	cases := []struct {
		name    string
		mutate  func(*CancelPendingInput)
		errSnip string
	}{
		{"zero holder", func(i *CancelPendingInput) { i.AccountHolder = 0 }, "account_holder"},
		{"zero currency", func(i *CancelPendingInput) { i.CurrencyID = 0 }, "currency_id"},
		{"zero amount", func(i *CancelPendingInput) { i.Amount = decimal.Zero }, "amount"},
		{"negative amount", func(i *CancelPendingInput) { i.Amount = decimal.NewFromInt(-1) }, "amount"},
		{"empty idempotency", func(i *CancelPendingInput) { i.IdempotencyKey = "" }, "idempotency_key"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := valid
			tc.mutate(&input)
			err := input.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errSnip)
			assert.True(t, errors.Is(err, ErrInvalidInput))
		})
	}
}
