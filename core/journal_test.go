package core

import (
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJournalInput_Validate_Balanced(t *testing.T) {
	input := JournalInput{
		JournalTypeUID: "jt-1",
		IdempotencyKey: "tx-001",
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyUID: "cur-1", ClassificationUID: "cls-1", EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyUID: "cur-1", ClassificationUID: "cls-2", EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	}
	require.NoError(t, input.Validate())
}

func TestJournalInput_Validate_Unbalanced(t *testing.T) {
	input := JournalInput{
		JournalTypeUID: "jt-1",
		IdempotencyKey: "tx-002",
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyUID: "cur-1", ClassificationUID: "cls-1", EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyUID: "cur-1", ClassificationUID: "cls-2", EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(50)},
		},
	}
	err := input.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unbalanced")
}

func TestJournalInput_Validate_PerCurrencyBalance(t *testing.T) {
	input := JournalInput{
		JournalTypeUID: "jt-1",
		IdempotencyKey: "tx-currency-balance",
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyUID: "cur-1", ClassificationUID: "cls-1", EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyUID: "cur-2", ClassificationUID: "cls-2", EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	}

	err := input.Validate()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnbalancedJournal), "expected ErrUnbalancedJournal, got: %v", err)
	assert.Contains(t, err.Error(), "currency cur-1 unbalanced")
}

func TestJournalInput_Validate_EmptyEntries(t *testing.T) {
	input := JournalInput{
		JournalTypeUID: "jt-1",
		IdempotencyKey: "tx-003",
		Entries:        []EntryInput{},
	}
	err := input.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "entries")
}

func TestJournalInput_Validate_ZeroAmount(t *testing.T) {
	input := JournalInput{
		JournalTypeUID: "jt-1",
		IdempotencyKey: "tx-004",
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyUID: "cur-1", ClassificationUID: "cls-1", EntryType: EntryTypeDebit, Amount: decimal.Zero},
		},
	}
	err := input.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "positive")
}

func TestJournalInput_Validate_NoIdempotencyKey(t *testing.T) {
	input := JournalInput{
		JournalTypeUID: "jt-1",
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyUID: "cur-1", ClassificationUID: "cls-1", EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyUID: "cur-1", ClassificationUID: "cls-2", EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	}
	err := input.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "idempotency")
}

func TestJournalInput_Validate_ZeroHolderRejected(t *testing.T) {
	input := JournalInput{
		JournalTypeUID: "jt-1",
		IdempotencyKey: "tx-zero-holder",
		Entries: []EntryInput{
			{AccountHolder: 0, CurrencyUID: "cur-1", ClassificationUID: "cls-1", EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyUID: "cur-1", ClassificationUID: "cls-2", EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	}

	err := input.Validate()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)
	assert.Contains(t, err.Error(), "account_holder")
}

func TestJournalInput_Validate_ZeroCurrencyRejected(t *testing.T) {
	input := JournalInput{
		JournalTypeUID: "jt-1",
		IdempotencyKey: "tx-zero-currency",
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyUID: "", ClassificationUID: "cls-1", EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyUID: "cur-1", ClassificationUID: "cls-2", EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	}

	err := input.Validate()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)
	assert.Contains(t, err.Error(), "currency_uid")
}

func TestJournalInput_Validate_ZeroClassificationRejected(t *testing.T) {
	input := JournalInput{
		JournalTypeUID: "jt-1",
		IdempotencyKey: "tx-zero-classification",
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyUID: "cur-1", ClassificationUID: "", EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyUID: "cur-1", ClassificationUID: "cls-2", EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	}

	err := input.Validate()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)
	assert.Contains(t, err.Error(), "classification_uid")
}

func TestJournalInput_Validate_WrapsInvalidInput(t *testing.T) {
	cases := []struct {
		name  string
		input JournalInput
	}{
		{
			name: "empty idempotency key",
			input: JournalInput{
				JournalTypeUID: "jt-1",
				Entries: []EntryInput{
					{AccountHolder: 1, CurrencyUID: "cur-1", ClassificationUID: "cls-1", EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(100)},
					{AccountHolder: -1, CurrencyUID: "cur-1", ClassificationUID: "cls-2", EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(100)},
				},
			},
		},
		{
			name: "empty entries",
			input: JournalInput{
				JournalTypeUID: "jt-1",
				IdempotencyKey: "tx-wrap-01",
				Entries:        []EntryInput{},
			},
		},
		{
			name: "invalid entry type",
			input: JournalInput{
				JournalTypeUID: "jt-1",
				IdempotencyKey: "tx-wrap-02",
				Entries: []EntryInput{
					{AccountHolder: 1, CurrencyUID: "cur-1", ClassificationUID: "cls-1", EntryType: EntryType("bad"), Amount: decimal.NewFromInt(100)},
				},
			},
		},
		{
			name: "negative amount",
			input: JournalInput{
				JournalTypeUID: "jt-1",
				IdempotencyKey: "tx-wrap-03",
				Entries: []EntryInput{
					{AccountHolder: 1, CurrencyUID: "cur-1", ClassificationUID: "cls-1", EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(-1)},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.input.Validate()
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrInvalidInput), "expected ErrInvalidInput, got: %v", err)
		})
	}
}

func TestJournalInput_Validate_WrapsUnbalanced(t *testing.T) {
	input := JournalInput{
		JournalTypeUID: "jt-1",
		IdempotencyKey: "tx-unbalanced",
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyUID: "cur-1", ClassificationUID: "cls-1", EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyUID: "cur-1", ClassificationUID: "cls-2", EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(50)},
		},
	}

	err := input.Validate()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnbalancedJournal), "expected ErrUnbalancedJournal, got: %v", err)
	assert.False(t, errors.Is(err, ErrInvalidInput), "unbalanced journal must NOT wrap ErrInvalidInput")
}

func TestJournalInput_Validate_EffectiveAt_Zero_OK(t *testing.T) {
	input := JournalInput{
		JournalTypeUID: "jt-1",
		IdempotencyKey: "tx-eff-zero",
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyUID: "cur-1", ClassificationUID: "cls-1", EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyUID: "cur-1", ClassificationUID: "cls-2", EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	}
	require.NoError(t, input.Validate())
}

func TestJournalInput_Validate_EffectiveAt_Past_OK(t *testing.T) {
	input := JournalInput{
		JournalTypeUID: "jt-1",
		IdempotencyKey: "tx-eff-past",
		EffectiveAt:    time.Now().AddDate(-1, 0, 0),
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyUID: "cur-1", ClassificationUID: "cls-1", EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyUID: "cur-1", ClassificationUID: "cls-2", EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	}
	require.NoError(t, input.Validate())
}

func TestJournalInput_Validate_EffectiveAt_WithinTolerance_OK(t *testing.T) {
	input := JournalInput{
		JournalTypeUID: "jt-1",
		IdempotencyKey: "tx-eff-tolerance",
		EffectiveAt:    time.Now().Add(2 * time.Minute),
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyUID: "cur-1", ClassificationUID: "cls-1", EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyUID: "cur-1", ClassificationUID: "cls-2", EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	}
	require.NoError(t, input.Validate())
}

func TestJournalInput_Validate_EffectiveAt_FarFuture_Rejected(t *testing.T) {
	input := JournalInput{
		JournalTypeUID: "jt-1",
		IdempotencyKey: "tx-eff-future",
		EffectiveAt:    time.Now().Add(time.Hour),
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyUID: "cur-1", ClassificationUID: "cls-1", EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyUID: "cur-1", ClassificationUID: "cls-2", EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	}
	err := input.Validate()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)
	assert.Contains(t, err.Error(), "effective_at")
}

func TestJournalInput_Totals(t *testing.T) {
	input := JournalInput{
		JournalTypeUID: "jt-1",
		IdempotencyKey: "tx-005",
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyUID: "cur-1", ClassificationUID: "cls-1", EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: 1, CurrencyUID: "cur-1", ClassificationUID: "cls-2", EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(50)},
			{AccountHolder: -1, CurrencyUID: "cur-1", ClassificationUID: "cls-3", EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(150)},
		},
	}
	debit, credit := input.Totals()
	assert.True(t, debit.Equal(decimal.NewFromInt(150)))
	assert.True(t, credit.Equal(decimal.NewFromInt(150)))
}
