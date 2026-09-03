package postgres_test

// Pin for D-M5 (2026-09-02 deep audit): the forensic trail has a reader.
//
// Migrations 006 and 010 built three tables to answer "who changed the rule
// that decides where money goes". A full-repo grep for their names, excluding
// migrations and web/, found them in exactly two kinds of place: INVARIANTS
// prose, and tests. No query file, no store method, no CLI subcommand, no
// runbook entry. By working-agreements §3's test -- "if this step had never
// run, would anything I can see be different?" -- the answer was no on every
// surface an operator reaches.
//
// So this test does not read the tables. It tampers, then reads the tamper
// back through the same path a consumer has: ledger.New(pool) ->
// svc.ConfigHistory(). Take the store method away and it stops compiling;
// take the migration's trigger away and it goes red on empty results.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
)

func TestConfigHistory_ReadsBackATamperedRule(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	svc, err := ledger.New(pool)
	require.NoError(t, err)
	history := svc.ConfigHistory()

	appPool := newAppPool(t, pool, "config-history-app-not-a-real-secret") //nolint:gosec

	// The attack from the threat model: an application credential that can
	// reach the database directly unfreezes an account and moves its overdraft
	// floor to -1,000,000. postgres/account_policy_enforce.go reads exactly
	// these three columns, so this is the whole DB-side withdrawal gate.
	var policyUID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO account_policies (account_holder, currency_id, classification_id, status, min_balance, enforce_min_balance, note, uid)
		VALUES (940101, 0, 0, 'frozen', 0, true, 'seed', gen_random_uuid())
		RETURNING uid
	`).Scan(&policyUID))
	_, err = appPool.Exec(ctx, `
		UPDATE account_policies SET status = 'active', min_balance = -1000000, enforce_min_balance = false
		WHERE uid = $1::uuid
	`, policyUID)
	require.NoError(t, err)

	t.Run("the policy change is visible through the port", func(t *testing.T) {
		changes, _, err := history.ListConfigChanges(ctx, core.ConfigChangeFilter{TableName: "account_policies"})
		require.NoError(t, err)
		require.NotEmpty(t, changes, "an operator asking 'what changed on account_policies' must get an answer")

		var found *core.ConfigChange
		for i := range changes {
			var newRow map[string]any
			require.NoError(t, json.Unmarshal(changes[i].NewRow, &newRow))
			if newRow["uid"] == policyUID {
				found = &changes[i]
				break
			}
		}
		require.NotNil(t, found, "the tampered policy's row must be in the trail")

		var oldRow, newRow map[string]any
		require.NoError(t, json.Unmarshal(found.OldRow, &oldRow))
		require.NoError(t, json.Unmarshal(found.NewRow, &newRow))
		assert.Equal(t, "frozen", oldRow["status"])
		assert.Equal(t, "active", newRow["status"])
		assert.Equal(t, "ledger_app", found.ChangedBy,
			"the trail's whole value is that this column is not forgeable by the credential it names")
		assert.WithinDuration(t, time.Now(), found.ChangedAt, time.Minute)
	})

	t.Run("no application actor recorded it, which is the tell", func(t *testing.T) {
		// The same edit made through UpsertAccountPolicy writes an
		// account_policy_changes row carrying a business actor_id. A raw-SQL
		// attacker does not, and cannot: it is the application that writes it.
		// A config_table_changes row with no matching entry here is the shape
		// of "a change nobody in the application made".
		policyChanges, _, err := history.ListAccountPolicyChanges(ctx, core.ConfigChangeFilter{AccountHolder: 940101})
		require.NoError(t, err)
		assert.Empty(t, policyChanges,
			"the DB-level trail caught this change precisely because the application-level one could not")
	})

	t.Run("filters and paging narrow the trail rather than truncating it", func(t *testing.T) {
		// Not incidental: during an incident the trail may be large (bookings
		// and reservations write to it at business rate) or deliberately
		// flooded, and a reader that silently returns only its first page is
		// how "I looked and found nothing" happens.
		seedConfigChanges(t, pool, appPool, 5)

		first, next, err := history.ListConfigChanges(ctx, core.ConfigChangeFilter{TableName: "currencies", Limit: 2})
		require.NoError(t, err)
		require.Len(t, first, 2)
		require.NotEmpty(t, next, "a full page must hand back a cursor")

		second, _, err := history.ListConfigChanges(ctx, core.ConfigChangeFilter{TableName: "currencies", Limit: 2, Cursor: next})
		require.NoError(t, err)
		require.NotEmpty(t, second)
		assert.NotEqual(t, first[0].ChangedAt, second[0].ChangedAt, "the second page must be different rows, not the first page again")

		// Newest-first: an incident asks "what changed recently".
		assert.True(t, !first[0].ChangedAt.Before(first[1].ChangedAt), "rows must come back newest first")

		none, _, err := history.ListConfigChanges(ctx, core.ConfigChangeFilter{TableName: "no_such_table"})
		require.NoError(t, err)
		assert.Empty(t, none, "a table filter that matches nothing must return nothing, not everything")

		future, _, err := history.ListConfigChanges(ctx, core.ConfigChangeFilter{Since: time.Now().Add(time.Hour)})
		require.NoError(t, err)
		assert.Empty(t, future)
	})

	t.Run("a malformed cursor surfaces instead of silently restarting", func(t *testing.T) {
		_, _, err := history.ListConfigChanges(ctx, core.ConfigChangeFilter{Cursor: "not-a-cursor"})
		require.ErrorIs(t, err, core.ErrInvalidInput)
	})
}

// TestConfigHistory_ReadsScanCursorWrites covers the second table: moving a
// reconciliation cursor forward is how a full reconciliation is made to report
// a clean bill of health over ledger it never scanned (migration 010's header).
// The DB layer's answer to that was detection rather than prevention, which
// only counts as detection if something reads the record.
func TestConfigHistory_ReadsScanCursorWrites(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	svc, err := ledger.New(pool)
	require.NoError(t, err)
	history := svc.ConfigHistory()
	appPool := newAppPool(t, pool, "config-history-cursor-not-a-real-secret") //nolint:gosec

	_, err = pool.Exec(ctx, `
		INSERT INTO reconcile_scan_cursors (check_name, after_holder, after_currency, lap_dirty)
		VALUES ('unauthorized_journals', 0, 0, false)
	`)
	require.NoError(t, err)

	_, err = appPool.Exec(ctx, `
		UPDATE reconcile_scan_cursors SET after_holder = 9223372036854775807 WHERE check_name = 'unauthorized_journals'
	`)
	require.NoError(t, err, "the cursor is mutable by design; the guarantee is that moving it leaves a record")

	changes, _, err := history.ListScanCursorChanges(ctx, core.ConfigChangeFilter{CheckName: "unauthorized_journals"})
	require.NoError(t, err)
	// Two rows since migration 029: creating the cursor is itself recorded
	// (old = the column defaults, i.e. "before every possible dimension
	// key"), and the tamper is the newest one.
	require.Len(t, changes, 2)
	assert.Equal(t, int64(0), changes[0].OldAfterHolder)
	assert.Equal(t, int64(9223372036854775807), changes[0].NewAfterHolder,
		"skipping the whole keyspace is the exact move the audit trigger exists to record")
	assert.Equal(t, "ledger_app", changes[0].ChangedBy)

	other, _, err := history.ListScanCursorChanges(ctx, core.ConfigChangeFilter{CheckName: "some_other_check"})
	require.NoError(t, err)
	assert.Empty(t, other)
}

// seedConfigChanges makes n audited changes to currencies through the
// application credential, i.e. through the trigger, not by inserting audit
// rows (which ledger_app cannot do -- see TestLedgerAppCannotWriteTheAuditTrail).
func seedConfigChanges(t *testing.T, pool, appPool *pgxpool.Pool, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		code := "CH" + string(rune('A'+i))
		_, err := pool.Exec(ctx, "INSERT INTO currencies (uid, code, name) VALUES (gen_random_uuid(), $1, 'config history seed')", code)
		require.NoError(t, err)
		_, err = appPool.Exec(ctx, "UPDATE currencies SET is_active = false WHERE code = $1", code)
		require.NoError(t, err)
	}
}
