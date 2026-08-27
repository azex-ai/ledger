package postgres_test

import (
	"context"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/postgrestest"
)

var monthlyPartitionNameRE = regexp.MustCompile(`^journal_entries_y(\d{4})m(\d{2})$`)

// createdAtWithinPartition returns a timestamp guaranteed to satisfy
// partition's own range-partition bound constraint: the 15th of the named
// month for a monthly partition (journal_entries_yYYYYmMM), or a date far
// enough in the past to fall outside every monthly partition's range for
// journal_entries_default. This matters for this test's ACL assertion to be
// meaningful: PostgreSQL checks privilege before evaluating the partition
// bound CHECK constraint, so a valid vs. invalid created_at does not change
// the 42501 result today -- but it does change what a WOULD-fail-differently
// row proves if the ACL protection is ever accidentally removed (a row
// outside its partition's bound would then be rejected by the constraint
// instead, masking the ACL regression this test exists to catch).
func createdAtWithinPartition(t *testing.T, partition string) time.Time {
	t.Helper()
	m := monthlyPartitionNameRE.FindStringSubmatch(partition)
	if m == nil {
		// journal_entries_default (or any non-monthly-named partition):
		// five years in the past is outside every bootstrap-horizon
		// monthly partition's range.
		return time.Now().AddDate(-5, 0, 0)
	}
	year, month := 0, 0
	_, err := fmt.Sscanf(m[1], "%d", &year)
	require.NoError(t, err)
	_, err = fmt.Sscanf(m[2], "%d", &month)
	require.NoError(t, err)
	return time.Date(year, time.Month(month), 15, 0, 0, 0, 0, time.UTC)
}

// TestLedgerAppCannotInsertIDDirectlyIntoAnyExistingPartition pins m-3
// (`.local/independent-review-2026-08-26.md`): migration 008's DO loop
// column-scopes ledger_app's INSERT on every partition that exists at
// install time (pg_partition_tree('journal_entries'), parent included) --
// but grant_coverage_test.go's TestGrantCoverage_EveryTableHasExpectedLedgerAppAndLedgerRoGrants
// deliberately excludes partitions (`AND NOT c.relispartition`, by design --
// see its header) and journal_entry_id_uniqueness_test.go only ever inserts
// through the PARENT table's name (relying on tuple routing, which checks
// privilege against the parent, not the partition the row lands in). No
// existing test attempts an id-bearing INSERT addressed directly BY a
// partition's own name -- the exact ACL entry 008's DO loop set on each one
// individually. This closes that gap: for every partition
// pg_partition_tree('journal_entries') reports today (the bootstrap
// horizon + journal_entries_default, all installed before 008's loop ran),
// an id-bearing INSERT issued directly against that partition's name, as
// ledger_app, must be refused at the ACL layer (42501) -- the same
// guarantee TestJournalEntries_DuplicateIDAcrossPartitions_Rejected proves
// via the parent's name, proven here via each partition's own name.
//
// Partitions created AFTER migration 008 (via
// ledger_create_monthly_partition / ledger_rebalance_default_partition) are
// deliberately NOT covered by this test: 008's own header explains ledger_app
// gets no grant on those at all (routing through the parent is the only
// path in), and TestLedgerAppInsertsIntoPartitionCreatedAfterGrant already
// pins that shape from the other direction.
func TestLedgerAppCannotInsertIDDirectlyIntoAnyExistingPartition(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	appPool := newAppPool(t, pool, "roles-test-app-partition-grant-not-a-real-secret") //nolint:gosec // test-only credential

	rows, err := pool.Query(ctx, `
		SELECT c.relname
		FROM pg_partition_tree('journal_entries'::regclass) pt
		JOIN pg_class c ON c.oid = pt.relid
		WHERE pt.relid <> 'journal_entries'::regclass
		ORDER BY c.relname
	`)
	require.NoError(t, err)
	var partitions []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		partitions = append(partitions, name)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	require.NotEmpty(t, partitions, "sanity: expected at least journal_entries_default plus the bootstrap horizon")

	for _, partition := range partitions {
		partition := partition
		t.Run(partition, func(t *testing.T) {
			createdAt := createdAtWithinPartition(t, partition)

			tx, err := appPool.Begin(ctx)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback(ctx) }()

			_, err = tx.Exec(ctx, fmt.Sprintf(`
				INSERT INTO %s (id, journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at, effective_at)
				VALUES (999999999, 1, 1, 1, 1, 'debit', 1, $1, $1)`, partition), createdAt)
			assertPermissionDenied(t, err)
		})
	}
}
