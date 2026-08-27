package postgres_test

// I-47: Migrate() serializes against every other Migrate() call on the same
// Postgres cluster, not just against other callers targeting the same
// database. See docs/INVARIANTS.md I-47 and postgres.acquireClusterLock's
// doc comment for the full account of why a per-database lock (which is all
// golang-migrate's own advisory lock, or a naively-placed
// pg_advisory_lock, can ever provide) is not enough.

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// TestMigrate_ConcurrentAcrossDatabases installs the ledger schema into N
// distinct, freshly created databases on one Postgres cluster concurrently
// -- exactly what N test binaries under `go test ./...` do when DATABASE_URL
// points every package at a single shared server (CI's shape) or when
// several projects share one local dev-postgres instance (infra.md's shape).
//
// Before I-47's fix, this reliably fails: 001_baseline's CREATE ROLE and
// 007_role_hardening_and_partition_security_definer's ALTER ROLE statements
// write cluster-wide shared-catalog rows (pg_authid) that every one of the N
// Migrate() calls touches, and Postgres rejects the losing statement with
// "tuple concurrently updated" instead of blocking it. Serial `go test -p 1`
// would hide this -- it never runs two Migrate() calls at the same instant --
// which is exactly why it is not an acceptable fix (working-agreements.md
// §1: that "fix" protects CI's clock, not the cluster the invariant is
// actually about).
func TestMigrate_ConcurrentAcrossDatabases(t *testing.T) {
	const n = 8

	// Warm the cluster's roles up first, sequentially, against a throwaway
	// database. Without this, every one of the N racers below hits
	// 001_baseline's `CREATE ROLE IF NOT EXISTS` at the same instant on a
	// cluster with no ledger roles yet, which surfaces as a
	// pg_authid_rolname_index unique-constraint violation instead of the
	// "tuple concurrently updated" CI actually reported. Both are the same
	// underlying defect (concurrent writes to a cluster-wide shared
	// catalog) and both are closed by the same lock, but pre-creating the
	// roles here makes every racer skip CREATE ROLE and land on
	// 007_role_hardening_and_partition_security_definer's unconditional
	// `ALTER ROLE` instead, reproducing the exact failure mode CI hit.
	require.NoError(t, postgres.Migrate(postgrestest.SetupRawDB(t)))

	urls := make([]string, n)
	for i := range urls {
		// Sequential on purpose: each call does its own CREATE DATABASE
		// against the shared cluster via postgrestest's own admin
		// connection. The race under test is in the N Migrate() calls
		// below, not in provisioning the N databases.
		urls[i] = postgrestest.SetupRawDB(t)
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i, url := range urls {
		wg.Add(1)
		go func(i int, url string) {
			defer wg.Done()
			errs[i] = postgres.Migrate(url)
		}(i, url)
	}
	wg.Wait()

	for i, err := range errs {
		require.NoErrorf(t, err, "Migrate() call #%d", i)
	}
}
