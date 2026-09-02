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
// accounting may continue to do so.  New journal types (fee, checkout_settlement)
// use "fees" for consistency with the payments reference catalogue.
var feeClassifications = []ClassificationPreset{
	{Code: "fees", Name: "Fees", NormalSide: core.NormalSideCredit, IsSystem: true},
}

var feeJournalTypes = []JournalTypePreset{
	{Code: "fee", Name: "Fee Charge", DisplayLabel: "Fee", HolderKind: core.HolderTxKindFee},
}

// feeTemplates: fee_charge, the four-leg direct fee.
//
//	fee_expense  DR amount  (user)    holder's cost tracker  +amount  (memo)
//	main_wallet  CR amount  (user)    holder's balance       -amount
//	custodial    DR amount  (system)  custody pool           -amount
//	fees         CR amount  (system)  platform revenue       +amount
//
//	sum(DR) = 2*amount = sum(CR)   ✓
//
// The holder leg follows main_wallet's declared polarity (NormalSideDebit): a
// fee is money leaving the holder, so it credits. It used to debit, which
// added the fee to the payer's balance instead of taking it.
//
// #### Why this is four legs and not two ####
//
// The 2026-08-25 audit fixed the holder leg and left the counterpart's
// direction open, on the reading that "how the fee account reads" was a
// presentation choice. It was not. fees is credit-normal (feeClassifications
// below), so revenue accumulates on CR -- that is what
// checkout_settlement_net does. Debiting it here made the same account count
// down, and the 2026-09-02 audit measured the consequence: two 30-unit fees
// collected through the two different paths summed to ZERO (audit A-M4).
// Aggregate fee income read as checkout_fees - direct_fees, which is not
// revenue and not any other meaningful quantity.
//
// Crediting fees requires a second debit, because main_wallet's leg is also a
// credit. That debit pair is the holder's memo cost tracker and the custodial
// pool -- exactly the shape withdraw_fee has always had
// (presets/templates.go), with fees standing in for fee_revenue and
// main_wallet for locked. Keeping the two fee paths structurally identical is
// the point: it is what makes "the platform earned a fee" a single accounting
// story regardless of which template recorded it, and it holds margin at zero
// (postgres/solvency_test.go).
var feeTemplates = []TemplatePreset{
	{
		Code:            "fee_charge",
		Name:            "Fee Charge",
		JournalTypeCode: "fee",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "fee_expense", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 1},
			{ClassificationCode: "custodial", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 2},
			{ClassificationCode: "main_wallet", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 3},
			{ClassificationCode: "fees", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 4},
		},
	},
}

// FeeBundle returns the classifications, journal types, and templates
// required to post first-class fee charge journals.
func FeeBundle() TemplateBundle {
	return TemplateBundle{
		// fee_expense (the payer's memo cost tracker) is part of the four-leg
		// fee_charge shape, so the bundle carries it even when installed
		// without the withdrawal bundle.
		Classifications: combineClassifications(
			sharedTemplateClassifications,
			feeClassifications,
			[]ClassificationPreset{feeExpenseClassification},
		),
		JournalTypes: cloneJournalTypes(feeJournalTypes),
		Templates:    cloneTemplates(feeTemplates),
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
