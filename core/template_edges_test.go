package core

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validateDefinition is invoked from Render. Cover its dedicated branches —
// nil receiver, missing code/name, non-positive journal type, malformed
// lines — so a future refactor that bypasses Validate doesn't silently
// accept broken templates.
func TestEntryTemplate_validateDefinition(t *testing.T) {
	cases := []struct {
		name    string
		tmpl    *EntryTemplate
		errSnip string
	}{
		{"nil receiver", nil, "template is nil"},
		{
			name:    "empty code",
			tmpl:    &EntryTemplate{Code: "", Name: "X", JournalTypeID: 1, IsActive: true, Lines: []EntryTemplateLine{{ClassificationID: 1, EntryType: EntryTypeDebit, HolderRole: HolderRoleUser, AmountKey: "x"}}},
			errSnip: "code required",
		},
		{
			name:    "empty name",
			tmpl:    &EntryTemplate{Code: "x", Name: "", JournalTypeID: 1, IsActive: true, Lines: []EntryTemplateLine{{ClassificationID: 1, EntryType: EntryTypeDebit, HolderRole: HolderRoleUser, AmountKey: "x"}}},
			errSnip: "name required",
		},
		{
			name:    "zero journal type id",
			tmpl:    &EntryTemplate{Code: "x", Name: "X", JournalTypeID: 0, IsActive: true, Lines: []EntryTemplateLine{{ClassificationID: 1, EntryType: EntryTypeDebit, HolderRole: HolderRoleUser, AmountKey: "x"}}},
			errSnip: "journal_type_id must be positive",
		},
		{
			name:    "negative classification id",
			tmpl:    &EntryTemplate{Code: "x", Name: "X", JournalTypeID: 1, IsActive: true, Lines: []EntryTemplateLine{{ClassificationID: -1, EntryType: EntryTypeDebit, HolderRole: HolderRoleUser, AmountKey: "x"}}},
			errSnip: "classification_id must be positive",
		},
		{
			name:    "invalid entry type",
			tmpl:    &EntryTemplate{Code: "x", Name: "X", JournalTypeID: 1, IsActive: true, Lines: []EntryTemplateLine{{ClassificationID: 1, EntryType: EntryType("upside-down"), HolderRole: HolderRoleUser, AmountKey: "x"}}},
			errSnip: "invalid entry type",
		},
		{
			name:    "missing amount key",
			tmpl:    &EntryTemplate{Code: "x", Name: "X", JournalTypeID: 1, IsActive: true, Lines: []EntryTemplateLine{{ClassificationID: 1, EntryType: EntryTypeDebit, HolderRole: HolderRoleUser, AmountKey: ""}}},
			errSnip: "amount_key required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// validateDefinition is unexported but reachable through Render
			// when the template is active.
			if tc.tmpl == nil {
				// Dereferencing nil through Render would panic before reaching
				// the nil check; call the method on a typed nil instead.
				err := tc.tmpl.validateDefinition()
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errSnip)
				assert.True(t, errors.Is(err, ErrInvalidInput))
				return
			}

			_, err := tc.tmpl.Render(TemplateParams{
				HolderID:       1,
				CurrencyID:     1,
				IdempotencyKey: "edge",
				Amounts:        map[string]decimal.Decimal{"x": decimal.NewFromInt(1)},
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errSnip)
			assert.True(t, errors.Is(err, ErrInvalidInput))
		})
	}
}

// Render must propagate IdempotencyKey, ActorID, Source, and Metadata to the
// resulting JournalInput verbatim so caller-supplied audit context survives
// the rendering step.
func TestEntryTemplate_Render_PassesThroughAuditContext(t *testing.T) {
	tmpl := &EntryTemplate{
		ID: 1, Code: "deposit_confirm", Name: "Deposit Confirm",
		JournalTypeID: 7, IsActive: true,
		Lines: []EntryTemplateLine{
			{ClassificationID: 10, EntryType: EntryTypeDebit, HolderRole: HolderRoleUser, AmountKey: "amount"},
			{ClassificationID: 20, EntryType: EntryTypeCredit, HolderRole: HolderRoleSystem, AmountKey: "amount"},
		},
	}

	meta := map[string]string{"chain": "ethereum", "tx": "0xabc"}
	input, err := tmpl.Render(TemplateParams{
		HolderID:       42,
		CurrencyID:     1,
		IdempotencyKey: "deposit-42-tx0xabc",
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(1000)},
		EventID:        77,
		ActorID:        99,
		Source:         "channel.evm",
		Metadata:       meta,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(7), input.JournalTypeID)
	assert.Equal(t, "deposit-42-tx0xabc", input.IdempotencyKey)
	assert.Equal(t, int64(77), input.EventID)
	assert.Equal(t, int64(99), input.ActorID)
	assert.Equal(t, "channel.evm", input.Source)
	assert.Equal(t, meta, input.Metadata)
}

// A zero amount in the params must be rejected by the downstream
// JournalInput.Validate() — Render builds the entries but Validate is the
// gatekeeper. Pin this contract so we don't accidentally bypass validation
// in a future refactor.
func TestEntryTemplate_Render_ZeroAmount_RejectedByValidate(t *testing.T) {
	tmpl := &EntryTemplate{
		ID: 1, Code: "x", Name: "X", JournalTypeID: 1, IsActive: true,
		Lines: []EntryTemplateLine{
			{ClassificationID: 10, EntryType: EntryTypeDebit, HolderRole: HolderRoleUser, AmountKey: "amount"},
			{ClassificationID: 20, EntryType: EntryTypeCredit, HolderRole: HolderRoleSystem, AmountKey: "amount"},
		},
	}
	_, err := tmpl.Render(TemplateParams{
		HolderID: 42, CurrencyID: 1, IdempotencyKey: "tx-zero",
		Amounts: map[string]decimal.Decimal{"amount": decimal.Zero},
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidInput))
	assert.Contains(t, err.Error(), "positive")
}

// TemplateInput.Validate has six independent guards (code, name, journal
// type, line count, plus three line-level guards). The existing test only
// hit lines + classification id; pin the rest so any future re-ordering of
// these checks is caught.
func TestTemplateInput_Validate_AllBranches(t *testing.T) {
	valid := TemplateInput{
		Code:          "deposit_confirm",
		Name:          "Deposit Confirm",
		JournalTypeID: 1,
		Lines: []TemplateLineInput{
			{ClassificationID: 1, EntryType: EntryTypeDebit, HolderRole: HolderRoleUser, AmountKey: "amount"},
		},
	}
	require.NoError(t, valid.Validate())

	cases := []struct {
		name    string
		mutate  func(*TemplateInput)
		errSnip string
	}{
		{"empty code", func(in *TemplateInput) { in.Code = "" }, "code required"},
		{"empty name", func(in *TemplateInput) { in.Name = "" }, "name required"},
		{"zero journal type", func(in *TemplateInput) { in.JournalTypeID = 0 }, "journal_type_id"},
		{"negative journal type", func(in *TemplateInput) { in.JournalTypeID = -5 }, "journal_type_id"},
		{"invalid entry type", func(in *TemplateInput) {
			in.Lines = []TemplateLineInput{{ClassificationID: 1, EntryType: EntryType("???"), HolderRole: HolderRoleUser, AmountKey: "x"}}
		}, "invalid entry type"},
		{"invalid holder role", func(in *TemplateInput) {
			in.Lines = []TemplateLineInput{{ClassificationID: 1, EntryType: EntryTypeDebit, HolderRole: HolderRole("ghost"), AmountKey: "x"}}
		}, "invalid holder role"},
		{"empty amount key", func(in *TemplateInput) {
			in.Lines = []TemplateLineInput{{ClassificationID: 1, EntryType: EntryTypeDebit, HolderRole: HolderRoleUser, AmountKey: ""}}
		}, "amount_key required"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := valid
			input.Lines = append([]TemplateLineInput{}, valid.Lines...)
			tc.mutate(&input)
			err := input.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errSnip)
			assert.True(t, errors.Is(err, ErrInvalidInput))
		})
	}
}

// HolderRoleUser routes to the positive holder; HolderRoleSystem routes to
// SystemAccountHolder(holder). Pin both branches together so a later
// refactor that swaps the assignment direction is caught immediately.
func TestEntryTemplate_Render_HolderRoleRouting(t *testing.T) {
	tmpl := &EntryTemplate{
		ID: 1, Code: "x", Name: "X", JournalTypeID: 1, IsActive: true,
		Lines: []EntryTemplateLine{
			{ClassificationID: 10, EntryType: EntryTypeDebit, HolderRole: HolderRoleUser, AmountKey: "a"},
			{ClassificationID: 20, EntryType: EntryTypeCredit, HolderRole: HolderRoleSystem, AmountKey: "a"},
		},
	}
	in, err := tmpl.Render(TemplateParams{
		HolderID: 42, CurrencyID: 1, IdempotencyKey: "tx-routing",
		Amounts: map[string]decimal.Decimal{"a": decimal.NewFromInt(1)},
	})
	require.NoError(t, err)
	require.Len(t, in.Entries, 2)
	assert.Equal(t, int64(42), in.Entries[0].AccountHolder)
	assert.Equal(t, SystemAccountHolder(42), in.Entries[1].AccountHolder)
}
