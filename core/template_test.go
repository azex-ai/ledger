package core

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEntryTemplate_Render(t *testing.T) {
	tmpl := EntryTemplate{
		ID:            1,
		Code:          "deposit_confirm",
		JournalTypeID: 1,
		IsActive:      true,
		Lines: []EntryTemplateLine{
			{ClassificationID: 10, EntryType: EntryTypeDebit, HolderRole: HolderRoleUser, AmountKey: "amount"},
			{ClassificationID: 20, EntryType: EntryTypeCredit, HolderRole: HolderRoleSystem, AmountKey: "amount"},
			{ClassificationID: 30, EntryType: EntryTypeDebit, HolderRole: HolderRoleUser, AmountKey: "fee"},
			{ClassificationID: 40, EntryType: EntryTypeCredit, HolderRole: HolderRoleSystem, AmountKey: "fee"},
		},
	}

	params := TemplateParams{
		HolderID:       42,
		CurrencyID:     1,
		IdempotencyKey: "tx-100",
		Amounts: map[string]decimal.Decimal{
			"amount": decimal.NewFromInt(1000),
			"fee":    decimal.NewFromInt(5),
		},
	}

	input, err := tmpl.Render(params)
	require.NoError(t, err)
	assert.Equal(t, int64(1), input.JournalTypeID)
	assert.Equal(t, "tx-100", input.IdempotencyKey)
	assert.Len(t, input.Entries, 4)

	// Verify holder resolution
	assert.Equal(t, int64(42), input.Entries[0].AccountHolder)  // user
	assert.Equal(t, int64(-42), input.Entries[1].AccountHolder) // system

	// Verify balance
	require.NoError(t, input.Validate())
}

func TestEntryTemplate_Render_MissingAmountKey(t *testing.T) {
	tmpl := EntryTemplate{
		ID:       1,
		Code:     "test",
		IsActive: true,
		Lines: []EntryTemplateLine{
			{ClassificationID: 10, EntryType: EntryTypeDebit, HolderRole: HolderRoleUser, AmountKey: "amount"},
		},
	}
	params := TemplateParams{
		HolderID:       42,
		CurrencyID:     1,
		IdempotencyKey: "tx-101",
		Amounts:        map[string]decimal.Decimal{},
	}
	_, err := tmpl.Render(params)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "amount key")
}

func TestEntryTemplate_Render_Inactive(t *testing.T) {
	tmpl := EntryTemplate{
		ID:       1,
		Code:     "test",
		IsActive: false,
		Lines:    []EntryTemplateLine{},
	}
	_, err := tmpl.Render(TemplateParams{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "inactive")
}
