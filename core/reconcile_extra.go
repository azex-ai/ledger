package core

import (
	"context"
	"time"
)

// CheckResult holds the outcome of a single reconciliation check.
// Every check runs independently; failure of one does NOT abort others.
type CheckResult struct {
	// Name is the human-readable name of the check (e.g. "orphan_entries").
	Name string `json:"name"`
	// Passed is false if any Finding was detected. Passed reports only on
	// what the check actually examined -- read it together with Complete,
	// never alone.
	Passed bool `json:"passed"`
	// Complete is false when the check could not cover its full intended
	// scope: a capped or timed-out scan, or a check that was skipped
	// outright. A check that ran to completion and found violations is
	// Passed=false/Complete=true; a check that examined only part of the
	// fleet and found nothing there is Passed=true/Complete=false. Partial
	// coverage must never read as a clean bill of health, so the zero value
	// is deliberately "not complete" -- a check that forgets to set it
	// reports as unverified rather than verified.
	Complete bool `json:"complete"`
	// Findings lists individual violations. Empty when Passed is true.
	Findings []Finding `json:"findings"`
	// CheckedAt is when the check completed.
	CheckedAt time.Time `json:"checked_at"`
}

// Finding describes a single violation detected by a reconciliation check.
type Finding struct {
	// Description is a human-readable summary of the violation.
	Description string `json:"description"`
	// Detail is an optional structured string with extra context.
	Detail string `json:"detail,omitempty"`
}

// ReconcileReport aggregates all check results from a full reconciliation run.
type ReconcileReport struct {
	// Checks holds one CheckResult per check that was executed.
	Checks []CheckResult `json:"checks"`
	// OverallPassed is true only when every check passed. It reports
	// violations found, not coverage achieved -- OverallPassed alone is NOT a
	// clean bill of health. Require OverallPassed && FullCoverage for that.
	OverallPassed bool `json:"overall_passed"`
	// FullCoverage is true only when every check covered its full intended
	// scope (every CheckResult.Complete is true). False means at least one
	// check was capped, timed out, or skipped, so the run cannot testify
	// about the parts it never looked at.
	FullCoverage bool `json:"full_coverage"`
	// RunAt is when the reconciliation run started.
	RunAt time.Time `json:"run_at"`
}

// FullReconciler runs the full reconciliation suite and returns a
// structured report. Checks are independent — a failure in one does not prevent
// the others from running.
//
// Defined on the consumer side (core/) following the hexagonal convention.
// The implementation lives in service/reconcile.go (FullReconciliationService).
type FullReconciler interface {
	RunFullReconciliation(ctx context.Context) (*ReconcileReport, error)
}
