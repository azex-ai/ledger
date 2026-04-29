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
	// Passed is false if any Finding was detected.
	Passed bool `json:"passed"`
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
	// OverallPassed is true only when every check passed.
	OverallPassed bool `json:"overall_passed"`
	// RunAt is when the reconciliation run started.
	RunAt time.Time `json:"run_at"`
}

// FullReconciler runs the complete 10-check reconciliation suite and returns a
// structured report. Checks are independent — a failure in one does not prevent
// the others from running.
//
// Defined on the consumer side (core/) following the hexagonal convention.
// The implementation lives in service/reconcile.go (FullReconciliationService).
type FullReconciler interface {
	RunFullReconciliation(ctx context.Context) (*ReconcileReport, error)
}
