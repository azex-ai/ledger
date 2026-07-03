package core

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEntryTemplate_Render(t *testing.T) {
	tmpl := EntryTemplate{
		UID:            "uid-1",
		Code:           "deposit_confirm",
		Name:           "Deposit Confirm",
		JournalTypeUID: "jt-1",
		IsActive:       true,
		Lines: []EntryTemplateLine{
			{ClassificationUID: "cls-10", EntryType: EntryTypeDebit, HolderRole: HolderRoleUser, AmountKey: "amount"},
			{ClassificationUID: "cls-20", EntryType: EntryTypeCredit, HolderRole: HolderRoleSystem, AmountKey: "amount"},
			{ClassificationUID: "cls-30", EntryType: EntryTypeDebit, HolderRole: HolderRoleUser, AmountKey: "fee"},
			{ClassificationUID: "cls-40", EntryType: EntryTypeCredit, HolderRole: HolderRoleSystem, AmountKey: "fee"},
		},
	}

	params := TemplateParams{
		HolderID:       42,
		CurrencyUID:    "cur-1",
		IdempotencyKey: "tx-100",
		Amounts: map[string]decimal.Decimal{
			"amount": decimal.NewFromInt(1000),
			"fee":    decimal.NewFromInt(5),
		},
	}

	input, err := tmpl.Render(params)
	require.NoError(t, err)
	assert.Equal(t, "jt-1", input.JournalTypeUID)
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
		UID:            "uid-1",
		Code:           "test",
		Name:           "Test",
		JournalTypeUID: "jt-1",
		IsActive:       true,
		Lines: []EntryTemplateLine{
			{ClassificationUID: "cls-10", EntryType: EntryTypeDebit, HolderRole: HolderRoleUser, AmountKey: "amount"},
		},
	}
	params := TemplateParams{
		HolderID:       42,
		CurrencyUID:    "cur-1",
		IdempotencyKey: "tx-101",
		Amounts:        map[string]decimal.Decimal{},
	}
	_, err := tmpl.Render(params)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "amount key")
}

func TestEntryTemplate_Render_Inactive(t *testing.T) {
	tmpl := EntryTemplate{
		UID:      "uid-1",
		Code:     "test",
		Name:     "Test",
		IsActive: false,
		Lines:    []EntryTemplateLine{},
	}
	_, err := tmpl.Render(TemplateParams{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "inactive")
}

func TestEntryTemplate_Render_WrapsInvalidInput(t *testing.T) {
	cases := []struct {
		name   string
		tmpl   EntryTemplate
		params TemplateParams
	}{
		{
			name: "inactive template",
			tmpl: EntryTemplate{
				UID:      "uid-1",
				Code:     "inactive_tmpl",
				Name:     "Inactive",
				IsActive: false,
				Lines:    []EntryTemplateLine{},
			},
			params: TemplateParams{},
		},
		{
			name: "missing amount key",
			tmpl: EntryTemplate{
				UID:      "uid-2",
				Code:     "active_tmpl",
				Name:     "Active",
				IsActive: true,
				Lines: []EntryTemplateLine{
					{ClassificationUID: "cls-10", EntryType: EntryTypeDebit, HolderRole: HolderRoleUser, AmountKey: "amount"},
				},
			},
			params: TemplateParams{
				HolderID:       42,
				CurrencyUID:    "cur-1",
				IdempotencyKey: "tx-tmpl-01",
				Amounts:        map[string]decimal.Decimal{},
			},
		},
		{
			name: "invalid holder role",
			tmpl: EntryTemplate{
				UID:      "uid-3",
				Code:     "active_tmpl2",
				Name:     "Active 2",
				IsActive: true,
				Lines: []EntryTemplateLine{
					{ClassificationUID: "cls-10", EntryType: EntryTypeDebit, HolderRole: HolderRole("admin"), AmountKey: "amount"},
				},
			},
			params: TemplateParams{
				HolderID:       42,
				CurrencyUID:    "cur-1",
				IdempotencyKey: "tx-tmpl-02",
				Amounts: map[string]decimal.Decimal{
					"amount": decimal.NewFromInt(100),
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.tmpl.Render(tc.params)
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrInvalidInput), "expected ErrInvalidInput, got: %v", err)
		})
	}
}

func TestTemplateInput_Validate(t *testing.T) {
	valid := TemplateInput{
		Code:           "deposit_confirm",
		Name:           "Deposit Confirm",
		JournalTypeUID: "jt-1",
		Lines: []TemplateLineInput{
			{
				ClassificationUID: "cls-1",
				EntryType:         EntryTypeDebit,
				HolderRole:        HolderRoleUser,
				AmountKey:         "amount",
			},
		},
	}

	require.NoError(t, valid.Validate())

	invalid := valid
	invalid.Lines = nil
	err := invalid.Validate()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidInput))

	invalid = valid
	invalid.Lines = []TemplateLineInput{{
		ClassificationUID: "",
		EntryType:         EntryTypeDebit,
		HolderRole:        HolderRoleUser,
		AmountKey:         "amount",
	}}
	err = invalid.Validate()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidInput))
}

func TestEntryTemplate_Render_RejectsEmptyLines(t *testing.T) {
	tmpl := EntryTemplate{
		UID:            "uid-1",
		Code:           "broken",
		Name:           "Broken",
		JournalTypeUID: "jt-1",
		IsActive:       true,
	}

	_, err := tmpl.Render(TemplateParams{
		HolderID:       42,
		CurrencyUID:    "cur-1",
		IdempotencyKey: "preview",
		Amounts:        map[string]decimal.Decimal{},
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidInput))
	assert.Contains(t, err.Error(), "lines")
}

// Pins the HolderID sign guard: a negative HolderID would silently swap the
// user/system namespaces (system line = -(-x) lands on real user account x).
func TestTemplateRender_NegativeHolderIDRejected(t *testing.T) {
	tmpl := EntryTemplate{
		UID:            "uid-neg",
		Code:           "neg_holder",
		Name:           "Negative Holder",
		JournalTypeUID: "jt-1",
		IsActive:       true,
		Lines: []EntryTemplateLine{
			{ClassificationUID: "cls-10", EntryType: EntryTypeDebit, HolderRole: HolderRoleUser, AmountKey: "amount"},
			{ClassificationUID: "cls-20", EntryType: EntryTypeCredit, HolderRole: HolderRoleSystem, AmountKey: "amount"},
		},
	}
	_, err := tmpl.Render(TemplateParams{
		HolderID:       -42,
		CurrencyUID:    "cur-1",
		IdempotencyKey: "k1",
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(10)},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)

	_, err = tmpl.Render(TemplateParams{
		HolderID:       0,
		CurrencyUID:    "cur-1",
		IdempotencyKey: "k1",
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(10)},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)
}
