package postgres_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// TestCheckpointDimensions_RejectOrphanFK is H-m8's pin (migration 022).
//
// balance_checkpoints, rollup_queue and balance_snapshots carried bare BIGINT
// dimension columns while system_rollups -- the table the first of them is
// aggregated INTO -- had always declared REFERENCES. The constraint was
// enforced at the destination and not at the source, so a row pointing at a
// classification that does not exist could be summed into a table whose own
// FKs guarantee it cannot contain that dimension. Only the after-the-fact
// orphan_* reconcile checks could find one.
//
// Without migration 022 every insert below succeeds and this test fails.
func TestCheckpointDimensions_RejectOrphanFK(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	// A real currency + classification, so the "valid half" of each insert
	// below is a genuine dimension and the rejection can only come from the
	// column under test.
	currencyStore := postgres.NewCurrencyStore(pool)
	classStore := postgres.NewClassificationStore(pool)
	_, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{Code: "USDT-FK", Name: "FK USDT", Exponent: 18})
	require.NoError(t, err)
	_, err = classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "wallet_fk", Name: "Wallet FK", NormalSide: core.NormalSideDebit,
		BalanceRole: core.BalanceRoleAvailable,
	})
	require.NoError(t, err)

	const absentDimension = 9_000_000_001 // no currencies/classifications row has this id

	cases := []struct {
		name string
		sql  string
		args []any
	}{
		{
			name: "balance_checkpoints.currency_id",
			sql: `INSERT INTO balance_checkpoints (account_holder, currency_id, classification_id, balance)
			      VALUES (5001, $1, (SELECT id FROM classifications LIMIT 1), 0)`,
			args: []any{int64(absentDimension)},
		},
		{
			name: "balance_checkpoints.classification_id",
			sql: `INSERT INTO balance_checkpoints (account_holder, currency_id, classification_id, balance)
			      VALUES (5002, (SELECT id FROM currencies LIMIT 1), $1, 0)`,
			args: []any{int64(absentDimension)},
		},
		{
			name: "rollup_queue.currency_id",
			sql: `INSERT INTO rollup_queue (account_holder, currency_id, classification_id)
			      VALUES (5003, $1, (SELECT id FROM classifications LIMIT 1))`,
			args: []any{int64(absentDimension)},
		},
		{
			name: "rollup_queue.classification_id",
			sql: `INSERT INTO rollup_queue (account_holder, currency_id, classification_id)
			      VALUES (5004, (SELECT id FROM currencies LIMIT 1), $1)`,
			args: []any{int64(absentDimension)},
		},
		{
			name: "balance_snapshots.currency_id",
			sql: `INSERT INTO balance_snapshots (account_holder, currency_id, classification_id, snapshot_date, balance)
			      VALUES (5005, $1, (SELECT id FROM classifications LIMIT 1), CURRENT_DATE, 0)`,
			args: []any{int64(absentDimension)},
		},
		{
			name: "balance_snapshots.classification_id",
			sql: `INSERT INTO balance_snapshots (account_holder, currency_id, classification_id, snapshot_date, balance)
			      VALUES (5006, (SELECT id FROM currencies LIMIT 1), $1, CURRENT_DATE, 0)`,
			args: []any{int64(absentDimension)},
		},
	}

	// Sanity: the dimension tables must be non-empty, or the "valid half" of
	// each insert would be NULL and the row would be rejected by NOT NULL
	// rather than by the foreign key -- a green test proving nothing.
	var currencies, classifications int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM currencies`).Scan(&currencies))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM classifications`).Scan(&classifications))
	require.NotZero(t, currencies, "no currencies rows: this test would pass for the wrong reason")
	require.NotZero(t, classifications, "no classifications rows: this test would pass for the wrong reason")

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, tc.sql, tc.args...)
			require.Error(t, err, "a row pointing at a nonexistent dimension must be rejected by a foreign key")
			require.Contains(t, err.Error(), "foreign key",
				"the rejection must come from the FK, not from some other constraint: %v", err)
		})
	}
}
