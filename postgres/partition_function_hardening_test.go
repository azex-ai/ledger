package postgres_test

// Pins for m-1/m-2 (`.local/independent-review-2026-08-26.md`, migration
// 013_partition_function_hardening): the two SECURITY DEFINER partition
// functions installed by migration 007.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/postgrestest"
)

// TestLedgerCreateMonthlyPartition_RejectsNameRangeMismatch pins m-2: the
// function used to trust (p_from, p_to) unconditionally once p_name passed
// its shape regex, so a caller could create a partition NAMED one month but
// covering a different range entirely (or a range narrower than a full
// month) -- 013 adds a check that the range must be exactly [first-of-month,
// first-of-next-month) for the month p_name encodes.
func TestLedgerCreateMonthlyPartition_RejectsNameRangeMismatch(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	cases := []struct {
		name    string
		partTbl string
		from    string
		to      string
	}{
		{"range narrower than the named month", "journal_entries_y2099m01", "2099-01-01", "2099-01-02"},
		{"range shifted entirely off the named month", "journal_entries_y2099m02", "2099-03-01", "2099-04-01"},
		{"range far wider than a month", "journal_entries_y2099m03", "2099-01-01", "2100-01-01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, `SELECT ledger_create_monthly_partition($1, $2::date, $3::date)`, tc.partTbl, tc.from, tc.to)
			require.Error(t, err, "a name/range mismatch must be refused, not silently create a mis-bounded partition")
			var pgErr *pgconn.PgError
			require.ErrorAs(t, err, &pgErr)
			assert.Equal(t, "22023", pgErr.Code, "invalid_parameter_value") // ERRCODE = invalid_parameter_value

			var exists bool
			require.NoError(t, pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", tc.partTbl).Scan(&exists))
			assert.False(t, exists, "the rejected call must not have created the mis-bounded partition")
		})
	}
}

// TestLedgerCreateMonthlyPartition_AcceptsMatchingNameAndRange is the
// positive counterpart: a correctly-derived name/range pair (the only shape
// PartitionStore.EnsureMonthlyPartitions ever actually sends) must still
// succeed exactly as before 013.
func TestLedgerCreateMonthlyPartition_AcceptsMatchingNameAndRange(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	var created bool
	err := pool.QueryRow(ctx, `SELECT ledger_create_monthly_partition('journal_entries_y2099m06', '2099-06-01'::date, '2099-07-01'::date)`).Scan(&created)
	require.NoError(t, err)
	assert.True(t, created)

	var exists bool
	require.NoError(t, pool.QueryRow(ctx, "SELECT to_regclass('journal_entries_y2099m06') IS NOT NULL").Scan(&exists))
	assert.True(t, exists)

	// Idempotent: calling again with the same, now-existing name returns
	// false rather than erroring or duplicating.
	err = pool.QueryRow(ctx, `SELECT ledger_create_monthly_partition('journal_entries_y2099m06', '2099-06-01'::date, '2099-07-01'::date)`).Scan(&created)
	require.NoError(t, err)
	assert.False(t, created)
}

// TestPartitionFunctions_SearchPathIncludesPgTemp pins m-1: both SECURITY
// DEFINER partition functions must pin search_path with pg_temp explicitly
// listed (not left to PostgreSQL's implicit "pg_temp searched first for
// relation names" default), closing the schema-shadowing vector migration
// 007's own header comment already reasoned about but did not fully close.
func TestPartitionFunctions_SearchPathIncludesPgTemp(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	for _, fn := range []string{
		"ledger_create_monthly_partition",
		"ledger_rebalance_default_partition",
	} {
		t.Run(fn, func(t *testing.T) {
			var proconfig []string
			err := pool.QueryRow(ctx, `
				SELECT p.proconfig
				FROM pg_proc p
				JOIN pg_namespace n ON n.oid = p.pronamespace
				WHERE n.nspname = 'public' AND p.proname = $1
			`, fn).Scan(&proconfig)
			require.NoError(t, err)
			require.NotEmpty(t, proconfig, "%s must have a proconfig (SET search_path) entry", fn)

			var searchPath string
			for _, cfg := range proconfig {
				if len(cfg) > len("search_path=") && cfg[:len("search_path=")] == "search_path=" {
					searchPath = cfg[len("search_path="):]
				}
			}
			require.NotEmpty(t, searchPath, "%s: no search_path entry found in proconfig %v", fn, proconfig)
			assert.Contains(t, searchPath, "pg_temp", "%s: search_path %q must explicitly include pg_temp", fn, searchPath)
		})
	}
}
