package postgres

// Test-only exports (idiomatic Go export_test.go pattern) so the external
// postgres_test package can drive the unexported advisory-lock primitives
// directly against real concurrent transactions. postgres_test needs
// internal/postgrestest for a real *pgxpool.Pool, and internal/postgrestest
// itself imports postgres -- an internal (package postgres) test file that
// also imported postgrestest would be a genuine import cycle, so these
// tests must live in postgres_test and reach the unexported symbols through
// this shim instead. See lock_order_test.go.

import "github.com/azex-ai/ledger/postgres/sqlcgen"

// BalancePair is balancePair, exported for tests only.
type BalancePair = balancePair

// NewBalancePair constructs a BalancePair for tests only.
func NewBalancePair(holder, currencyID int64) BalancePair {
	return balancePair{holder: holder, currencyID: currencyID}
}

// AcquireBalanceLocksForTest is acquireBalanceLocks, exported for tests only.
var AcquireBalanceLocksForTest = acquireBalanceLocks

// AcquireIdempotencyLockForTest is acquireIdempotencyLock, exported for
// tests only.
var AcquireIdempotencyLockForTest = acquireIdempotencyLock

// SortedUniquePairsForTest is sortedUniquePairs, exported for tests only.
var SortedUniquePairsForTest = sortedUniquePairs

// NewQueriesForTest is sqlcgen.New, re-exported so tests do not need their
// own import of postgres/sqlcgen just to build a *sqlcgen.Queries bound to a
// pgx.Tx for the two primitives above.
var NewQueriesForTest = sqlcgen.New

// AcquireClusterLockForTest is acquireClusterLock, exported for tests only.
//
// Role names, role attributes and role membership are cluster-wide catalogs,
// not per-database ones, so postgrestest's database-level isolation does not
// reach them: a test that has to put ledger_app into an elevated state to
// prove a migration takes it back away is mutating a row every other package's
// test binary can see, and every Migrate() call can rewrite. That is precisely
// what this lock already serializes -- Migrate holds it for its whole run --
// so a test that holds it too becomes mutually exclusive with them rather than
// racing them. See roles_test.go's TestRoleAttributeHardening... and I-47.
var AcquireClusterLockForTest = acquireClusterLock
