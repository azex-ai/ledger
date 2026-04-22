package presets

import "github.com/azex-ai/ledger/core"

// DepositLifecycle is a preset classification lifecycle for deposit operations.
// States: pending -> confirming -> confirmed | failed | expired
var DepositLifecycle = &core.Lifecycle{
	Initial:  "pending",
	Terminal: []core.Status{"confirmed", "failed", "expired"},
	Transitions: map[core.Status][]core.Status{
		"pending":    {"confirming", "failed", "expired"},
		"confirming": {"confirmed", "failed"},
	},
}
