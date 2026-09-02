package presets

import (
	"context"

	"github.com/azex-ai/ledger/core"
)

// settlementJournalTypes introduces the checkout_settlement journal type.
// The "settlement" classification is defined in transfer.go because it is
// also used by the transfer bundle; importing it here would create a duplicate.
// Both bundles share the same classification declaration via combineClassifications.
// HolderTxKindDeposit: from the receiving holder's perspective this is
// external funds landing in their spendable balance, the same shape as a
// deposit — DisplayLabel ("Payment") carries the more specific product
// wording; HolderKind carries the coarse bucket.
var settlementJournalTypes = []JournalTypePreset{
	{Code: "checkout_settlement", Name: "Checkout Settlement", DisplayLabel: "Payment", HolderKind: core.HolderTxKindDeposit},
}

// settlementTemplates: merchant settlement with an optional fee leg.
//
// #### The one reading of "checkout settlement" this package has ####
//
// A customer outside the platform pays gross. The merchant (the holder these
// templates are executed for) is credited net. The platform keeps fee.
// gross == net + fee.
//
// Before 2026-09-02 the repository carried three mutually exclusive readings
// of this template -- this header said the merchant was being debited, the
// journal type said HolderTxKindDeposit ("funds landing in their spendable
// balance"), and presets/transfer.go said "the holder is paying" -- and the
// code agreed with none of them (audit A-M2). This is now the only reading;
// the other two have been deleted at their source.
//
// The journal type declares HolderTxKindDeposit and DisplayLabel "Payment",
// so the holder-facing row MUST read as money coming in
// (docs/INVARIANTS.md I-44). main_wallet is debit-normal, so "coming in" is
// a DEBIT of net_amount.
//
// #### Where gross goes ####
//
//	main_wallet  DR net_amount   (user)    merchant's claim  +net
//	custodial    CR net_amount   (system)  custody backing it +net
//	fee_expense  DR fee_amount   (user)    merchant's cost    +fee   (memo)
//	fees         CR fee_amount   (system)  platform revenue   +fee
//
//	sum(DR) = net + fee = gross = sum(CR)   ✓
//
// custodial rises by net, not by gross, and that is deliberate: this ledger
// keeps the custodial pool equal to exactly what is owed to holders and
// books the platform's own earnings in a revenue account instead -- the same
// split withdraw_fee has always used (presets/templates.go's withdraw_fee
// moves its fee out of custodial into fee_revenue). custodial + fees rises by
// gross, which is the cash that actually arrived, and SolvencyCheck's margin
// stays at zero through a settlement instead of drifting by one fee per
// transaction. postgres/solvency_test.go pins that.
//
// There is no leg for gross_amount and there cannot be one: expressing
// "custodial +gross AND fees +fee" needs 2*fee of debits with no account to
// put them against, because custodial and fees are both credit-normal (the
// audit's proposed shape does not balance). gross is implied by net + fee, so
// nothing is lost -- the ledger's per-currency balance check enforces the
// relationship structurally instead of relying on the caller to hold to it.
//
// Amount keys:
//   - "net_amount"   — merchant's net receipt
//   - "fee_amount"   — platform fee retained (checkout_settlement_net only)
var settlementTemplates = []TemplatePreset{
	{
		// Use when the platform fee is zero: the merchant receives the whole
		// gross amount, so there is no fee leg and no fee_expense memo.
		Code:            "checkout_settlement_gross",
		Name:            "Checkout Settlement (Gross)",
		JournalTypeCode: "checkout_settlement",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "main_wallet", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "gross_amount", SortOrder: 1},
			{ClassificationCode: "custodial", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "gross_amount", SortOrder: 2},
		},
	},
	{
		// Use when the platform fee > 0. Caller supplies net_amount and
		// fee_amount; gross is their sum.
		Code:            "checkout_settlement_net",
		Name:            "Checkout Settlement (Net)",
		JournalTypeCode: "checkout_settlement",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "main_wallet", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "net_amount", SortOrder: 1},
			{ClassificationCode: "fee_expense", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "fee_amount", SortOrder: 2},
			{ClassificationCode: "custodial", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "net_amount", SortOrder: 3},
			{ClassificationCode: "fees", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "fee_amount", SortOrder: 4},
		},
	},
}

// SettlementBundle returns the classifications, journal types, and templates
// required to post checkout-settlement journals.
//
// This bundle depends on:
//   - "settlement" classification (from TransferBundle / transfer.go)
//   - "fees" classification (from FeeBundle / fee.go)
//   - "fee_expense" classification (from WithdrawalBundle / templates.go)
//   - "custodial" + "main_wallet" (from sharedTemplateClassifications)
//
// Install all four bundles if you need the full accounting catalogue, or call
// InstallExtendedPresets which handles ordering automatically.
func SettlementBundle() TemplateBundle {
	return TemplateBundle{
		// Pulls in custodial + main_wallet via shared; adds settlement + fees inline
		// so the bundle is self-contained even when installed standalone.
		Classifications: combineClassifications(
			sharedTemplateClassifications,
			[]ClassificationPreset{
				{Code: "settlement", Name: "Settlement", NormalSide: core.NormalSideCredit, IsSystem: true},
				{Code: "fees", Name: "Fees", NormalSide: core.NormalSideCredit, IsSystem: true},
				// checkout_settlement_net books the merchant's fee to their
				// own memo tracker; declared here (identically to
				// withdrawalOnlyClassifications) so the bundle stays
				// self-contained when installed standalone.
				feeExpenseClassification,
			},
		),
		JournalTypes: cloneJournalTypes(settlementJournalTypes),
		Templates:    cloneTemplates(settlementTemplates),
	}
}

// InstallSettlementBundle installs the checkout settlement bundle. Safe to call
// repeatedly — existing rows are validated and reused.
func InstallSettlementBundle(
	ctx context.Context,
	classifications core.ClassificationStore,
	journalTypes core.JournalTypeStore,
	templates core.TemplateStore,
) error {
	return InstallTemplateBundle(ctx, classifications, journalTypes, templates, SettlementBundle())
}
