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
			// Counted before the seed, not assumed to be zero: since
			// migration 029 the account_policies INSERT above leaves its own
			// forensic row in config_table_changes, so "exactly one row
			// exists" would be asserting something this test never
			// established.
			before := countRows(t, pool, table)

			_, err := pool.Exec(ctx, seed[table])
			require.NoError(t, err, "sanity: the owner credential seeds a row so the row-level guard has something to fire on")

			assertBlockedByGuard(t, pool, fmt.Sprintf("UPDATE %s SET id = id", table))
			assertBlockedByGuard(t, pool, fmt.Sprintf("DELETE FROM %s", table))

			assert.Equal(t, before+1, countRows(t, pool, table), "the seeded row must still be there, and nothing else may have gone")
		})
	}
}

func countRows(t *testing.T, pool *pgxpool.Pool, table string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n))
	return n
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

// auditTriggerFunctions are the trigger functions that WRITE the forensic
// trail. Two exist: the config-table logger from migration 006/020 and the
// reconcile-cursor logger, which records into its own shaped table.
var auditTriggerFunctions = []string{
	"ledger_log_config_table_change",
	"ledger_log_reconcile_scan_cursor_change",
}

// unauditedWrites maps a write ledger_app can perform -- keyed
// "<schema>.<table>/<EVENT>" -- that no blanket refusal guard stops and no
// audit trigger records, to why that is acceptable.
//
// Three columns, not one (2026-09-03 independent review, money-out M-1). The
// map used to be keyed by table alone and derived from UPDATE privilege, so
// it could only ever ask "can this row be changed without a trace". A row can
// also be ADDED without a trace, and for entry_template_lines, bookings,
// chain_cursors and account_policies that was the more valuable attack of the
// two -- appending a template leg multiplied every future deposit, and
// appending an account_policies tier undid a freeze, both with
// config_table_changes at zero rows. DELETE joins them for completeness:
// exactly one table grants it, and that fact should be derived rather than
// remembered.
//
// Every entry has the same shape of argument: the row holds DERIVED or
// OPERATIONAL state whose truth lives somewhere append-only, so tampering
// shows up as a reconciliation gap rather than as a missing forensic row --
// or it is a spool whose rows carry no authority at all -- or it
// authenticates itself. A table holding authoritative state does not belong
// here; it belongs behind a guard, an audit trigger, or both.
var unauditedWrites = map[string]string{
	// ---- INSERT ----
	//
	// The two self-authenticating tables. Both are the ledger's highest-rate
	// writes, and both carry the one signal this threat model says an
	// attacker cannot forge, so a second full jsonb copy of every row would
	// double the money path's write volume to re-detect what the signature
	// already detects.
	"public.journals/INSERT":        "the journal authenticates itself: auth_digest/auth_signature/auth_key_id (I-26). An appended journal is exactly what VerifyJournalAuth refuses, and I-49's gated withdrawal base is UNDEFINED for every dimension one touches -- fail-closed, at the rate money actually moves",
	"public.journal_entries/INSERT": "append-only content of the journals above, covered exactly once by ledger_attestations/entry_attestations (I-27); VerifyLedger step 3b reports any entry no signed attestation covers. ledger_app's INSERT here is column-level (id excluded, I-42), so a table-privilege derivation would miss it -- this gate looks at column privileges too, which is why the entry is here rather than absent",

	// The attestation chain: the row IS the record.
	"public.ledger_attestations/INSERT": "the hash chain is its own forensic trail, and migration 029 refuses any row but seq = head+1 whose prev_root equals the head's root_hash (I-27, I-66). An appended row can only ever be the next one, and VerifyLedger checks its signature",
	"public.entry_attestations/INSERT":  "coverage side table for the chain above; PRIMARY KEY (entry_id) makes double coverage a unique violation, and its content is recomputed from journal_entries by VerifyLedger",

	// Derived caches, recomputable from journal_entries.
	"public.balance_checkpoints/INSERT": "derived cache: balance = checkpoint + SUM(entries after it), and journal_entries is blanket-guarded append-only. A tampered checkpoint is what reconciliation's checkpoint-vs-delta check (I-2) reports, which is a stronger control than a forensic row",
	"public.balance_snapshots/INSERT":   "point-in-time copies of the same derived figure as balance_checkpoints, recomputable from journal_entries",
	"public.system_rollups/INSERT":      "derived per-(classification, currency) aggregate, recomputed from journal_entries by the rollup worker",
	"public.rollup_queue/INSERT":        "work queue of pending recomputations; rows are claimed and consumed and hold no authoritative state",

	// Operational spools and bookkeeping.
	"public.registration_rescans/INSERT": "bookkeeping for deposit-address rescans; the same rescan running twice is idempotent",
	"public.deposit_reorgs/INSERT":       "the reorg-anomaly record itself: appending one raises an operator alert, which is the safe direction. Suppressing a real one is an UPDATE, and that is audited",
	"public.ingest_dead_letters/INSERT":  "spool of inbound payloads that failed to parse; rows are operator scratch space and authorize nothing",
	"public.webhook_nonces/INSERT":       "replay-protection cache with a TTL; a row is an opaque seen-nonce marker, and appending one can only refuse a delivery, never accept one",
	"public.checkpoint_rebuilds/INSERT":  "append-only record of a checkpoint rebuild having been asked for; the rebuild itself recomputes from journal_entries (I-23), so a forged row describes work that either happened or is redone",
	"public.period_closes/INSERT":        "append-only accounting-period close. A forged row only ever ADDS a restriction (it makes effective-dated writes into that period violations the reconcile check reports), so the direction is denial-of-service, not money",

	// The application-written forensic trail, and the idempotency receipts.
	"public.account_policy_changes/INSERT":         "this IS a forensic trail, written by UpsertAccountPolicy in the caller's transaction with a business actor_id no trigger can produce (migration 020's header). The DB-role half of the same question is answered by the config_table_changes row account_policies now writes on INSERT as well as UPDATE",
	"public.reservation_settlement_legs/INSERT":    "append-only settlement claims. Since migration 028 each carries auth_digest/auth_signature/auth_key_id, and a gated Reserve credits a claim with no valid signature as discharging nothing (I-65) -- a forged leg makes the hold larger, never smaller",
	"public.reservation_operation_receipts/INSERT": "same table family and same signature (I-65, migration 028); an unsigned or forged receipt discharges no hold",
	"public.booking_transition_receipts/INSERT":    "append-only idempotency receipts keyed by the caller-derived key (I-3). Recorded as a conscious call, with its residual stated: a forged receipt makes the next legitimate Transition for that key report success without re-applying, which is a stuck booking an operator sees, not a balance change -- the accounting itself is a journal, and a journal without a valid signature is refused (I-26)",

	// ---- UPDATE ----
	"public.balance_checkpoints/UPDATE":  "derived cache: balance = checkpoint + SUM(entries after it), and journal_entries is blanket-guarded append-only. A tampered checkpoint is what reconciliation's checkpoint-vs-delta check (I-2) reports, which is a stronger control than a forensic row",
	"public.balance_snapshots/UPDATE":    "point-in-time copies of the same derived figure as balance_checkpoints, recomputable from journal_entries",
	"public.system_rollups/UPDATE":       "derived per-(classification, currency) aggregate, recomputed from journal_entries by the rollup worker",
	"public.rollup_queue/UPDATE":         "work queue of pending recomputations; rows are claimed and consumed and hold no authoritative state",
	"public.registration_rescans/UPDATE": "bookkeeping for deposit-address rescans; the same rescan running twice is idempotent",
	"public.deposit_reorgs/UPDATE":       "the reorg-anomaly record itself: rows are appended by ingestion and only their review status is updated. It is a forensic table, not a configuration one",
	"public.ingest_dead_letters/UPDATE":  "spool of inbound payloads that failed to parse; rows are operator scratch space and authorize nothing",
	"public.webhook_nonces/UPDATE":       "replay-protection cache with a TTL; a row is an opaque seen-nonce marker, and losing one costs at most one accepted replay that the ledger idempotency key then refuses",
	"public.webhook_subscribers/UPDATE":  "column-level since migration 014: only last_status_code / last_delivered_at / last_error, the delivery bookkeeping RecordDeliveryStatus writes. url and secret -- the two columns that decide where a signed event goes and who can forge one -- are not writable at all, and INSERT is revoked. That is the same carve-out 020 made for events' delivery columns, enforced by the ACL instead of by a WHEN clause",

	// ---- DELETE ----
	"public.webhook_nonces/DELETE": "the one DELETE grant in the schema (migration 002): TryRecordNonce prunes expired nonces, and without it every inbound webhook failed on a permission error. Deleting a nonce costs at most one accepted replay that the ledger idempotency key then refuses",
}

// TestWritableTablesAreAuditedOrClassified is M-6 (W3 adversarial review of
// the gates), extended to all three write events by the 2026-09-03
// independent review (money-out M-1).
//
// TestPartialGuardTablesAreAudited above derives its population from "has a
// BEFORE UPDATE row trigger that is not the blanket refusal" -- i.e. from
// having a PARTIAL guard. A table with NO guard at all lets every update
// through and records none of them, and is not in that population. The
// reviewer added a config table (fee rules, with a bps column), granted
// ledger_app SELECT/INSERT/UPDATE, classified it in
// grant_coverage_test.go's `reviewed` bucket, and attached neither a guard
// nor an audit trigger: green. I-58's promise -- "every table whose guard
// lets updates through has a forensic trail" -- can be satisfied by not
// having a guard.
//
// So the population here is derived from PRIVILEGE instead: for each of
// INSERT, UPDATE and DELETE, every table ledger_app may perform that write on
// without a blanket refusal in the way. Each must have an audit trigger
// firing on THAT event, or an entry above saying why it does not need one.
//
// Deriving the events separately is the 2026-09-03 finding: a blanket refusal
// guard is BEFORE UPDATE OR DELETE and never BEFORE INSERT, and every audit
// trigger in the schema before migration 029 was AFTER UPDATE, so a
// table-keyed, UPDATE-derived gate reported full coverage over a schema in
// which not one appended row was recorded anywhere.
func TestWritableTablesAreAuditedOrClassified(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	// tgtype bits, from PostgreSQL's TRIGGER_TYPE_* macros.
	events := []struct {
		name string
		priv string
		bit  int
	}{
		{"INSERT", "INSERT", 4},
		{"UPDATE", "UPDATE", 16},
		{"DELETE", "DELETE", 8},
	}

	classified := map[string]bool{}
	for _, ev := range events {
		ev := ev
		t.Run(ev.name, func(t *testing.T) {
			rows, err := pool.Query(ctx, `
				SELECT n.nspname || '.' || c.relname,
				       EXISTS (
				         SELECT 1 FROM pg_trigger t JOIN pg_proc p ON p.oid = t.tgfoid
				         WHERE t.tgrelid = c.oid AND NOT t.tgisinternal
				           AND p.proname = ANY($1::text[])
				           AND (t.tgtype & $2) <> 0
				       ) AS audited
				FROM pg_class c
				JOIN pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
				  AND n.nspname NOT LIKE 'pg\_%'
				  AND c.relkind IN ('r', 'p')
				  AND NOT c.relispartition
				  -- Column privileges count. journal_entries' INSERT is
				  -- column-level (migration 008, I-42) and
				  -- webhook_subscribers' UPDATE is (migration 014), so a
				  -- table-level test would drop the ledger's most important
				  -- append and its only credential-bearing table out of the
				  -- population entirely. DELETE has no column form, so it is
				  -- asked table-level -- and asking has_any_column_privilege
				  -- for it is an error, not a false, which is why the two
				  -- are separate branches rather than one clever expression.
				  AND (CASE WHEN $3::text = 'DELETE'
				            THEN has_table_privilege('ledger_app', c.oid, 'DELETE')
				            ELSE has_any_column_privilege('ledger_app', c.oid, $3::text) END)
				  AND NOT EXISTS (
				        SELECT 1 FROM pg_trigger t JOIN pg_proc p ON p.oid = t.tgfoid
				        WHERE t.tgrelid = c.oid AND NOT t.tgisinternal
				          AND p.proname = 'ledger_block_mutation'
				          AND (t.tgtype & 2) <> 0 AND (t.tgtype & $2) <> 0 AND (t.tgtype & 1) <> 0
				      )
				ORDER BY 1
			`, auditTriggerFunctions, ev.bit, ev.priv)
			require.NoError(t, err)
			defer rows.Close()

			population := 0
			for rows.Next() {
				var table string
				var audited bool
				require.NoError(t, rows.Scan(&table, &audited))
				population++
				key := table + "/" + ev.name
				if audited {
					continue
				}
				if _, ok := unauditedWrites[key]; ok {
					classified[key] = true
					continue
				}
				t.Errorf("ledger_app can %s %s, no blanket guard refuses that, and no audit trigger records it -- "+
					"so a write made with the leaked credential leaves no trace anywhere (I-58, I-66).\n\n"+
					"Note the population this gate derives: PRIVILEGE, per event. A blanket refusal guard is BEFORE UPDATE OR DELETE "+
					"and never BEFORE INSERT, so before migration 029 an appended row satisfied I-58 by there being no event to record.\n\n"+
					"Attach ledger_log_config_table_change for this event, or -- if the row holds derived or operational state whose "+
					"truth lives in an append-only table, or authenticates itself -- add %q to unauditedWrites with that argument.", ev.name, table, key)
			}
			require.NoError(t, rows.Err())
			require.Positive(t, population, "no ledger_app-writable table was found for %s -- the query regressed, and an empty population reads as a pass", ev.name)
		})
	}

	for key, reason := range unauditedWrites {
		assert.Truef(t, classified[key],
			"stale entry %q (%s): the table is gone, no longer writable by ledger_app for that event, or now audited -- delete the entry", key, reason)
	}
}
