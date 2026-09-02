package presets

import (
	"context"

	"github.com/azex-ai/ledger/core"
)

// capitalClassifications introduces the equity system account.
//
// equity is DEBIT-normal, which is the opposite of what standard accounting's
// A = L + E would suggest, and the reason is this ledger's own polarity rule
// rather than a preference: a journal must satisfy sum(DR) == sum(CR), and a
// leg increases its account when entry_type == normal_side (core.Sign,
// docs/INVARIANTS.md I-43 / I-49). Two accounts that both INCREASE in the
// same journal must therefore carry OPPOSITE normal_sides. A capital
// injection increases custodial and equity together, and custodial is
// credit-normal (presets/templates.go), so equity has to be debit-normal for
// the pair to be expressible at all. Reading: a positive equity balance is
// the capital the operator has contributed and not yet taken back.
//
// The previous credit-normal declaration is what produced the 2026-09-02
// audit's Critical A-C1: written to standard convention ("DR the asset to
// increase it"), capital_injection debited credit-normal custodial and drove
// the custody figure DOWN by the injected amount, pinning SolvencyCheck at
// solvent=false for good -- the one action that exists to improve solvency
// was the one that destroyed it.
var capitalClassifications = []ClassificationPreset{
	{Code: "equity", Name: "Equity", NormalSide: core.NormalSideDebit, IsSystem: true},
}

// HolderTxKindOther: both journal types only ever touch system-side
// classifications (custodial, equity — both IsSystem), never a role-bearing
// one, so they never actually surface in a holder's transaction view; "the
// operator moved platform capital" doesn't fit deposit/withdrawal/transfer/
// fee/adjustment even in principle.
var capitalJournalTypes = []JournalTypePreset{
	{Code: "capital_injection", Name: "Capital Injection", HolderKind: core.HolderTxKindOther},
	{Code: "capital_withdraw", Name: "Capital Withdrawal", HolderKind: core.HolderTxKindOther},
}

// capitalTemplates defines the two capital movement patterns:
//
//	capital_injection: CR custodial (system, +amount)  DR equity (system, +amount)
//	capital_withdraw:  DR custodial (system, -amount)  CR equity (system, -amount)
//
// Both legs of an injection increase; both legs of a withdrawal decrease.
// custodial is credit-normal and equity debit-normal, so "increase" reads CR
// on one and DR on the other -- see capitalClassifications above for why the
// polarities have to be opposite.
//
// These are the only two shipped templates that move the solvency margin:
// they add (or remove) custodied assets without creating a matching holder
// liability. Every other preset is margin-neutral by construction
// (postgres/solvency_test.go pins the whole table).
var capitalTemplates = []TemplatePreset{
	{
		Code:            "capital_injection",
		Name:            "Capital Injection",
		JournalTypeCode: "capital_injection",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "custodial", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 1},
			{ClassificationCode: "equity", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 2},
		},
	},
	{
		Code:            "capital_withdraw",
		Name:            "Capital Withdrawal",
		JournalTypeCode: "capital_withdraw",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "custodial", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 1},
			{ClassificationCode: "equity", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 2},
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
