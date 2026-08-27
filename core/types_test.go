package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEntryType_IsValid(t *testing.T) {
	assert.True(t, EntryTypeDebit.IsValid())
	assert.True(t, EntryTypeCredit.IsValid())
	assert.False(t, EntryType("invalid").IsValid())
}

func TestNormalSide_IsValid(t *testing.T) {
	assert.True(t, NormalSideDebit.IsValid())
	assert.True(t, NormalSideCredit.IsValid())
	assert.False(t, NormalSide("invalid").IsValid())
}

// TestHolderTxKind_IsValid pins the M-7 fix's closed vocabulary
// (docs/INVARIANTS.md I-44): the six named values plus HolderTxKindNone
// ("", tolerated here unlike BalanceRoleNone) are valid; anything else,
// including the two rejected prior shapes (a journal-type code or a uid
// string), is not.
func TestHolderTxKind_IsValid(t *testing.T) {
	valid := []HolderTxKind{
		HolderTxKindNone, HolderTxKindDeposit, HolderTxKindWithdrawal,
		HolderTxKindTransfer, HolderTxKindFee, HolderTxKindAdjustment, HolderTxKindOther,
	}
	for _, k := range valid {
		assert.True(t, k.IsValid(), "%q should be valid", k)
	}
	invalid := []HolderTxKind{
		"deposit_confirm",                      // the rejected journal_types.code shape (M-7)
		"9f3a1b2c-0000-0000-0000-000000000000", // the rejected journal_types.uid shape (M-7)
		"DEPOSIT", "Deposit", "not_a_kind",
	}
	for _, k := range invalid {
		assert.False(t, k.IsValid(), "%q should be invalid", k)
	}
}

func TestSystemAccountHolder(t *testing.T) {
	assert.Equal(t, int64(-42), SystemAccountHolder(42))
	assert.True(t, IsSystemAccount(-42))
	assert.False(t, IsSystemAccount(42))
}

func TestCurrencyInput_Validate(t *testing.T) {
	valid := CurrencyInput{Code: "USD", Name: "US Dollar", Exponent: 2}
	require.NoError(t, valid.Validate())

	// Exponent 0 is a legitimate value (JPY), not treated as "missing".
	require.NoError(t, CurrencyInput{Code: "JPY", Name: "Yen", Exponent: 0}.Validate())

	// Exponent 18 is the upper bound, inclusive.
	require.NoError(t, CurrencyInput{Code: "WEI", Name: "Wei", Exponent: 18}.Validate())

	cases := []CurrencyInput{
		{Code: "", Name: "US Dollar", Exponent: 2},
		{Code: "USD", Name: "", Exponent: 2},
		{Code: "USD", Name: "US Dollar", Exponent: -1},
		{Code: "USD", Name: "US Dollar", Exponent: 19},
	}
	for _, tc := range cases {
		err := tc.Validate()
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidInput)
	}
}
