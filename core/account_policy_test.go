package core

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAccountPolicyStatus_IsValid(t *testing.T) {
	assert.True(t, AccountPolicyStatusActive.IsValid())
	assert.True(t, AccountPolicyStatusFrozen.IsValid())
	assert.True(t, AccountPolicyStatusClosed.IsValid())
	assert.False(t, AccountPolicyStatus("bogus").IsValid())
	assert.False(t, AccountPolicyStatus("").IsValid())
}

func TestAccountPolicyInput_Validate(t *testing.T) {
	valid := AccountPolicyInput{
		AccountHolder: 7,
		CurrencyID:    1,
		Status:        AccountPolicyStatusFrozen,
	}
	require.NoError(t, valid.Validate())

	// Wildcard tiers (currency_id == 0, classification_id == 0) are valid.
	require.NoError(t, AccountPolicyInput{AccountHolder: 7, Status: AccountPolicyStatusActive}.Validate())

	cases := []struct {
		name  string
		input AccountPolicyInput
	}{
		{
			name:  "zero holder",
			input: AccountPolicyInput{Status: AccountPolicyStatusActive},
		},
		{
			name:  "negative currency",
			input: AccountPolicyInput{AccountHolder: 7, CurrencyID: -1, Status: AccountPolicyStatusActive},
		},
		{
			name:  "negative classification",
			input: AccountPolicyInput{AccountHolder: 7, ClassificationID: -1, Status: AccountPolicyStatusActive},
		},
		{
			name:  "invalid status",
			input: AccountPolicyInput{AccountHolder: 7, Status: AccountPolicyStatus("bogus")},
		},
		{
			name: "note too long",
			input: AccountPolicyInput{
				AccountHolder: 7,
				Status:        AccountPolicyStatusActive,
				Note:          string(make([]byte, accountPolicyNoteMaxLen+1)),
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

func TestAccountPolicyInput_Validate_NegativeMinBalanceAllowed(t *testing.T) {
	// Negative min_balance (overdraft/credit limit) is a valid, deliberate
	// configuration — must not be rejected.
	input := AccountPolicyInput{
		AccountHolder:     7,
		Status:            AccountPolicyStatusActive,
		MinBalance:        decimal.NewFromInt(-100),
		EnforceMinBalance: true,
	}
	require.NoError(t, input.Validate())
}

func TestEntryDirection(t *testing.T) {
	cases := []struct {
		name       string
		entryType  EntryType
		normalSide NormalSide
		want       BalanceDirection
	}{
		{"debit on debit-normal increases", EntryTypeDebit, NormalSideDebit, BalanceDirectionIncrease},
		{"credit on debit-normal decreases", EntryTypeCredit, NormalSideDebit, BalanceDirectionDecrease},
		{"credit on credit-normal increases", EntryTypeCredit, NormalSideCredit, BalanceDirectionIncrease},
		{"debit on credit-normal decreases", EntryTypeDebit, NormalSideCredit, BalanceDirectionDecrease},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, EntryDirection(tc.entryType, tc.normalSide))
		})
	}
}
