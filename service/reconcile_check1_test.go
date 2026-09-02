package service

// F-P12 (2026-09-02 audit): docs/INVARIANTS.md I-12's three cited pins
// (TestMoneyConservation_Network / TestCheck4AccountingEquation_Balanced /
// TestReconciliationService_BalancedSystem) are all happy-path "the system
// is balanced" assertions. Every other check in RunFullReconciliation has a
// sibling test that injects real drift and asserts Passed=false --
// check1 ("global_dr_cr_equality") did not. This file closes that gap:
// mutating runCheck1JournalBalance's `r.Balanced -> result.Passed` mapping
// to always report true must make this test fail.

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// TestFullReconciliation_DetectsGlobalImbalance pins I-12's negative case at
// the RunFullReconciliation layer: a GlobalSummer reporting debit != credit
// for a currency must surface as check "global_dr_cr_equality" with
// Passed=false, a non-empty Finding, and OverallPassed=false.
func TestFullReconciliation_DetectsGlobalImbalance(t *testing.T) {
	unbalanced := &mockGlobalSummer{
		totals: []CurrencyReconcileTotals{
			{CurrencyID: 1, Debit: decimal.NewFromInt(1100), Credit: decimal.NewFromInt(1000)},
		},
	}
	engine := core.NewEngine()
	basic := NewReconciliationService(unbalanced, nil, nil, &mockClassificationLister{}, engine)
	svc := NewFullReconciliationService(basic, cleanQuerier(), FullReconciliationConfig{}, engine)

	report, err := svc.RunFullReconciliation(context.Background())
	require.NoError(t, err)

	check := findCheckInReport(t, report, "global_dr_cr_equality")
	assert.False(t, check.Passed, "a currency-level debit/credit gap must fail check1")
	require.NotEmpty(t, check.Findings, "the gap must surface as a Finding, not a silent Passed=false")
	assert.False(t, report.OverallPassed, "a global imbalance must sink OverallPassed")
}
