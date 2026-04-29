package presets

import (
	"context"

	"github.com/azex-ai/ledger/core"
)

// transferClassifications are the classifications exclusive to the transfer bundle.
// main_wallet and custodial are shared and pulled in via sharedTemplateClassifications
// when both bundles are installed together.
var transferClassifications = []ClassificationPreset{
	// settlement: credit-normal system account that acts as an intermediary
	// for P2P transfers, ensuring each leg is independently settled and
	// the double-entry constraint is never violated mid-flight.
	{Code: "settlement", Name: "Settlement", NormalSide: core.NormalSideCredit, IsSystem: true},
}

var transferJournalTypes = []JournalTypePreset{
	{Code: "transfer", Name: "User-to-user Transfer"},
}

// transferTemplates defines the four-leg transfer pattern:
//
//	DR main_wallet (sender)    CR settlement (system/sender)   — sender leg out
//	DR settlement (system/receiver) CR main_wallet (receiver)  — receiver leg in
//
// Note: In the ledger template model, HolderRoleUser resolves to the HolderID
// supplied at execution time. Both sender and receiver legs must therefore be
// executed as two separate template calls, one per user, with amount_key
// "amount" on each side.
//
// Alternatively, callers may use PostJournal directly with all four entries
// when they need to express both legs atomically in a single journal. The
// template here covers the simpler two-leg call (one holder at a time).
//
// Template: transfer_out — sender side:
//
//	DR main_wallet (user=sender)  CR settlement (system derived from sender)
//
// Template: transfer_in — receiver side:
//
//	DR settlement (system derived from receiver)  CR main_wallet (user=receiver)
var transferTemplates = []TemplatePreset{
	{
		Code:            "transfer_out",
		Name:            "Transfer Out",
		JournalTypeCode: "transfer",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "main_wallet", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 1},
			{ClassificationCode: "settlement", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 2},
		},
	},
	{
		Code:            "transfer_in",
		Name:            "Transfer In",
		JournalTypeCode: "transfer",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "settlement", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 1},
			{ClassificationCode: "main_wallet", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 2},
		},
	},
}

// TransferBundle returns the classifications, journal types, and templates
// required to post P2P transfer journals.
func TransferBundle() TemplateBundle {
	return TemplateBundle{
		Classifications: combineClassifications(sharedTemplateClassifications, transferClassifications),
		JournalTypes:    cloneJournalTypes(transferJournalTypes),
		Templates:       cloneTemplates(transferTemplates),
	}
}

// InstallTransferBundle installs the transfer bundle. Safe to call repeatedly —
// existing rows are validated and reused.
func InstallTransferBundle(
	ctx context.Context,
	classifications core.ClassificationStore,
	journalTypes core.JournalTypeStore,
	templates core.TemplateStore,
) error {
	return InstallTemplateBundle(ctx, classifications, journalTypes, templates, TransferBundle())
}
