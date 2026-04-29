package presets

import (
	"context"

	"github.com/azex-ai/ledger/core"
)

// settlementJournalTypes introduces the checkout_settlement journal type.
// The "settlement" classification is defined in transfer.go because it is
// also used by the transfer bundle; importing it here would create a duplicate.
// Both bundles share the same classification declaration via combineClassifications.
var settlementJournalTypes = []JournalTypePreset{
	{Code: "checkout_settlement", Name: "Checkout Settlement"},
}

// settlementTemplates: merchant settlement with optional fee leg.
//
// A single template cannot conditionally include entries, so we provide two
// templates:
//   - checkout_settlement_gross: 2-leg, no fee (when platform fee is zero)
//   - checkout_settlement_net:   3-leg, with fee (gross amount split to merchant + fees)
//
// Amount keys:
//   - "gross_amount" — total received from customer, debited from custodial
//   - "net_amount"   — merchant's net receipt, credited to main_wallet
//   - "fee_amount"   — platform fee retained, credited to fees
//
// Invariant enforced by callers: gross_amount == net_amount + fee_amount.
var settlementTemplates = []TemplatePreset{
	{
		// Use when platform fee is zero.
		Code:            "checkout_settlement_gross",
		Name:            "Checkout Settlement (Gross)",
		JournalTypeCode: "checkout_settlement",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "custodial", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleSystem, AmountKey: "gross_amount", SortOrder: 1},
			{ClassificationCode: "main_wallet", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleUser, AmountKey: "gross_amount", SortOrder: 2},
		},
	},
	{
		// Use when platform fee > 0. Caller must supply gross_amount, net_amount, fee_amount.
		Code:            "checkout_settlement_net",
		Name:            "Checkout Settlement (Net)",
		JournalTypeCode: "checkout_settlement",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "custodial", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleSystem, AmountKey: "gross_amount", SortOrder: 1},
			{ClassificationCode: "main_wallet", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleUser, AmountKey: "net_amount", SortOrder: 2},
			{ClassificationCode: "fees", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "fee_amount", SortOrder: 3},
		},
	},
}

// SettlementBundle returns the classifications, journal types, and templates
// required to post checkout-settlement journals.
//
// This bundle depends on:
//   - "settlement" classification (from TransferBundle / transfer.go)
//   - "fees" classification (from FeeBundle / fee.go)
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
