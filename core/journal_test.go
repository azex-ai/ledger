package core

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJournalInput_Validate_Balanced(t *testing.T) {
	input := JournalInput{
		JournalTypeID:  1,
		IdempotencyKey: "tx-001",
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyID: 1, ClassificationID: 1, EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyID: 1, ClassificationID: 2, EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	}
	require.NoError(t, input.Validate())
}

func TestJournalInput_Validate_Unbalanced(t *testing.T) {
	input := JournalInput{
		JournalTypeID:  1,
		IdempotencyKey: "tx-002",
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyID: 1, ClassificationID: 1, EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyID: 1, ClassificationID: 2, EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(50)},
		},
	}
	err := input.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unbalanced")
}

func TestJournalInput_Validate_EmptyEntries(t *testing.T) {
	input := JournalInput{
		JournalTypeID:  1,
		IdempotencyKey: "tx-003",
		Entries:        []EntryInput{},
	}
	err := input.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "entries")
}

func TestJournalInput_Validate_ZeroAmount(t *testing.T) {
	input := JournalInput{
		JournalTypeID:  1,
		IdempotencyKey: "tx-004",
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyID: 1, ClassificationID: 1, EntryType: EntryTypeDebit, Amount: decimal.Zero},
		},
	}
	err := input.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "positive")
}

func TestJournalInput_Validate_NoIdempotencyKey(t *testing.T) {
	input := JournalInput{
		JournalTypeID: 1,
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyID: 1, ClassificationID: 1, EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyID: 1, ClassificationID: 2, EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	}
	err := input.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "idempotency")
}

func TestJournalInput_Totals(t *testing.T) {
	input := JournalInput{
		JournalTypeID:  1,
		IdempotencyKey: "tx-005",
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyID: 1, ClassificationID: 1, EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: 1, CurrencyID: 1, ClassificationID: 2, EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(50)},
			{AccountHolder: -1, CurrencyID: 1, ClassificationID: 3, EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(150)},
		},
	}
	debit, credit := input.Totals()
	assert.True(t, debit.Equal(decimal.NewFromInt(150)))
	assert.True(t, credit.Equal(decimal.NewFromInt(150)))
}
