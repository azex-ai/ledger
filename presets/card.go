package presets

import (
	"context"

	"github.com/azex-ai/ledger/core"
)

// cardClassifications introduces the card_account user-side classification.
// card_account is debit-normal (asset for the platform, represents user's card balance).
var cardClassifications = []ClassificationPreset{
	// card_account: debit-normal, user-side — mirrors the user's physical/virtual
	// card balance held at the card processor (e.g. Nium).
	{Code: "card_account", Name: "Card Account", NormalSide: core.NormalSideDebit},
}

var cardJournalTypes = []JournalTypePreset{
	{Code: "card_topup", Name: "Card Top-up"},
}

// cardTemplates defines the card top-up pattern.
//
// A card top-up moves funds from the user's main_wallet into card_account.
// The locked intermediary phase (lock → confirm/cancel) is handled by the
// withdrawal preset; the templates here cover the final settlement leg:
//
//	card_topup_settle: DR main_wallet (user) CR card_account (user)
//
// When a platform fee applies, use the fee-inclusive variant (fee_amount key):
//
//	card_topup_settle_net: DR main_wallet (user, gross)
//	                       CR card_account (user, net_amount)
//	                       CR fees (system, fee_amount)
var cardTemplates = []TemplatePreset{
	{
		// No-fee card top-up: full amount credited to card account.
		Code:            "card_topup_settle",
		Name:            "Card Top-up Settle",
		JournalTypeCode: "card_topup",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "main_wallet", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 1},
			{ClassificationCode: "card_account", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 2},
		},
	},
	{
		// Fee-inclusive card top-up: gross debited from wallet, net to card, fee to platform.
		// Invariant enforced by caller: gross_amount == net_amount + fee_amount.
		Code:            "card_topup_settle_net",
		Name:            "Card Top-up Settle (Net)",
		JournalTypeCode: "card_topup",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "main_wallet", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "gross_amount", SortOrder: 1},
			{ClassificationCode: "card_account", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleUser, AmountKey: "net_amount", SortOrder: 2},
			{ClassificationCode: "fees", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "fee_amount", SortOrder: 3},
		},
	},
}

// CardBundle returns the classifications, journal types, and templates
// required to post card top-up journals.
//
// This bundle depends on:
//   - "fees" classification (from FeeBundle / fee.go)
//   - "main_wallet" (from sharedTemplateClassifications)
//
// Install alongside FeeBundle, or call InstallExtendedPresets.
func CardBundle() TemplateBundle {
	return TemplateBundle{
		Classifications: combineClassifications(
			sharedTemplateClassifications,
			cardClassifications,
			[]ClassificationPreset{
				{Code: "fees", Name: "Fees", NormalSide: core.NormalSideCredit, IsSystem: true},
			},
		),
		JournalTypes: cloneJournalTypes(cardJournalTypes),
		Templates:    cloneTemplates(cardTemplates),
	}
}

// InstallCardBundle installs the card top-up bundle. Safe to call repeatedly —
// existing rows are validated and reused.
func InstallCardBundle(
	ctx context.Context,
	classifications core.ClassificationStore,
	journalTypes core.JournalTypeStore,
	templates core.TemplateStore,
) error {
	return InstallTemplateBundle(ctx, classifications, journalTypes, templates, CardBundle())
}
