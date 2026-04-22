package presets

import "github.com/azex-ai/ledger/core"

// WithdrawalLifecycle is a preset classification lifecycle for withdrawal operations.
// States: locked -> reserved -> reviewing|processing -> confirmed | failed | expired
// Retry: failed -> reserved
var WithdrawalLifecycle = &core.Lifecycle{
	Initial:  "locked",
	Terminal: []core.Status{"confirmed", "failed", "expired"},
	Transitions: map[core.Status][]core.Status{
		"locked":     {"reserved"},
		"reserved":   {"reviewing", "processing"},
		"reviewing":  {"processing", "failed"},
		"processing": {"confirmed", "failed", "expired"},
		"failed":     {"reserved"},
	},
}
