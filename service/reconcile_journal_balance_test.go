package service

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// Pins the service-layer wiring of runCheck11JournalBalance (M1 fix): a
// clean querier reports Passed/Complete true; a querier reporting
// violations reports Passed false with Findings describing the count, and
// never puts the internal journal_id/currency_id in a Finding (I-18) --
// see TestFullReconciliation_JournalBalance_LeaksNoInternalIDs below for
// that half of the contract.
func TestFullReconciliation_JournalBalance_CleanReportsPass(t *testing.T) {
	svc := buildFullSvc(t, balancedGlobalSummer(), cleanQuerier(), FullReconciliationConfig{})
	report, err := svc.RunFullReconciliation(context.Background())
	require.NoError(t, err)

	check := findCheckInReport(t, report, "journal_dr_cr")
	assert.True(t, check.Passed)
	assert.True(t, check.Complete)
	assert.Empty(t, check.Findings)
}

// TestFullReconciliation_JournalBalance_DetectsPerJournalDrift pins that the
// "journal_dr_cr" check (now the genuine per-journal check, not the global
// equality it used to mean) reports the violations a querier surfaces.
func TestFullReconciliation_JournalBalance_DetectsPerJournalDrift(t *testing.T) {
	q := cleanQuerier()
	q.unbalancedJournals = []UnbalancedJournal{
		{JournalID: 501, CurrencyID: 1, Drift: decimal.NewFromInt(50)},
		{JournalID: 502, CurrencyID: 1, Drift: decimal.NewFromInt(-50)},
	}

	svc := buildFullSvc(t, balancedGlobalSummer(), q, FullReconciliationConfig{})
	report, err := svc.RunFullReconciliation(context.Background())
	require.NoError(t, err)

	check := findCheckInReport(t, report, "journal_dr_cr")
	assert.False(t, check.Passed)
	assert.True(t, check.Complete, "this scan is a single aggregate query, not a partial/capped scan")
	require.NotEmpty(t, check.Findings)

	// The global equality check ("global_dr_cr_equality") is unaffected --
	// the two checks are independent (M1: neither substitutes for the
	// other). balancedGlobalSummer reports a clean global equation, so it
	// must still pass even though the per-journal check fails.
	global := findCheckInReport(t, report, "global_dr_cr_equality")
	assert.True(t, global.Passed, "the global equality check must stay independent of the per-journal check")

	assert.False(t, report.OverallPassed, "a per-journal violation must sink OverallPassed")
}

// TestFullReconciliation_JournalBalance_LeaksNoInternalIDs pins I-18: the
// internal journal_id/currency_id from a violation sample must never appear
// verbatim in a public Finding string (the report is returned by the API
// verbatim); they belong in server logs only.
func TestFullReconciliation_JournalBalance_LeaksNoInternalIDs(t *testing.T) {
	q := cleanQuerier()
	const sentinelJournalID = 918273
	q.unbalancedJournals = []UnbalancedJournal{
		{JournalID: sentinelJournalID, CurrencyID: 1, Drift: decimal.NewFromInt(50)},
	}

	svc := buildFullSvc(t, balancedGlobalSummer(), q, FullReconciliationConfig{})
	report, err := svc.RunFullReconciliation(context.Background())
	require.NoError(t, err)

	check := findCheckInReport(t, report, "journal_dr_cr")
	for _, f := range check.Findings {
		assert.NotContains(t, f.Description, "918273", "internal journal_id must not leak into a public Finding")
		assert.NotContains(t, f.Detail, "918273", "internal journal_id must not leak into a public Finding")
	}
}

// TestFullReconciliation_JournalBalance_QueryErrorIsReported pins that a
// querier error fails the check rather than being swallowed.
func TestFullReconciliation_JournalBalance_QueryErrorIsReported(t *testing.T) {
	q := cleanQuerier()
	q.errUnbalancedCount = assertAnError{}

	svc := buildFullSvc(t, balancedGlobalSummer(), q, FullReconciliationConfig{})
	report, err := svc.RunFullReconciliation(context.Background())
	require.NoError(t, err)

	check := findCheckInReport(t, report, "journal_dr_cr")
	assert.False(t, check.Passed)
	require.NotEmpty(t, check.Findings)
}

type assertAnError struct{}

func (assertAnError) Error() string { return "forced query failure" }

// findCheckInReport is the package-internal twin of the service_test
// package's findCheck helper (reconcile_full_integration_test.go) -- kept
// separate because that one lives in package service_test and this file is
// package service (needs access to unexported mockReconcileQuerier).
func findCheckInReport(t *testing.T, report *core.ReconcileReport, name string) core.CheckResult {
	t.Helper()
	for _, c := range report.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q not found in report", name)
	return core.CheckResult{}
}
