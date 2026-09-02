package postgres_test

// Pins for migration 020 and I-58 (docs/INVARIANTS.md): the forensic trail
// covers every table whose guard lets some updates through, and the role that
// trail is about cannot write to it.
//
// Every attack below was run as ledger_app against a real database before
// migration 020 existed, and every one succeeded.

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/postgrestest"
)

// TestPartialGuardTablesAreAudited derives the population that needs an audit
// trigger instead of restating it.
//
// Migration 006 attached the trigger to four tables it named by hand:
// currencies, classifications, journal_types, entry_templates. The predicate
// that actually describes "a change here can get through and leave no record"
// is 001 section 14's, run backwards -- a table carrying a BEFORE UPDATE row
// trigger that is NOT the blanket ledger_block_mutation() refusal is only
// partly guarded, so by construction some updates pass and none are recorded.
// Deriving it yields eleven tables, not four; the five the audit report named
// (account_policies, bookings, events, reservations, journals) plus
// deposit_addresses and entry_template_lines.
//
// ⚠️ Goes red when a migration adds a partial guard without an audit trigger.
// That is the same shape as grant_coverage_test.go's three-way classification
// and exists for the same reason: the rule must not depend on the next author
// remembering it.
func TestPartialGuardTablesAreAudited(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rows, err := pool.Query(ctx, `
		SELECT DISTINCT c.relname
		FROM pg_trigger t
		JOIN pg_class c ON c.oid = t.tgrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_proc p ON p.oid = t.tgfoid
		WHERE n.nspname = 'public'
		  AND NOT t.tgisinternal
		  AND (t.tgtype & 2) <> 0   -- BEFORE
		  AND (t.tgtype & 16) <> 0  -- UPDATE
		  AND (t.tgtype & 1) <> 0   -- FOR EACH ROW
		  AND p.proname <> 'ledger_block_mutation'
		ORDER BY 1
	`)
	require.NoError(t, err)
	var partiallyGuarded []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		partiallyGuarded = append(partiallyGuarded, name)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	require.NotEmpty(t, partiallyGuarded, "sanity: the schema has whitelist-guarded tables")

	audited := map[string]bool{}
	rows, err = pool.Query(ctx, `
		SELECT DISTINCT c.relname
		FROM pg_trigger t
		JOIN pg_class c ON c.oid = t.tgrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_proc p ON p.oid = t.tgfoid
		WHERE n.nspname = 'public'
		  AND NOT t.tgisinternal
		  AND p.proname = 'ledger_log_config_table_change'
	`)
	require.NoError(t, err)
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		audited[name] = true
	}
	require.NoError(t, rows.Err())
	rows.Close()

	for _, table := range partiallyGuarded {
		assert.True(t, audited[table],
			"%s has a whitelist guard, so some updates pass it; without an audit trigger none of them leave a record", table)
	}
}

// TestAccountPolicyEnforcementKnobChangeIsAudited is the specific attack the
// derived rule above exists for, run end to end as ledger_app.
//
// account_policies is the only DB-enforced freeze/overdraft floor
// (postgres/account_policy_enforce.go). Migration 006 gave it a guard whose
// whitelist has to contain status, min_balance and enforce_min_balance --
// UpsertAccountPolicy writes them -- and then excluded it from the audit
// triggers on the grounds that it already had an application-level trail in
// account_policy_changes. That trail is written by the application, in the
// application's transaction. An attacker holding the DB credential does not
// use the application.
//
// So the statement below is expected to SUCCEED, both before and after the
// fix. What migration 020 changes is whether it leaves a trace: before, both
// account_policy_changes and config_table_changes stayed at zero rows.
func TestAccountPolicyEnforcementKnobChangeIsAudited(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	appPool := newAppPool(t, pool, "audit-trail-app-policy-not-a-real-secret") //nolint:gosec

	var policyUID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO account_policies (account_holder, currency_id, classification_id, status, min_balance, enforce_min_balance, note, uid)
		VALUES (910101, 0, 0, 'frozen', 0, true, 'seed', gen_random_uuid())
		RETURNING uid
	`).Scan(&policyUID))

	_, err := appPool.Exec(ctx, `
		UPDATE account_policies SET status = 'active', min_balance = -1000000, enforce_min_balance = false
		WHERE uid = $1::uuid
	`, policyUID)
	require.NoError(t, err, "the guard's whitelist has to permit this -- UpsertAccountPolicy writes the same three columns")

	var changedBy, oldStatus, newStatus, newMin string
	err = pool.QueryRow(ctx, `
		SELECT changed_by, old_row->>'status', new_row->>'status', new_row->>'min_balance'
		FROM config_table_changes
		WHERE table_name = 'account_policies' AND new_row->>'uid' = $1
		ORDER BY id DESC LIMIT 1
	`, policyUID).Scan(&changedBy, &oldStatus, &newStatus, &newMin)
	require.NoError(t, err, "unfreezing an account and moving its overdraft floor must leave a forensic row")

	assert.Equal(t, "ledger_app", changedBy, "the audit row must name the role that authenticated, not the trigger function's owner")
	assert.Equal(t, "frozen", oldStatus)
	assert.Equal(t, "active", newStatus)
	assert.Equal(t, "-1000000.000000000000000000", newMin)
}

// TestLedgerAppCannotWriteTheAuditTrail pins the other half: a trail the
// suspect can append to answers "who" with a value the suspect chose.
//
// Migration 006 granted ledger_app INSERT on both audit tables because its
// trigger functions ran with invoker rights. UPDATE and DELETE were already
// refused, so real rows could not be erased -- but forging rows and flooding
// the table are both appends. Measured before 020, as ledger_app: the insert
// below succeeded and read back with changed_by='ledger_owner' and a
// changed_at 30 days in the past.
func TestLedgerAppCannotWriteTheAuditTrail(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	appPool := newAppPool(t, pool, "audit-trail-app-forge-not-a-real-secret") //nolint:gosec

	t.Run("cannot forge a config change attributed to another role at another time", func(t *testing.T) {
		_, err := appPool.Exec(ctx, `
			INSERT INTO config_table_changes (table_name, old_row, new_row, changed_by, changed_at)
			VALUES ('currencies', '{}', '{}', 'ledger_owner', now() - interval '30 days')
		`)
		assertPermissionDenied(t, err)
	})

	t.Run("cannot forge a reconcile cursor change", func(t *testing.T) {
		_, err := appPool.Exec(ctx, `
			INSERT INTO reconcile_scan_cursor_changes
				(check_name, old_after_holder, old_after_currency, old_lap_dirty,
				 new_after_holder, new_after_currency, new_lap_dirty, changed_by)
			VALUES ('unauthorized_journals', 0, 0, false, 0, 0, false, 'ledger_owner')
		`)
		assertPermissionDenied(t, err)
	})

	t.Run("cannot flood the trail through the sequence either", func(t *testing.T) {
		// The INSERT revoke alone would still leave ledger_app USAGE on the
		// BIGSERIAL sequence, which is not an attack by itself but is the
		// other half of the grant 006 issued: revoking one and leaving the
		// other is the shape that reads as "narrowed" and is not.
		var n int64
		err := appPool.QueryRow(ctx, "SELECT nextval('config_table_changes_id_seq')").Scan(&n)
		assertPermissionDenied(t, err)
	})

	t.Run("the trigger path still writes", func(t *testing.T) {
		// The whole revoke is worthless if it also broke the legitimate
		// writer: SECURITY DEFINER is what keeps this working, and a
		// regression here would look exactly like "the audit trail is quiet".
		_, err := pool.Exec(ctx, "INSERT INTO currencies (uid, code, name) VALUES (gen_random_uuid(), 'AUD1', 'audit probe')")
		require.NoError(t, err)
		_, err = appPool.Exec(ctx, "UPDATE currencies SET is_active = false WHERE code = 'AUD1'")
		require.NoError(t, err)

		var changedBy string
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT changed_by FROM config_table_changes
			WHERE table_name = 'currencies' AND new_row->>'code' = 'AUD1'
			ORDER BY id DESC LIMIT 1
		`).Scan(&changedBy))
		assert.Equal(t, "ledger_app", changedBy)
	})
}

// TestAuditTrailRowsStayImmutable re-pins what migration 006 established, so
// that making the writer SECURITY DEFINER cannot quietly reopen it: a definer
// function's privileges are the owner's, and the owner can do anything to
// these tables. The guard triggers, not the ACL, are what stop that -- and
// they have to still be in place.
func TestAuditTrailRowsStayImmutable(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	// Row-level triggers only fire on rows, so each table needs one first --
	// a guard asserted against an empty table passes for the wrong reason.
	seed := map[string]string{
		"config_table_changes": `INSERT INTO config_table_changes (table_name, old_row, new_row)
			VALUES ('currencies', '{"a":1}', '{"a":2}')`,
		"reconcile_scan_cursor_changes": `INSERT INTO reconcile_scan_cursor_changes
			(check_name, old_after_holder, old_after_currency, old_lap_dirty, new_after_holder, new_after_currency, new_lap_dirty)
			VALUES ('unauthorized_journals', 0, 0, false, 1, 0, false)`,
		"account_policy_changes": `INSERT INTO account_policy_changes (policy_id, old_state, new_state, actor_id)
			SELECT id, '{"status":"frozen"}', '{"status":"active"}', 7 FROM account_policies LIMIT 1`,
	}

	// account_policy_changes' seed needs a policy to reference (policy_id is a
	// real FK), so create one first.
	_, err := pool.Exec(ctx, `
		INSERT INTO account_policies (account_holder, currency_id, classification_id, status, note, uid)
		VALUES (930101, 0, 0, 'active', 'immutability probe', gen_random_uuid())
	`)
	require.NoError(t, err)

	for _, table := range []string{"config_table_changes", "reconcile_scan_cursor_changes", "account_policy_changes"} {
		table := table
		t.Run(table, func(t *testing.T) {
			_, err := pool.Exec(ctx, seed[table])
			require.NoError(t, err, "sanity: the owner credential seeds a row so the row-level guard has something to fire on")

			assertBlockedByGuard(t, pool, fmt.Sprintf("UPDATE %s SET id = id", table))
			assertBlockedByGuard(t, pool, fmt.Sprintf("DELETE FROM %s", table))

			var n int64
			require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n))
			assert.Equal(t, int64(1), n, "the seeded row must still be there")
		})
	}
}

// assertBlockedByGuard runs stmt as the pool's own (owner-privileged)
// credential and requires the row-level guard trigger to refuse it. Running as
// the owner rather than as ledger_app is deliberate: an ACL refusal would pass
// a weaker version of this test without proving the trigger is there at all.
func assertBlockedByGuard(t *testing.T, pool *pgxpool.Pool, stmt string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), stmt)
	require.Error(t, err, "%s must be refused by the append-only guard", stmt)
}
