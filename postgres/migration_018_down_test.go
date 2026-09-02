package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/internal/postgrestest"
)

// TestMigration018_DownAndReUpRoundTrip executes migration 018's down
// script and then its up script again.
//
// `deployment.md` requires every new migration to ship a down script, and
// this repository's test suite only ever runs migrations UP -- so a down
// script is, by default, code that has never been executed even once. That
// is the same "未运行 ≠ 通过" shape the rest of this round is about
// (working-agreements.md §3), and it matters more than usual here because
// 018 takes a temporary `ledger_owner` membership and transfers ownership:
// a down script that cannot drop what it created leaves an operator who
// needs to roll back with a half-migrated database and no way forward.
//
// The re-up half is the part a rollback actually needs: rolling back is
// only useful if the next deploy can re-apply.
func TestMigration018_DownAndReUpRoundTrip(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	const base = "sql/migrations/018_reject_nan_amounts_and_record_anchor_observations"
	down, err := os.ReadFile(base + ".down.sql")
	require.NoError(t, err)
	up, err := os.ReadFile(base + ".up.sql")
	require.NoError(t, err)

	// Precondition: 018 is applied (SetupDB migrates up).
	var n int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM anchor_observations").Scan(&n))

	_, err = pool.Exec(ctx, string(down))
	require.NoError(t, err, "the down script must apply cleanly")
	require.Error(t, pool.QueryRow(ctx, "SELECT count(*) FROM anchor_observations").Scan(&n),
		"down must drop anchor_observations")

	// With the CHECK constraints gone, NaN is accepted again -- which is
	// what proves the down script actually dropped them rather than
	// silently no-op'ing on names that never matched.
	var journalTypeID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO journal_types (uid, code, name) VALUES (gen_random_uuid(), 'down018_jt', 'Down 018 JT') RETURNING id
	`).Scan(&journalTypeID))
	_, err = pool.Exec(ctx, `
		INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit, metadata, actor_id, source, effective_at, uid)
		VALUES ($1, $2, 'NaN'::numeric, 'NaN'::numeric, '{}'::jsonb, 0, 'down018', now(), gen_random_uuid())
	`, journalTypeID, postgrestest.UniqueKey("down018"))
	require.NoError(t, err, "after the down script the pre-018 behaviour is back (NaN accepted) -- this is the control")

	// The NaN row has to go before the constraints can come back. journals
	// is append-only by trigger, so this is the owner-role dance an operator
	// would have to perform too -- and the reason the up script refuses to
	// install while a violating row exists (rather than silently skipping).
	_, err = pool.Exec(ctx, string(up))
	require.Error(t, err, "re-up must REFUSE while a NaN row exists, naming the offending table")
	require.Contains(t, err.Error(), "existing NaN amounts must be corrected")

	_, err = pool.Exec(ctx, "ALTER TABLE journals DISABLE TRIGGER journals_no_delete")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "DELETE FROM journals WHERE source = 'down018'")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "ALTER TABLE journals ENABLE TRIGGER journals_no_delete")
	require.NoError(t, err)

	_, err = pool.Exec(ctx, string(up))
	require.NoError(t, err, "the up script must re-apply after a rollback")
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM anchor_observations").Scan(&n),
		"re-up must recreate anchor_observations")
}
