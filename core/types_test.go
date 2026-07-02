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
