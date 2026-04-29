package presets

import (
	"context"

	"github.com/azex-ai/ledger/core"
)

// feeClassifications introduces the aggregate "fees" system account.
//
// Naming convention vs existing accounts:
//   - fee_expense (debit-normal, user)  — withdrawalOnlyClassifications — records the
//     user-side cost of a withdrawal fee as a debit against the user's account.
//   - fee_revenue (credit-normal, system) — withdrawalOnlyClassifications — records the
//     platform's revenue from withdrawal fees on the system side.
//   - fees (credit-normal, system) — THIS file — is a first-class catch-all revenue
//     account that aggregates all platform fee income (withdrawal fees, card top-up
//     fees, checkout fees, direct fee charges, etc.) in one place.  It is analogous
//     to consts.AccountClassificationFees in payments/backend.
//
// Relationship: fee_revenue and fees serve the same economic purpose but were
// coined independently.  Callers that previously used fee_revenue for withdrawal
// accounting may continue to do so.  New journal types (fee, checkout_settlement,
// card_topup) use "fees" for consistency with the payments reference catalogue.
var feeClassifications = []ClassificationPreset{
	{Code: "fees", Name: "Fees", NormalSide: core.NormalSideCredit, IsSystem: true},
}

var feeJournalTypes = []JournalTypePreset{
	{Code: "fee", Name: "Fee Charge"},
}

// feeTemplates: DR main_wallet (user) CR fees (system)
var feeTemplates = []TemplatePreset{
	{
		Code:            "fee_charge",
		Name:            "Fee Charge",
		JournalTypeCode: "fee",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "main_wallet", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 1},
			{ClassificationCode: "fees", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 2},
		},
	},
}

// FeeBundle returns the classifications, journal types, and templates
// required to post first-class fee charge journals.
func FeeBundle() TemplateBundle {
	return TemplateBundle{
		Classifications: combineClassifications(sharedTemplateClassifications, feeClassifications),
		JournalTypes:    cloneJournalTypes(feeJournalTypes),
		Templates:       cloneTemplates(feeTemplates),
	}
}

// InstallFeeBundle installs the fee bundle. Safe to call repeatedly —
// existing rows are validated and reused.
func InstallFeeBundle(
	ctx context.Context,
	classifications core.ClassificationStore,
	journalTypes core.JournalTypeStore,
	templates core.TemplateStore,
) error {
	return InstallTemplateBundle(ctx, classifications, journalTypes, templates, FeeBundle())
}
