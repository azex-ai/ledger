package presets

import (
	"context"

	"github.com/azex-ai/ledger/core"
)

// capitalClassifications introduces the equity system account.
// Equity is credit-normal (A = L + E); increases on the right side.
var capitalClassifications = []ClassificationPreset{
	{Code: "equity", Name: "Equity", NormalSide: core.NormalSideCredit, IsSystem: true},
}

var capitalJournalTypes = []JournalTypePreset{
	{Code: "capital_injection", Name: "Capital Injection"},
	{Code: "capital_withdraw", Name: "Capital Withdrawal"},
}

// capitalTemplates defines the two capital movement patterns:
//
//	capital_injection: DR custodial (system) CR equity (system)
//	capital_withdraw:  DR equity (system)    CR custodial (system)
var capitalTemplates = []TemplatePreset{
	{
		Code:            "capital_injection",
		Name:            "Capital Injection",
		JournalTypeCode: "capital_injection",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "custodial", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 1},
			{ClassificationCode: "equity", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 2},
		},
	},
	{
		Code:            "capital_withdraw",
		Name:            "Capital Withdrawal",
		JournalTypeCode: "capital_withdraw",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "equity", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 1},
			{ClassificationCode: "custodial", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 2},
		},
	},
}

// CapitalBundle returns the classifications, journal types, and templates
// required to post capital injection and capital withdrawal journals.
func CapitalBundle() TemplateBundle {
	return TemplateBundle{
		Classifications: combineClassifications(sharedTemplateClassifications, capitalClassifications),
		JournalTypes:    cloneJournalTypes(capitalJournalTypes),
		Templates:       cloneTemplates(capitalTemplates),
	}
}

// InstallCapitalBundle installs the capital bundle. Safe to call repeatedly —
// existing rows are validated and reused.
func InstallCapitalBundle(
	ctx context.Context,
	classifications core.ClassificationStore,
	journalTypes core.JournalTypeStore,
	templates core.TemplateStore,
) error {
	return InstallTemplateBundle(ctx, classifications, journalTypes, templates, CapitalBundle())
}
