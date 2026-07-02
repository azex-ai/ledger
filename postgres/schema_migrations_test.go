package postgres_test

// Schema-level pins for migrations 022-023. These assert the physical
// catalog objects the migrations create, independent of any store code, so a
// future migration can't silently drop the primary key or index while still
// leaving application code passing.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/postgrestest"
)

// Migration 022: journal_entries gets a primary key on (id, created_at). The
// table is PARTITION BY RANGE (created_at), so the partition key must be part
// of the primary key.
func TestMigration022_JournalEntriesHasPrimaryKey(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rows, err := pool.Query(ctx, `
		SELECT a.attname
		FROM pg_constraint c
		JOIN unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord) ON true
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
		WHERE c.conrelid = 'journal_entries'::regclass
		  AND c.contype = 'p'
		ORDER BY k.ord
	`)
	require.NoError(t, err)
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var col string
		require.NoError(t, rows.Scan(&col))
		cols = append(cols, col)
	}
	require.NoError(t, rows.Err())

	require.Len(t, cols, 2, "journal_entries primary key must be (id, created_at)")
	assert.Equal(t, []string{"id", "created_at"}, cols)
}

// Migration 023: reservations gets a non-partial index covering
// ListReservationsByAccount's filter (account_holder) and its
// ORDER BY created_at DESC, so listing by any status doesn't fall back to a
// sequential scan. currency_id is deliberately NOT in the index: the query
// does not filter on it, and placing it between account_holder and created_at
// would prevent the index from satisfying the ORDER BY.
func TestMigration023_ReservationsAccountCreatedIndexExists(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	var indexdef string
	err := pool.QueryRow(ctx, `
		SELECT indexdef FROM pg_indexes
		WHERE tablename = 'reservations' AND indexname = 'idx_reservations_account_created'
	`).Scan(&indexdef)
	require.NoError(t, err, "idx_reservations_account_created must exist on reservations")
	assert.Contains(t, indexdef, "account_holder")
	assert.NotContains(t, indexdef, "currency_id", "currency_id is not filtered by ListReservationsByAccount and would break the ORDER BY index usage")
	assert.Contains(t, indexdef, "created_at")
	assert.NotContains(t, indexdef, "WHERE", "index must not be partial — ListReservationsByAccount queries any status")
}

// Migration 024: webhook_subscribers gets delivery-status columns, all
// NOT NULL with meaningful defaults per the project's No-NULL policy.
func TestMigration024_WebhookSubscribersHasDeliveryStatusColumns(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rows, err := pool.Query(ctx, `
		SELECT column_name, is_nullable, column_default
		FROM information_schema.columns
		WHERE table_name = 'webhook_subscribers'
		  AND column_name IN ('last_status_code', 'last_error', 'last_attempt_at')
	`)
	require.NoError(t, err)
	defer rows.Close()

	got := map[string]struct {
		nullable string
		def      string
	}{}
	for rows.Next() {
		var name, nullable string
		var def *string
		require.NoError(t, rows.Scan(&name, &nullable, &def))
		d := ""
		if def != nil {
			d = *def
		}
		got[name] = struct {
			nullable string
			def      string
		}{nullable, d}
	}
	require.NoError(t, rows.Err())

	require.Len(t, got, 3, "expected last_status_code, last_error, last_attempt_at columns")
	for _, col := range []string{"last_status_code", "last_error", "last_attempt_at"} {
		assert.Equal(t, "NO", got[col].nullable, "%s must be NOT NULL", col)
	}
}

// Migration 025: journals and journal_entries both get a NOT NULL effective_at
// column, and journal_entries gets an index on (currency_id, effective_at) to
// drive as-of / trial-balance queries. Pins I-14.
func TestMigration025_EffectiveAtColumnsExist(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	for _, table := range []string{"journals", "journal_entries"} {
		var nullable string
		err := pool.QueryRow(ctx, `
			SELECT is_nullable FROM information_schema.columns
			WHERE table_name = $1 AND column_name = 'effective_at'
		`, table).Scan(&nullable)
		require.NoError(t, err, "%s.effective_at must exist", table)
		assert.Equal(t, "NO", nullable, "%s.effective_at must be NOT NULL", table)
	}

	var indexdef string
	err := pool.QueryRow(ctx, `
		SELECT indexdef FROM pg_indexes
		WHERE tablename = 'journal_entries' AND indexname = 'idx_entries_currency_effective'
	`).Scan(&indexdef)
	require.NoError(t, err, "idx_entries_currency_effective must exist on journal_entries")
	assert.Contains(t, indexdef, "currency_id")
	assert.Contains(t, indexdef, "effective_at")
}

// Migration 026: period_closes is append-only (no unique constraint on
// close_before — latest-row-wins comes from ORDER BY created_at, not from a
// uniqueness guarantee). Pins I-15.
func TestMigration026_PeriodClosesTableExists(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rows, err := pool.Query(ctx, `
		SELECT column_name, is_nullable, column_default
		FROM information_schema.columns
		WHERE table_name = 'period_closes'
	`)
	require.NoError(t, err)
	defer rows.Close()

	got := map[string]string{}
	for rows.Next() {
		var name, nullable string
		var def *string
		require.NoError(t, rows.Scan(&name, &nullable, &def))
		got[name] = nullable
	}
	require.NoError(t, rows.Err())

	for _, col := range []string{"id", "close_before", "note", "actor_id", "created_at"} {
		assert.Equal(t, "NO", got[col], "period_closes.%s must be NOT NULL", col)
	}
}
