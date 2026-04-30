package presets

import (
	"context"

	"github.com/azex-ai/ledger/core"
)

// FX (foreign exchange) is a per-currency template pair, not a single
// multi-currency template. Each currency balances independently; the
// `settlement` system classification absorbs the platform's net FX exposure.
//
// Caller workflow for converting CCY-A → CCY-B at a known rate:
//
//	1. Execute fx_sell with currency_id=CCY-A, amounts={"amount": <CCY-A qty>}
//	2. Execute fx_buy  with currency_id=CCY-B, amounts={"amount": <CCY-B qty>}
//
// Both calls share the same metadata (e.g. fx_rate, quote_id, request_uid)
// and ideally the same idempotency-key root (with -sell / -buy suffixes), so
// audit can stitch the two legs back together.
//
// Why two templates instead of one four-entry template? The current template
// renderer applies a single currency_id to every line; FX is by definition
// cross-currency. Keeping each leg single-currency lets per-currency balance
// validation (DB trigger + Go validator) catch any rate-quote bug — neither
// leg can be unbalanced and silently pass.
//
// Net effect on system books after both legs settle:
//
//	settlement (CCY-A): +qtyA   ← platform owes / holds in CCY-A
//	settlement (CCY-B): -qtyB   ← platform consumed in CCY-B
//
// Reconciling settlement balances against external custody figures is the
// caller's responsibility — the ledger only records what was promised, not
// where the inventory physically lives.

var fxJournalTypes = []JournalTypePreset{
	{Code: "fx_sell", Name: "FX Sell Leg"},
	{Code: "fx_buy", Name: "FX Buy Leg"},
}

// fx_sell — user gives up their CCY-A; platform settlement pool absorbs it.
//
//	DR main_wallet (user)        CR settlement (system)
//
// fx_buy — platform settlement pool releases CCY-B; user receives it.
//
//	DR settlement (system)       CR main_wallet (user)
var fxTemplates = []TemplatePreset{
	{
		Code:            "fx_sell",
		Name:            "FX Sell",
		JournalTypeCode: "fx_sell",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "main_wallet", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 1},
			{ClassificationCode: "settlement", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 2},
		},
	},
	{
		Code:            "fx_buy",
		Name:            "FX Buy",
		JournalTypeCode: "fx_buy",
		Lines: []TemplateLinePreset{
			{ClassificationCode: "settlement", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 1},
			{ClassificationCode: "main_wallet", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 2},
		},
	},
}

// FXBundle returns classifications, journal types, and templates required
// for cross-currency conversion. main_wallet + settlement classifications
// are pulled in via shared / transfer presets when those bundles are also
// installed; this bundle is safe to compose with them.
func FXBundle() TemplateBundle {
	return TemplateBundle{
		Classifications: combineClassifications(
			sharedTemplateClassifications,
			[]ClassificationPreset{
				{Code: "settlement", Name: "Settlement", NormalSide: core.NormalSideCredit, IsSystem: true},
			},
		),
		JournalTypes: cloneJournalTypes(fxJournalTypes),
		Templates:    cloneTemplates(fxTemplates),
	}
}

// InstallFXBundle installs the FX bundle. Idempotent; safe alongside
// transfer / settlement bundles that share the settlement classification.
func InstallFXBundle(
	ctx context.Context,
	classifications core.ClassificationStore,
	journalTypes core.JournalTypeStore,
	templates core.TemplateStore,
) error {
	return InstallTemplateBundle(ctx, classifications, journalTypes, templates, FXBundle())
}
