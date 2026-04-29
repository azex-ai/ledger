package presets

import (
	"context"

	"github.com/azex-ai/ledger/core"
)

// pendingOnlyClassifications are the classifications exclusive to the pending
// balance bundle.  main_wallet, suspense, and custodial are shared with the
// deposit bundle and will be pulled in via sharedTemplateClassifications if
// InstallPendingBundle is called alongside InstallDefaultTemplatePresets.
var pendingOnlyClassifications = []ClassificationPreset{
	// pending: credit-normal — user's in-flight deposit balance.
	{Code: "pending", Name: "Pending", NormalSide: core.NormalSideCredit},
	// suspense: debit-normal — system counterpart holding unconfirmed funds.
	{Code: "suspense", Name: "Suspense", NormalSide: core.NormalSideDebit, IsSystem: true},
}

// pendingBundleJournalTypes are the three journal types used by the two-phase
// pending pattern.  deposit_record_overage / deposit_resolve_overage are NOT
// included here — they belong to the tolerance plan, not the core pending API.
var pendingBundleJournalTypes = []JournalTypePreset{
	{Code: "deposit_pending", Name: "Deposit Pending"},
	{Code: "deposit_confirm_pending", Name: "Deposit Confirm Pending"},
	{Code: "deposit_release_pending", Name: "Deposit Release Pending"},
}

// pendingBundleTemplates defines the three entry templates that drive the
// pending two-phase pattern.
//
//	deposit_pending:         DR suspense (system)  CR pending (user)
//	deposit_confirm_pending: DR pending (user) + DR main_wallet (user)
//	                         CR suspense (system) + CR custodial (system)
//	deposit_release_pending: DR pending (user)     CR suspense (system)
var pendingBundleTemplates = []TemplatePreset{
	{
		Code:            "deposit_pending",
		Name:            "Deposit Pending",
		JournalTypeCode: "deposit_pending",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "suspense", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 1},
			{ClassificationCode: "pending", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 2},
		},
	},
	{
		Code:            "deposit_confirm_pending",
		Name:            "Deposit Confirm Pending",
		JournalTypeCode: "deposit_confirm_pending",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "pending", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 1},
			{ClassificationCode: "main_wallet", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 2},
			{ClassificationCode: "suspense", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 3},
			{ClassificationCode: "custodial", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 4},
		},
	},
	{
		Code:            "deposit_release_pending",
		Name:            "Deposit Release Pending",
		JournalTypeCode: "deposit_release_pending",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "pending", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 1},
			{ClassificationCode: "suspense", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 2},
		},
	},
}

// PendingBundle returns the minimal TemplateBundle required to run the
// two-phase pending balance API.  It includes:
//   - suspense + pending + main_wallet + custodial classifications
//   - deposit_pending / deposit_confirm_pending / deposit_release_pending journal types
//   - the three matching templates
//
// Install it with InstallTemplateBundle or use InstallPendingBundle for the
// one-shot convenience wrapper.
func PendingBundle() TemplateBundle {
	return TemplateBundle{
		Classifications: combineClassifications(
			sharedTemplateClassifications, // main_wallet, suspense, custodial
			pendingOnlyClassifications,    // pending
		),
		JournalTypes: cloneJournalTypes(pendingBundleJournalTypes),
		Templates:    cloneTemplates(pendingBundleTemplates),
	}
}

// InstallPendingBundle installs only the classifications, journal types, and
// templates required by the two-phase pending balance API.  It is idempotent —
// safe to call multiple times.
//
// If you are already calling InstallDefaultTemplatePresets you do NOT need to
// call this — the pending bundle is a strict subset of the default presets.
func InstallPendingBundle(
	ctx context.Context,
	classifications core.ClassificationStore,
	journalTypes core.JournalTypeStore,
	templates core.TemplateStore,
) error {
	return InstallTemplateBundle(ctx, classifications, journalTypes, templates, PendingBundle())
}

// cloneTemplates returns a shallow copy of a TemplatePreset slice.
func cloneTemplates(in []TemplatePreset) []TemplatePreset {
	out := make([]TemplatePreset, len(in))
	copy(out, in)
	return out
}
