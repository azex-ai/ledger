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

	// Tables whose UPDATE was revoked outright because they have no
	// legitimate one. entry_template_lines is written once when a template
	// bundle installs and never touched again -- no upsert, no deactivation
	// -- and every column of it decides which account a journal leg hits and
	// in which direction, so migration 003 took the privilege away as well as
	// guarding the rows.
	//
	// deposit_addresses is deliberately NOT here despite also allowing no
	// mutation: registration is an upsert whose conflict branch assigns
	// account_holder its own value so RETURNING yields the existing row. The
	// row never changes, so the guard passes it, but the statement is still
	// an UPDATE and still needs the grant.
	//
	// account_policy_changes / reservation_settlement_legs /
	// reservation_operation_receipts / booking_transition_receipts /
	// reconcile_scan_cursor_changes / config_table_changes are NOT listed
	// here even though migration 006 revoked UPDATE on all six: each carries
	// a `ledger_block_mutation()` guard, so queryAppendOnlyGuardedTables
	// already classifies them append-only and this map would be redundant --
	// see migration 006's header for why they have no legitimate UPDATE.
	updateRevoked := map[string]bool{"entry_template_lines": true}

	// webhook_subscribers is column-scoped for ledger_ro as of migration
	// 007: it holds every outbound webhook's HMAC secret
	// (webhook_subscribers.secret), which a blanket SELECT ON ALL TABLES
	// handed to a read-only BI credential along with everything else. It
	// keeps table-level SELECT/INSERT/UPDATE for ledger_app (nothing about
	// the app's own access changed) but ledger_ro's grant is now
	// column-level, which information_schema.role_table_grants does not
	// report at all -- asserting plain "SELECT" against it here would read
	// as "no access", which is wrong; the real assertion is in
	// TestLedgerRoCannotReadWebhookSecret (roles_test.go) and in the
	// column-level check below.
	roColumnScoped := map[string]bool{"webhook_subscribers": true}

	// journal_entries is column-scoped for ledger_app's INSERT as of
	// migration 008: id is excluded, making the shared journal_entries_id_seq
	// the only source of a row's id (board #37 / I-43 -- a partitioned
	// table's composite (id, created_at) primary key only guarantees
	// uniqueness within one partition, not across the whole table). SELECT
	// stays table-level and unaffected (RETURNING id off the one legitimate
	// INSERT still works off that grant, not a column-level one). Same
	// information_schema blind spot as roColumnScoped above: a column-level
	// INSERT does not show up in role_table_grants, so the plain-INSERT
	// expectation append-only tables otherwise get would misread as "no
	// INSERT at all" -- the real assertion is in
	// TestJournalEntries_DuplicateIDAcrossPartitions_Rejected (an actual
	// ledger_app-credentialed attack) and the column-level check below.
	appInsertColumnScoped := map[string]bool{"journal_entries": true}

	// ####  threat-model.md Major: "no gate can discover a missing guard"  ####
	//
	// Before this, any table not in appendOnly/updateRevoked silently fell
	// into the `else` branch and got full SELECT/INSERT/UPDATE -- the exact
	// fail-open default the finding describes ("没有 trigger 一律读成『可
	// UPDATE 是意图』"). That default is what let seven tables (bookings,
	// events, account_policies, account_policy_changes and three
	// idempotency-receipt tables) keep a UPDATE grant nobody had reviewed.
	// Migration 006 closed those seven specifically; this closes the shape of
	// the gap, not just today's instances of it: reviewed is now an explicit
	// allowlist of every OTHER table this test enumerates, and any table
	// this loop finds that is in none of appendOnly/updateRevoked/reviewed
	// fails loudly instead of defaulting to "ordinary". A future migration
	// that adds an eighth ungated table now has to earn its way into this
	// list -- or this test goes red on the PR that adds it, not years later
	// in an audit.
	//
	// "reviewed" does not mean "guarded" -- most of these are guarded by a
	// column-whitelist or dimension-key trigger (currencies, journal_types,
	// entry_templates, deposit_addresses, classifications, reservations,
	// journals, account_policies, bookings, events all are; see 003, 045 and
	// 006), which this test cannot see because that guard is enforced by
	// trigger logic, not by the ACL -- their ACL shape genuinely is ordinary
	// SELECT/INSERT/UPDATE, and mutation_guards_test.go / roles_test.go pin
	// the trigger behavior directly. The rest (balance_checkpoints,
	// balance_snapshots, chain_cursors, deposits/withdrawals (history,
	// nothing reads or writes them), ingest_dead_letters,
	// reconcile_scan_cursors (mutable by design -- see
	// TestReconcileScanCursorChangesAudited), registration_rescans,
	// rollup_queue, system_rollups, webhook_nonces, webhook_subscribers)
	// have no guard at all and this is the record of that having been a
	// conscious call, not an oversight.
	reviewed := map[string]bool{
		"account_policies":       true,
		"balance_checkpoints":    true,
		"balance_snapshots":      true,
		"bookings":               true,
		"chain_cursors":          true,
		"classifications":        true,
		"currencies":             true,
		"deposit_addresses":      true,
		"deposits":               true,
		"entry_templates":        true,
		"events":                 true,
		"ingest_dead_letters":    true,
		"journal_types":          true,
		"journals":               true,
		"reconcile_scan_cursors": true,
		"registration_rescans":   true,
		"reservations":           true,
		"rollup_queue":           true,
		"system_rollups":         true,
		"webhook_nonces":         true,
		"webhook_subscribers":    true,
		"withdrawals":            true,
	}

	for _, table := range tables {
		table := table
		t.Run(table, func(t *testing.T) {
			if !appendOnly[table] && !updateRevoked[table] && !reviewed[table] {
				t.Fatalf("table %q is not classified in grant_coverage_test.go (append-only / update-revoked / reviewed-ordinary) -- "+
					"a new table defaults to nothing, not to full access; decide its mutation policy and add it to one of the three sets", table)
			}

			wantApp := []string{"SELECT", "INSERT", "UPDATE"}
			if appendOnly[table] {
				wantApp = []string{"SELECT", "INSERT"}
			}
			if updateRevoked[table] {
				wantApp = []string{"SELECT", "INSERT"}
			}
			if appInsertColumnScoped[table] {
				wantApp = []string{"SELECT"}
			}
			if deleteAllowed[table] {
				wantApp = append(wantApp, "DELETE")
			}
			assertGrants(t, pool, "ledger_app", table, wantApp)

			if appInsertColumnScoped[table] {
				assertColumnPrivilegeExists(t, pool, "ledger_app", table, "journal_id", "INSERT")
				assertColumnPrivilegeAbsent(t, pool, "ledger_app", table, "id", "INSERT")
			}

			if roColumnScoped[table] {
				assertGrants(t, pool, "ledger_ro", table, nil)
				assertColumnPrivilegeExists(t, pool, "ledger_ro", table, "name", "SELECT")
				assertColumnPrivilegeAbsent(t, pool, "ledger_ro", table, "secret", "SELECT")
				return
			}
			assertGrants(t, pool, "ledger_ro", table, []string{"SELECT"})
		})
	}
}

// assertColumnPrivilegeExists confirms grantee holds a column-level grant of
// the given privilege type on the named column -- the counterpart check for
// tables/columns where the privilege is scoped narrower than the whole
// table, which information_schema.role_table_grants (table-level-ACL-only)
// does not report at all.
func assertColumnPrivilegeExists(t *testing.T, pool *pgxpool.Pool, grantee, table, column, privilege string) {
	t.Helper()
	ctx := context.Background()
	var got string
	err := pool.QueryRow(ctx, `
		SELECT privilege_type FROM information_schema.column_privileges
		WHERE grantee = $1 AND table_schema = 'public' AND table_name = $2 AND column_name = $3 AND privilege_type = $4
	`, grantee, table, column, privilege).Scan(&got)
	require.NoError(t, err, "%s must hold %s on %s.%s", grantee, privilege, table, column)
	assert.Equal(t, privilege, got)
}

// assertColumnPrivilegeAbsent confirms grantee holds no grant of the given
// privilege type on the named column -- scoped to that one privilege type,
// not "no privilege of any kind", because information_schema.column_privileges
// also surfaces a column's row for every OTHER privilege the grantee
// independently holds at the table level (e.g. journal_entries.id still
// carries a SELECT row from ledger_app's unrelated, unmodified table-level
// SELECT grant -- checking for "any row at all" there would misfire on that
// and read as a regression that never happened).
func assertColumnPrivilegeAbsent(t *testing.T, pool *pgxpool.Pool, grantee, table, column, privilege string) {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `
		SELECT privilege_type FROM information_schema.column_privileges
		WHERE grantee = $1 AND table_schema = 'public' AND table_name = $2 AND column_name = $3 AND privilege_type = $4
	`, grantee, table, column, privilege)
	require.NoError(t, err)
	defer rows.Close()
	assert.False(t, rows.Next(), "%s must hold no %s privilege on %s.%s", grantee, privilege, table, column)
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
