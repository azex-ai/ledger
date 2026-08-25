package postgres_test

// Structural pin for I-22 (docs/INVARIANTS.md): every table (and sequence)
// in the `public` schema must hold exactly the grants the ledger's role
// setup intends for ledger_app / ledger_ro, regardless of which migration
// introduced it.
//
// Why this exists: the GRANT statements that establish ledger_app/ledger_ro
// only enumerate objects that existed when they ran, and every later
// migration that adds a table must GRANT ledger_app/ledger_ro on it
// explicitly -- a rule that depends on a human remembering it. It was
// violated more than once during development: tables were written and
// merged whose migrations carried no ledger_app/ledger_ro grant at all.
// working-agreements §5: a rule that can be enforced structurally should
// not depend on people remembering it -- this test is that enforcement. It
// will go red the moment any future migration adds a table/sequence
// without also granting it (by design -- see the note below).

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
// ledger_app/ledger_ro have no legitimate reason to touch) and asserts the
// grant shape the ledger's role policy establishes: a table protected by an
// unconditional append-only mutation guard (any BEFORE UPDATE trigger
// executing `ledger_block_mutation()` -- journal_entries, period_closes,
// checkpoint_rebuilds as of this writing) gets SELECT/INSERT only for
// ledger_app; every other table gets SELECT/INSERT/UPDATE. ledger_ro gets
// SELECT everywhere. Partitions of journal_entries are excluded (a separate
// pin, TestLedgerAppInsertsIntoPartitionCreatedAfterGrant, covers
// partition-inheritance behavior specifically).
//
// The append-only set is derived from pg_trigger/pg_proc, not a hardcoded
// table list -- a fixed list (only journal_entries) previously drifted from
// reality the moment another table reused the same guard function but was
// left grantable UPDATE: ACL and trigger must say the same thing, and
// whichever table gets a new `ledger_block_mutation()` guard in the future
// must not require a matching edit here. Tables with a *partial*
// (whitelist-based) guard -- classifications
// (`ledger_classifications_guard`), reservations
// (`ledger_reservations_guard`), journals
// (`ledger_journals_block_arbitrary_update`, which permits the event_id
// set-once backfill) -- are deliberately NOT in this set: those tables are
// legitimately updated through controlled paths and still need the
// ledger_app UPDATE grant for that to work; only their trigger enforces
// which columns may change, not the ACL layer.
//
// ⚠️ Expected to go red the moment any future migration adds a table (e.g.
// carrying a new attestation or auth-status column) without its own
// ledger_app/ledger_ro GRANT -- that is this pin doing its job, not a bug
// in it.
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

	appendOnly := queryAppendOnlyGuardedTables(t, pool)
	require.NotEmpty(t, appendOnly, "sanity: expected at least journal_entries to carry the append-only guard")

	// The single table allowed to hold a DELETE, and the reason it is allowed.
	//
	// webhook_nonces is a replay cache holding no financial data, and its prune
	// is a DELETE. 001_baseline's grant loop issued none, which did not merely
	// let the cache grow: TryRecordNonce ran the prune first and returned its
	// error, so every inbound webhook failed on a permission error in exactly
	// the deployments that connect as ledger_app. Migration 002 grants it.
	//
	// Naming it here rather than loosening the assertion is the point. Any
	// other table that acquires a DELETE fails this test, and a second entry
	// in this map is a deliberate, reviewable act -- the same shape as the
	// journals guard's mutable-column whitelist.
	deleteAllowed := map[string]bool{"webhook_nonces": true}

	for _, table := range tables {
		table := table
		t.Run(table, func(t *testing.T) {
			wantApp := []string{"SELECT", "INSERT", "UPDATE"}
			if appendOnly[table] {
				wantApp = []string{"SELECT", "INSERT"}
			}
			if deleteAllowed[table] {
				wantApp = append(wantApp, "DELETE")
			}
			assertGrants(t, pool, "ledger_app", table, wantApp)
			assertGrants(t, pool, "ledger_ro", table, []string{"SELECT"})
		})
	}
}

// queryAppendOnlyGuardedTables returns the set of `public` tables carrying
// an unconditional append-only mutation guard: a BEFORE UPDATE trigger
// executing the `ledger_block_mutation()` function (which always raises,
// regardless of what changed -- see 018). Filtering specifically on the
// UPDATE event matters: `journals` has a `ledger_block_mutation()` trigger
// too, but only for DELETE (018) -- its UPDATE path goes through the
// separate, partial `ledger_journals_block_arbitrary_update()` guard (033),
// which permits the event_id set-once backfill, so journals legitimately
// still needs the ledger_app UPDATE grant. Matching on the function name
// without the event filter would misclassify it.
//
// Uses information_schema.triggers rather than pg_trigger/pg_proc directly
// -- event_manipulation/action_timing read as plain strings instead of
// tgtype bitmask arithmetic, and action_statement already renders the
// function call as text.
func queryAppendOnlyGuardedTables(t *testing.T, pool *pgxpool.Pool) map[string]bool {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT event_object_table
		FROM information_schema.triggers
		WHERE trigger_schema = 'public'
		  AND event_manipulation = 'UPDATE'
		  AND action_timing = 'BEFORE'
		  AND action_statement = 'EXECUTE FUNCTION ledger_block_mutation()'
	`)
	require.NoError(t, err)
	defer rows.Close()

	set := map[string]bool{}
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		set[name] = true
	}
	require.NoError(t, rows.Err())
	return set
}

// TestGrantCoverage_EverySequenceHasExpectedGrants is the sequence-level
// counterpart: any table with a SERIAL/BIGSERIAL column needs its owning
// sequence granted too (USAGE is required for INSERT's implicit nextval()
// call), or the table grant alone is not enough to actually write.
// checkpoint_rebuilds_id_seq had exactly this gap alongside its table grant
// during development, closed once this pin caught it.
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
