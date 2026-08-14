package presets

import (
	"context"

	"github.com/azex-ai/ledger/core"
)

// DevCreditClassificationCode is the system-side counterparty every simulated
// top-up is booked against. Consumers reference it to report or reconcile the
// unbacked portion of their liabilities.
//
// It is deliberately NOT "custodial". Solvency is computed as
// GetSystemSideCustodialBalance (only classifications whose code is
// "custodial") minus GetTotalUserSideBalance (every user-side balance, no
// matter which classification carries it) — see
// postgres/sql/queries/platform_balances.sql. Booking simulated credit
// against its own classification therefore makes /platform/solvency report
// the shortfall honestly, and the shortfall equals this account's balance:
// money was promised to a holder that no custodied asset backs.
//
// Reusing "custodial" (or routing simulated credit through the deposit
// templates) would instead inflate the asset side by the same amount and
// leave the platform looking solvent while it is not.
const DevCreditClassificationCode = "dev_credit"

// DevCreditJournalTypeCode / DevCreditTemplateCode identify the journal type
// and entry template a simulated top-up posts under. Every such journal is
// therefore filterable out of deposit reporting by journal type alone.
const (
	DevCreditJournalTypeCode = "dev_credit"
	DevCreditTemplateCode    = "dev_credit"
)

// devCreditClassifications introduces the developer-credit system account.
// Credit-normal and system-side, mirroring custodial: a positive balance is
// the running total of credit issued without backing.
var devCreditClassifications = []ClassificationPreset{
	{
		Code:       DevCreditClassificationCode,
		Name:       "Developer Credit",
		NormalSide: core.NormalSideCredit,
		IsSystem:   true,
	},
}

var devCreditJournalTypes = []JournalTypePreset{
	// DisplayLabel is what the holder wallet surface renders for these
	// journals. It stays a neutral, user-facing phrase rather than
	// "Developer credit" — the holder view must not narrate how the balance
	// was produced (~/.claude/rules/user-facing-surfaces.md). The internal
	// name and the classification code carry that fact for operators.
	{Code: DevCreditJournalTypeCode, Name: "Developer Credit", DisplayLabel: "Credit adjustment"},
}

// devCreditTemplates books a simulated top-up as
//
//	dev_credit: DR main_wallet (user) CR dev_credit (system)
//
// which is the deposit_confirm shape with the custodial leg swapped for the
// unbacked counterparty. The credited funds land in main_wallet, so they
// carry BalanceRoleAvailable and are immediately spendable and reservable —
// the point of the feature is that the holder can exercise real downstream
// flows against them.
var devCreditTemplates = []TemplatePreset{
	{
		Code:            DevCreditTemplateCode,
		Name:            "Developer Credit",
		JournalTypeCode: DevCreditJournalTypeCode,
		Lines: []TemplateLinePreset{
			{ClassificationCode: "main_wallet", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 1},
			{ClassificationCode: DevCreditClassificationCode, EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 2},
		},
	},
}

// DevCreditBundle returns the classification, journal type, and template that
// let a deployment credit a holder without a corresponding custodied asset —
// the accounting half of a developer/sandbox "simulate a deposit" facility.
//
// Deliberately excluded from InstallExtendedPresets: installing every other
// bundle must never pull this one in as a side effect. A deployment opts in
// by calling InstallDevCreditBundle explicitly, which keeps "can this
// environment mint balance out of nothing?" a single greppable decision in
// the composition root.
//
// The journals it produces are ordinary journals: append-only, and reversed
// through the normal reversal path (POST /journals/{uid}/reverse), never
// deleted. Simulated credit is real credit — only its backing differs.
func DevCreditBundle() TemplateBundle {
	return TemplateBundle{
		Classifications: combineClassifications(sharedTemplateClassifications, devCreditClassifications),
		JournalTypes:    cloneJournalTypes(devCreditJournalTypes),
		Templates:       cloneTemplates(devCreditTemplates),
	}
}

// InstallDevCreditBundle installs the developer-credit bundle. Safe to call
// repeatedly — existing rows are validated and reused.
func InstallDevCreditBundle(
	ctx context.Context,
	classifications core.ClassificationStore,
	journalTypes core.JournalTypeStore,
	templates core.TemplateStore,
) error {
	return InstallTemplateBundle(ctx, classifications, journalTypes, templates, DevCreditBundle())
}
