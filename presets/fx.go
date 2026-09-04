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
// ATOMICITY: the two journals MUST land in one transaction — pass both to
// ExecuteTemplateBatch, or run two ExecuteTemplate calls inside one RunInTx
// (docs/COOKBOOK.md "FX"). Executed as two independent calls, a failure
// after the first journal leaves the user's CCY-A gone with no CCY-B credited
// and only a dangling settlement balance to show for it — and there is no
// repair API other than manually reversing the surviving journal.
//
// Both calls share the same metadata (e.g. fx_rate, quote_id, request_uid)
// and ideally the same idempotency-key root (with -sell / -buy suffixes), so
// audit can stitch the two journals back together.
//
// Why two templates instead of one four-entry template? The current template
// renderer applies a single currency_id to every line; FX is by definition
// cross-currency, so one template cannot express both sides.
//
// ⚠️ WHAT THE LEDGER DOES NOT CHECK. Each journal is single-currency and balances
// within itself for ANY amount, so per-currency balance validation (DB
// trigger + Go validator) says nothing whatsoever about the rate: quote CCY-B
// at 100x the correct figure and both journals still pass every check. The ledger
// does not know fx_sell and fx_buy are related, does not verify
// qtyB == round(qtyA * rate), and — as the ATOMICITY paragraph above says —
// does not guarantee both journals land. Rate correctness and cross-journal
// atomicity are entirely the caller's responsibility; the ledger records what
// it was told. (An earlier version of this comment claimed the balance
// validation would "catch any rate-quote bug". It cannot, and
// postgres.TestFX_LedgerDoesNotCheckTheRate now pins that both journals of a
// 100x-wrong quote are accepted, so the absence of that check is an explicit
// contract rather than an assumption someone can read back into the code.)
//
// Net effect on system books after both journals settle:
//
//	settlement (CCY-A): -qtyA   ← platform absorbed the user's CCY-A sale
//	settlement (CCY-B): +qtyB   ← platform absorbed the user's CCY-B purchase
//
// (settlement is credit-normal, presets/transfer.go; fx_sell debits it,
// fx_buy credits it — same polarity as transfer_out/transfer_in's shared
// transit account, which nets to zero across a matched pair. The FX journals are
// cross-currency, so CCY-A and CCY-B never net against each other; each
// stays on the books as the platform's per-currency FX exposure.)
//
// Reconciling settlement balances against external custody figures is the
// caller's responsibility — the ledger only records what was promised, not
// where the inventory physically lives.
//
// SolvencyCheck DOES count settlement as part of the custodial position (it
// is in PlatformBalanceStore's default custodial class-code set), which is
// what keeps a healthy cross-currency position from reporting permanent
// insolvency on the bought currency: settlement(CCY-B) = +qtyB is exactly the
// asset backing the qtyB now owed to the holder. Before 2026-09-02 the scope
// was the hardcoded literal 'custodial' and every FX deployment read
// solvent=false on every currency it bought, forever (audit A-M6).

// HolderTxKindOther: a cross-currency conversion is neither money arriving
// from outside the platform (deposit), leaving it (withdrawal), moving
// between two holders (transfer), a platform charge (fee), nor a correction
// (adjustment) — it is its own thing. HolderTxKindOther is the honest
// bucket rather than forcing a fit; a deployment that wants a dedicated
// "exchange" kind can add one later (additive, expand-safe — see
// core.HolderTxKind's doc comment).
// TODO(w5-term-entry): Name still reads "FX Sell/Buy Leg" — this is persisted
// data (journal_types.name), not prose. Renaming it here would make a fresh
// install disagree with what an already-installed deployment's row says,
// which InstallTemplatePresets treats as a mismatch to validate against, not
// something safe to silently drift. Leaving it as "Leg" pending a decision on
// whether it's worth an expand/migrate/contract to "Journal" (deployment.md).
var fxJournalTypes = []JournalTypePreset{
	{Code: "fx_sell", Name: "FX Sell Leg", DisplayLabel: "Exchange", HolderKind: core.HolderTxKindOther},
	{Code: "fx_buy", Name: "FX Buy Leg", DisplayLabel: "Exchange", HolderKind: core.HolderTxKindOther},
}

// fx_sell — user gives up their CCY-A; platform settlement pool absorbs it.
//
//	CR main_wallet (user)        DR settlement (system)
//
// fx_buy — platform settlement pool releases CCY-B; user receives it.
//
//	CR settlement (system)       DR main_wallet (user)
//
// main_wallet is debit-normal (presets/templates.go): CR decreases it (user
// gives up CCY-A in fx_sell), DR increases it (user receives CCY-B in
// fx_buy) — same polarity as transfer_out/transfer_in (presets/transfer.go).
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
