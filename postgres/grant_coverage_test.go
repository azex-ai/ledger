package postgres_test

// Structural pin for I-22 (docs/INVARIANTS.md): every table (and sequence)
// in the `public` schema must hold exactly the grants 042's policy intends
// for ledger_app / ledger_ro, regardless of which migration introduced it.
//
// Why this exists: 042's own GRANT loop only enumerates objects that
// existed when 042 ran, and its header requires every later migration that
// adds a table to GRANT ledger_app/ledger_ro on it explicitly -- a rule
// that depends on a human remembering it. It was violated twice before this
// pin existed: reconcile_scan_cursors (043) and checkpoint_rebuilds (050)
// were both written and merged before 042 landed, and neither carries a
// ledger_app/ledger_ro grant (fixed in 052). working-agreements §5: a rule
// that can be enforced structurally should not depend on people remembering
// it -- this test is that enforcement. It will go red the moment any future
// migration adds a table/sequence without also granting it (by design --
// see the P6/P5-fix note below).

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/postgrestest"
)

// TestGrantCoverage_EveryTableHasExpectedLedgerAppAndLedgerRoGrants enumerates
// every ordinary table in `public` (excluding schema_migrations, which
// ledger_app/ledger_ro have no legitimate reason to touch -- see 042's
// header) and asserts the grant shape 042's policy establishes:
// journal_entries (the append-only table) gets SELECT/INSERT only for
// ledger_app; every other table gets SELECT/INSERT/UPDATE. ledger_ro gets
// SELECT everywhere. Partitions of journal_entries are excluded (a separate
// pin, TestMigration042_LedgerAppInsertsIntoPartitionCreatedAfterGrant,
// covers partition-inheritance behavior specifically).
//
// ⚠️ Expected to go red when P6 (ledger_attestations/entry_attestations,
// migration 047) or the P5-fix auth_status column work (051) merge without
// their own GRANT -- that is this pin doing its job, not a bug in it.
func TestGrantCoverage_EveryTableHasExpectedLedgerAppAndLedgerRoGrants(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rows, err := pool.Query(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public'
		  AND c.relkind IN ('r', 'p')
		  AND NOT c.relispartition
		  AND c.relname <> 'schema_migrations'
		ORDER BY c.relname
	`)
	require.NoError(t, err)
	var tables []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		tables = append(tables, name)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	require.NotEmpty(t, tables, "sanity: expected at least the core tables to exist")

	for _, table := range tables {
		table := table
		t.Run(table, func(t *testing.T) {
			wantApp := []string{"SELECT", "INSERT", "UPDATE"}
			if table == "journal_entries" {
				wantApp = []string{"SELECT", "INSERT"}
			}
			assertGrants(t, pool, "ledger_app", table, wantApp)
			assertGrants(t, pool, "ledger_ro", table, []string{"SELECT"})
		})
	}
}

// TestGrantCoverage_EverySequenceHasExpectedGrants is the sequence-level
// counterpart: any table with a SERIAL/BIGSERIAL column needs its owning
// sequence granted too (USAGE is required for INSERT's implicit nextval()
// call), or the table grant alone is not enough to actually write.
// checkpoint_rebuilds_id_seq (050) had exactly this gap alongside its table
// grant, closed in the same migration (052).
//
// information_schema has no clean "SELECT on sequence" view (role_table_grants
// does not cover sequences at all; role_usage_grants only reports USAGE) --
// this queries pg_class.relacl directly via aclexplode to get the real ACL.
func TestGrantCoverage_EverySequenceHasExpectedGrants(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rows, err := pool.Query(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind = 'S'
		ORDER BY c.relname
	`)
	require.NoError(t, err)
	var sequences []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		sequences = append(sequences, name)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	require.NotEmpty(t, sequences, "sanity: expected at least one BIGSERIAL-backed sequence to exist")

	for _, seq := range sequences {
		seq := seq
		t.Run(seq, func(t *testing.T) {
			assertSequenceGrants(t, pool, "ledger_app", seq, []string{"USAGE", "SELECT"})
			assertSequenceGrants(t, pool, "ledger_ro", seq, []string{"SELECT"})
		})
	}
}

// assertSequenceGrants asserts the exact ACL entries `grantee` holds on
// sequence `seq`, via pg_class.relacl (see the test doc comment for why
// information_schema does not work here).
func assertSequenceGrants(t *testing.T, pool *pgxpool.Pool, grantee, seq string, want []string) {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `
		SELECT a.privilege_type
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		CROSS JOIN LATERAL aclexplode(c.relacl) AS a(grantor, grantee, privilege_type, is_grantable)
		WHERE n.nspname = 'public' AND c.relname = $1 AND a.grantee::regrole::text = $2
	`, seq, grantee)
	require.NoError(t, err)
	defer rows.Close()

	var got []string
	for rows.Next() {
		var p string
		require.NoError(t, rows.Scan(&p))
		got = append(got, p)
	}
	require.NoError(t, rows.Err())
	assert.ElementsMatch(t, want, got, "%s privileges on sequence %s", grantee, seq)
}
