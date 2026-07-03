package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// TestMigrate_PopulatedDatabase pins the upgrade path for library consumers
// whose databases already contain data. A fresh (empty) database — which is
// all plain CI ever exercises — cannot catch two classes of migration bug:
//
//  1. Backfill UPDATEs that fire the 018 append-only row triggers
//     (0 rows updated = trigger never fires; real rows = migration aborts).
//     Bit armatrix on migration 025's effective_at backfill.
//  2. ADD COLUMN ... NOT NULL without a DEFAULT on a non-empty table
//     (migration 031's uid columns).
//
// The test migrates to version 24 (pre-effective-date), seeds realistic rows
// into every entity table 031 touches, then runs the remaining migrations and
// asserts uids were backfilled.
func TestMigrate_PopulatedDatabase(t *testing.T) {
	ctx := context.Background()

	connStr := postgrestest.SetupRawDB(t)
	migrateURL := strings.Replace(connStr, "postgres://", "pgx5://", 1)

	// Step 1: migrate to 24 — the last version before the backfilling
	// migrations under test (025 effective_at, 031 uid).
	source, err := postgres.NewMigrationSource()
	require.NoError(t, err)
	m, err := migrate.NewWithSourceInstance("iofs", source, migrateURL)
	require.NoError(t, err)
	require.NoError(t, m.Migrate(24))

	// Step 2: seed rows the way a live consumer database would have them.
	pool, err := pgxpool.New(ctx, connStr)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(ctx, `
		INSERT INTO currencies (code, name) VALUES ('USD', 'US Dollar');
		INSERT INTO classifications (code, name, normal_side, is_system)
			VALUES ('upg_wallet', 'Wallet', 'debit', false),
			       ('upg_custodial', 'Custodial', 'credit', true);
		INSERT INTO journal_types (code, name) VALUES ('upg_deposit', 'Deposit');
		INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit, actor_id, source, event_id)
			SELECT jt.id, 'upg-j1', 100, 100, 0, 'upgrade-test', 0 FROM journal_types jt WHERE jt.code = 'upg_deposit';
		INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
			SELECT j.id, 42, c.id, cl.id, 'debit', 100
			FROM journals j, currencies c, classifications cl
			WHERE j.idempotency_key = 'upg-j1' AND c.code = 'USD' AND cl.code = 'upg_wallet';
		INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
			SELECT j.id, -42, c.id, cl.id, 'credit', 100
			FROM journals j, currencies c, classifications cl
			WHERE j.idempotency_key = 'upg-j1' AND c.code = 'USD' AND cl.code = 'upg_custodial';
		INSERT INTO reservations (account_holder, currency_id, reserved_amount, status, idempotency_key, expires_at)
			SELECT 42, c.id, 10, 'active', 'upg-r1', now() + interval '1 hour' FROM currencies c WHERE c.code = 'USD';
	`)
	require.NoError(t, err)

	// Step 3: run the remaining migrations over the populated database.
	require.NoError(t, m.Up())
	srcErr, dbErr := m.Close()
	require.NoError(t, srcErr)
	require.NoError(t, dbErr)

	// Step 4: every pre-existing row must have a backfilled uid, and the
	// effective_at backfill must have landed despite the append-only trigger.
	for _, table := range []string{"journals", "reservations", "classifications", "journal_types", "currencies"} {
		var nullCount int
		require.NoError(t, pool.QueryRow(ctx,
			"SELECT count(*) FROM "+table+" WHERE uid IS NULL").Scan(&nullCount))
		require.Zero(t, nullCount, "%s must have uid backfilled on every pre-existing row", table)
	}
	var mismatch int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM journal_entries je JOIN journals j ON j.id = je.journal_id
		WHERE je.effective_at IS DISTINCT FROM j.effective_at
	`).Scan(&mismatch))
	require.Zero(t, mismatch, "journal_entries.effective_at must equal the parent journal's after backfill")
}
