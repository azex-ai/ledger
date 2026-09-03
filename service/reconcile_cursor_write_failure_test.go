package service

// Pin for m4 of the 2026-09-03 independent review: a checkpoint scan that
// cannot write its resume cursor is INCOMPLETE, not FAILED.
//
// The report ran `ledger-cli reconcile --full` on the read-only credential and
// got:
//
//	{"name":"checkpoint_balance","passed":false,"complete":true,
//	 "findings":[{"description":"checkpoint scan complete: 0 account/currency pairs verified this run"},
//	             {"description":"checkpoint scan cursor reset failed",
//	              "detail":"... permission denied for table reconcile_scan_cursors (SQLSTATE 42501)"}]}
//
// with `full_coverage: true`. `passed:false` claims the ledger disagreed with
// itself; nothing disagreed. `complete:true` claims full coverage, which a run
// whose own bookkeeping write failed cannot know. The pair of them pushes an
// operator towards handing the investigation tool a write credential -- the
// opposite of what the credential split exists for.

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCheck2GlobalBalance_CursorWriteFailureIsIncompleteNotFailed covers the
// clean-lap branch: the scan itself found nothing wrong, and only the cursor
// reset failed.
func TestCheck2GlobalBalance_CursorWriteFailureIsIncompleteNotFailed(t *testing.T) {
	q := cleanQuerier()
	q.errSetScanCursor = errors.New(`ERROR: permission denied for table reconcile_scan_cursors (SQLSTATE 42501)`)
	svc := buildFullSvcForCheck2(t, nil, nil, nil, q, FullReconciliationConfig{})

	result := svc.runCheck2GlobalBalance(context.Background())

	assert.True(t, result.Passed,
		"the balances this run examined were fine; a failed bookkeeping write is not a disagreement in the ledger")
	assert.False(t, result.Complete,
		"a run that could not persist its resume position cannot claim to have covered the fleet -- and complete:true here is what made full_coverage true in the report m4 quotes")

	var sawCursorFinding bool
	for _, f := range result.Findings {
		if f.Description == "checkpoint scan cursor reset failed" {
			sawCursorFinding = true
			assert.Contains(t, f.Detail, "permission denied",
				"the underlying cause must survive into the finding -- an operator needs to know it was a credential, not corruption")
			assert.Contains(t, f.Detail, "coverage cannot be claimed",
				"the finding must say what the failure means, not only what failed")
		}
	}
	require.True(t, sawCursorFinding, "the failure must be reported, not swallowed: %+v", result.Findings)
}

// TestFullReconciliation_CursorWriteFailureDropsFullCoverage is the half that
// matters to whoever reads the report rather than the check: `full_coverage`
// must go false, because that is the field an operator scans for.
func TestFullReconciliation_CursorWriteFailureDropsFullCoverage(t *testing.T) {
	q := cleanQuerier()
	q.errSetScanCursor = errors.New(`ERROR: permission denied for table reconcile_scan_cursors (SQLSTATE 42501)`)
	svc := buildFullSvc(t, balancedGlobalSummer(), q, FullReconciliationConfig{})

	report, err := svc.RunFullReconciliation(context.Background())
	require.NoError(t, err)
	assert.False(t, report.FullCoverage,
		"a check that could not establish its coverage must drag full_coverage down with it")
}

// TestCheck2GlobalBalance_CursorWriteSucceedsStillReportsComplete is the
// control: the change must not make every clean run look incomplete.
func TestCheck2GlobalBalance_CursorWriteSucceedsStillReportsComplete(t *testing.T) {
	svc := buildFullSvcForCheck2(t, nil, nil, nil, cleanQuerier(), FullReconciliationConfig{})

	result := svc.runCheck2GlobalBalance(context.Background())
	assert.True(t, result.Passed)
	assert.True(t, result.Complete, "with the cursor written, coverage is claimable exactly as before")
}
