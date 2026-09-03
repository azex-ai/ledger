package service

// Pins for the reversal_chain_integrity check (docs/INVARIANTS.md I-51;
// 2026-09-03 independent review, money-out.md M-2).
//
// The SQL half -- that the scan actually sees a forged `reversal_of` row in
// a real database -- is pinned in
// postgres.TestCorruptReversalLinks_FindsTheForgedLink. What is pinned here
// is what the suite DOES with the answer: a violation must fail the check
// and name both journals, and an unanswerable scan must never read as a
// pass.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

func reversalChainCheck(t *testing.T, report *core.ReconcileReport) core.CheckResult {
	t.Helper()
	for _, c := range report.Checks {
		if c.Name == "reversal_chain_integrity" {
			return c
		}
	}
	t.Fatalf("reversal_chain_integrity did not run at all; checks were %v", checkNames(report))
	return core.CheckResult{}
}

func checkNames(report *core.ReconcileReport) []string {
	out := make([]string, 0, len(report.Checks))
	for _, c := range report.Checks {
		out = append(out, c.Name)
	}
	return out
}

// TestFullReconciliation_ReversalChainIntegrity_ReportsAForgedLink is the
// injection pin: one violation from the querier has to fail the check, fail
// the run, and produce a Finding an operator can act on -- which means both
// uids, because "some chain somewhere is wrong" is not actionable.
func TestFullReconciliation_ReversalChainIntegrity_ReportsAForgedLink(t *testing.T) {
	q := cleanQuerier()
	q.corruptReversalLinks = []CorruptReversalLink{{
		OriginalUID:      "01a06831-3e59-7d61-bdf6-8918e2ca9270",
		ReversalUID:      "13afca12-3491-4b1b-8c6a-ff8be0e78abd",
		Violation:        "unmatched_dimension",
		AccountHolder:    8801,
		CurrencyID:       1,
		ClassificationID: 3,
		EntryType:        "credit",
		ReversedAmount:   decimal.NewFromInt(50),
	}}

	svc := buildFullSvc(t, balancedGlobalSummer(), q, FullReconciliationConfig{})
	report, err := svc.RunFullReconciliation(context.Background())
	require.NoError(t, err)

	check := reversalChainCheck(t, report)
	assert.False(t, check.Passed, "a journal linked as a reversal of something it does not reverse is a violation")
	assert.True(t, check.Complete, "the scan ran to completion; only the verdict is bad")
	assert.False(t, report.OverallPassed, "and it has to fail the run, not sit quietly in one check")

	require.Len(t, check.Findings, 1)
	desc := check.Findings[0].Description
	assert.Contains(t, desc, "13afca12-3491-4b1b-8c6a-ff8be0e78abd", "the finding must name the forged journal")
	assert.Contains(t, desc, "01a06831-3e59-7d61-bdf6-8918e2ca9270", "and the journal it claims to reverse")
	assert.Contains(t, check.Findings[0].Detail, "RUNBOOK",
		"a finding about an append-only table the operator cannot simply undo has to point at the procedure")
}

// TestFullReconciliation_ReversalChainIntegrity_OverReversedNamesNoJournal
// pins the deliberate asymmetry between the two violations: an overshoot is
// a property of the total, so the finding describes the dimension and the
// two amounts rather than blaming a journal.
func TestFullReconciliation_ReversalChainIntegrity_OverReversedNamesNoJournal(t *testing.T) {
	q := cleanQuerier()
	q.corruptReversalLinks = []CorruptReversalLink{{
		OriginalUID:      "01a06831-3e59-7d61-bdf6-8918e2ca9270",
		Violation:        "over_reversed",
		AccountHolder:    8801,
		CurrencyID:       1,
		ClassificationID: 3,
		EntryType:        "debit",
		ReversedAmount:   decimal.NewFromInt(150),
		OriginalAmount:   decimal.NewFromInt(100),
	}}

	svc := buildFullSvc(t, balancedGlobalSummer(), q, FullReconciliationConfig{})
	report, err := svc.RunFullReconciliation(context.Background())
	require.NoError(t, err)

	check := reversalChainCheck(t, report)
	assert.False(t, check.Passed)
	require.Len(t, check.Findings, 1)
	desc := check.Findings[0].Description
	assert.Contains(t, desc, "150")
	assert.Contains(t, desc, "100")
	assert.Contains(t, desc, "01a06831-3e59-7d61-bdf6-8918e2ca9270")
}

// TestFullReconciliation_ReversalChainIntegrity_UnrunnableScanIsNotAPass is
// the working-agreements §3 half. An implementation that cannot answer --
// a permission error, a consumer's own stub, a timeout -- must not leave
// this check looking like it verified anything.
func TestFullReconciliation_ReversalChainIntegrity_UnrunnableScanIsNotAPass(t *testing.T) {
	q := cleanQuerier()
	q.errCorruptReversalLinks = errors.New("permission denied for table journals")

	svc := buildFullSvc(t, balancedGlobalSummer(), q, FullReconciliationConfig{})
	report, err := svc.RunFullReconciliation(context.Background())
	require.NoError(t, err)

	check := reversalChainCheck(t, report)
	assert.False(t, check.Passed, "a scan that could not run must not pass")
	assert.False(t, check.Complete, "and must not claim coverage it did not achieve")
	assert.False(t, report.FullCoverage)
	require.NotEmpty(t, check.Findings)
	assert.Contains(t, check.Findings[0].Detail, "permission denied",
		"the operator needs the underlying reason, not just that something failed")
}

// TestFullReconciliation_ReversalChainIntegrity_PageLimitMarksIncomplete
// pins the other way this check can be honest about not having seen
// everything.
func TestFullReconciliation_ReversalChainIntegrity_PageLimitMarksIncomplete(t *testing.T) {
	q := cleanQuerier()
	for i := 0; i < 3; i++ {
		q.corruptReversalLinks = append(q.corruptReversalLinks, CorruptReversalLink{
			OriginalUID:    "01a06831-3e59-7d61-bdf6-8918e2ca9270",
			ReversalUID:    "13afca12-3491-4b1b-8c6a-ff8be0e78abd",
			Violation:      "unmatched_dimension",
			AccountHolder:  int64(8801 + i),
			ReversedAmount: decimal.NewFromInt(50),
		})
	}

	svc := buildFullSvc(t, balancedGlobalSummer(), q, FullReconciliationConfig{CorruptReversalLinkPageLimit: 2})
	report, err := svc.RunFullReconciliation(context.Background())
	require.NoError(t, err)

	check := reversalChainCheck(t, report)
	assert.False(t, check.Passed)
	assert.False(t, check.Complete, "hitting the page limit means the list was truncated; saying so is the point")

	var sawLimitNote bool
	for _, f := range check.Findings {
		if strings.Contains(f.Description, "hit page limit") {
			sawLimitNote = true
		}
	}
	assert.True(t, sawLimitNote, "the truncation has to appear in the report, not only in Complete=false")
}

// TestFullReconciliation_ReversalChainIntegrity_CleanLedgerPasses is the
// false-positive guard at this layer: with no violations the check passes
// and says what it verified, so an operator reading the report can tell it
// from a check that was skipped.
func TestFullReconciliation_ReversalChainIntegrity_CleanLedgerPasses(t *testing.T) {
	svc := buildFullSvc(t, balancedGlobalSummer(), cleanQuerier(), FullReconciliationConfig{})
	report, err := svc.RunFullReconciliation(context.Background())
	require.NoError(t, err)

	check := reversalChainCheck(t, report)
	assert.True(t, check.Passed)
	assert.True(t, check.Complete)
	require.Len(t, check.Findings, 1)
	assert.Contains(t, check.Findings[0].Description, "reversal chains")
}
