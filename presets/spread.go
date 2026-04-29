package presets

import (
	"context"

	"github.com/azex-ai/ledger/core"
)

// spreadClassifications introduces the spread system revenue account.
// spread is credit-normal — records FX spread, swap markup, or other
// price-differential revenue earned by the platform.
var spreadClassifications = []ClassificationPreset{
	{Code: "spread", Name: "Spread", NormalSide: core.NormalSideCredit, IsSystem: true},
}

// SpreadBundle returns just the "spread" classification. There is no dedicated
// journal type for spread because spread revenue is typically recorded as part
// of a swap or checkout journal (see payments/backend BuildSpreadRevenueEntries).
// Callers that need a standalone spread journal type can register one at runtime
// and post via PostJournal.
func SpreadBundle() TemplateBundle {
	return TemplateBundle{
		Classifications: cloneClassifications(spreadClassifications),
		JournalTypes:    nil,
		Templates:       nil,
	}
}

// InstallSpreadBundle installs only the "spread" classification. Safe to call
// repeatedly — existing rows are validated and reused.
func InstallSpreadBundle(
	ctx context.Context,
	classifications core.ClassificationStore,
	journalTypes core.JournalTypeStore,
	templates core.TemplateStore,
) error {
	return InstallTemplateBundle(ctx, classifications, journalTypes, templates, SpreadBundle())
}

// cloneClassifications returns a shallow copy of a ClassificationPreset slice.
func cloneClassifications(in []ClassificationPreset) []ClassificationPreset {
	out := make([]ClassificationPreset, len(in))
	copy(out, in)
	return out
}
