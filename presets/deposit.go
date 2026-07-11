package presets

import "github.com/azex-ai/ledger/core"

// DepositLifecycle is a preset classification lifecycle for deposit operations.
// States: pending -> confirming -> confirmed | failed | expired | review;
// review -> confirmed | failed (design doc §9.1: M3 compensating controls --
// review is a non-terminal holding state a confirming deposit is routed to
// instead of confirmed when the threshold gate or reconciliation gate trips;
// only a human calling service.Onchain.ApproveReview/RejectReview moves it
// out. This is an expand-only change over the pre-M3 lifecycle (design doc
// §9.6): "review" is a new state and a new pair of edges, nothing existing
// is removed, so bookings already in flight under the old lifecycle are
// unaffected.
var DepositLifecycle = &core.Lifecycle{
	Initial:  "pending",
	Terminal: []core.Status{"confirmed", "failed", "expired"},
	Transitions: map[core.Status][]core.Status{
		"pending":    {"confirming", "failed", "expired"},
		"confirming": {"confirmed", "failed", "review"},
		"review":     {"confirmed", "failed"},
	},
}
