package service

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// TestWorker_RuntimeRoleWarning_EmptyByDefault is the control for the test
// below: a Worker built through NewWorker directly (bypassing
// ledger.Service.Worker, the only caller of SetRuntimeRoleWarning) never has
// it set, so StartupReport must not invent one.
func TestWorker_RuntimeRoleWarning_EmptyByDefault(t *testing.T) {
	worker := NewWorker(nil, nil, nil, nil, nil, WorkerConfig{}, core.NewEngine())

	report := worker.StartupReport()
	assert.Empty(t, report.RuntimeRoleWarning)
	for _, w := range report.Warnings {
		assert.NotContains(t, w, "role", "no warning should mention a runtime-role mismatch when SetRuntimeRoleWarning was never called: %v", report.Warnings)
	}
}

// TestWorker_SetRuntimeRoleWarning_AppearsInStartupReport is the pin missing
// from the original W5-readme delivery (team-lead review, 2026-09-04):
// SetRuntimeRoleWarning existed, ledger.Service.Worker called it, and
// StartupReport surfaced it -- but nothing exercised the wiring, so a
// regression (e.g. StartupReport forgetting to read w.runtimeRoleWarning)
// would have shipped with every other test still green.
func TestWorker_SetRuntimeRoleWarning_AppearsInStartupReport(t *testing.T) {
	worker := NewWorker(nil, nil, nil, nil, nil, WorkerConfig{}, core.NewEngine())

	roleErr := fmt.Errorf("postgres: role check: connected as %q, expected %q -- the ACL-enforced "+
		"invariants (I-22, I-42, and the append-only guards) constrain %q and nothing else, so on this "+
		"connection they are not in force: %w", "ledger_owner", "ledger_app", "ledger_app", core.ErrInvalidInput)
	worker.SetRuntimeRoleWarning(roleErr)

	report := worker.StartupReport()
	require.NotEmpty(t, report.RuntimeRoleWarning, "StartupReport.RuntimeRoleWarning must reflect SetRuntimeRoleWarning")
	assert.Equal(t, roleErr.Error(), report.RuntimeRoleWarning)

	assert.Contains(t, report.Warnings, "worker: "+roleErr.Error(),
		"the role mismatch must reach Warnings the same way every other degraded mode does -- data a "+
			"caller can read from StartupReport(), not only a log line: %v", report.Warnings)

	// nil clears it (StartupReport's own doc: "nil clears it, the check
	// passed") -- pinned so a future SetRuntimeRoleWarning(nil) call cannot
	// leave a stale warning behind.
	worker.SetRuntimeRoleWarning(nil)
	report = worker.StartupReport()
	assert.Empty(t, report.RuntimeRoleWarning)
	for _, w := range report.Warnings {
		assert.NotContains(t, w, roleErr.Error())
	}
}
