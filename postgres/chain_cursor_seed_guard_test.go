package postgres_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// TestChainCursorFirstWriteIsBoundedToo pins migration 032's chain_cursors
// INSERT guard (2026-09-03 independent review, re-check round, onchain-ops;
// docs/INVARIANTS.md I-67 rule 2).
//
// Migration 029 bounded how far one write may MOVE a cursor and concluded
// that a single statement therefore "skips at most one oversized window".
// That was true of the UPDATE branch, which was the only branch it guarded.
// A chain with no cursor row -- newly configured, or one whose row an owner
// deleted -- takes the INSERT branch, and there `ledger_app` could name any
// starting block it liked (88,888,888 measured), leaving nothing behind but
// an audit row. Every deposit below that block is then invisible to every
// code path, permanently: the forward scan never looks back (I-52).
//
// I-67's rule said the cursor "only moves forward", and that word is what
// let this through -- the first write does not move anything, it decides
// where moving starts.
func TestChainCursorFirstWriteIsBoundedToo(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	appPool := newAppPool(t, pool, "seed-guard-app-not-a-real-secret") //nolint:gosec

	cursors := postgres.NewChainCursorStore(pool)

	t.Run("ledger_app cannot create a cursor beyond the cap", func(t *testing.T) {
		_, err := appPool.Exec(ctx,
			`INSERT INTO chain_cursors (chain_id, last_scanned_block) VALUES ($1, 88888888)`, 912001)
		require.Error(t, err, "one INSERT used to decide that every deposit below block 88,888,888 never happened")
		assert.Contains(t, err.Error(), "may not be created at block",
			"ledger_app must get the rule, not a bare 42501 from the door predicate whose EXECUTE it does not hold")
	})

	t.Run("the cap itself is allowed", func(t *testing.T) {
		_, err := appPool.Exec(ctx,
			`INSERT INTO chain_cursors (chain_id, last_scanned_block) VALUES ($1, 100000)`, 912002)
		require.NoError(t, err, "the bound is the same 100,000 the UPDATE branch uses, inclusive")
	})

	t.Run("one block past the cap is not", func(t *testing.T) {
		_, err := appPool.Exec(ctx,
			`INSERT INTO chain_cursors (chain_id, last_scanned_block) VALUES ($1, 100001)`, 912003)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "may not be created at block")
	})

	t.Run("the honest first write is unaffected", func(t *testing.T) {
		// What the watcher actually does on a chain it has never scanned:
		// start at genesis and walk up in maxBlocksPerScan-sized windows, so
		// its first cursor write is small by construction.
		const chainID = int64(912004)
		require.NoError(t, cursors.SetCursor(ctx, chainID, 1999))
		got, err := cursors.GetCursor(ctx, chainID)
		require.NoError(t, err)
		assert.Equal(t, int64(1999), got.LastScannedBlock)
	})

	t.Run("nor can the owner, outside the seeding door", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO chain_cursors (chain_id, last_scanned_block) VALUES ($1, 5000000)`, 912005)
		require.Error(t, err, "a raw high INSERT is what the seeding door exists to replace")
		assert.Contains(t, err.Error(), "by a raw INSERT, even as owner")
	})

	t.Run("ledger_app cannot call the seeding door", func(t *testing.T) {
		_, err := appPool.Exec(ctx, `SELECT ledger_seed_chain_cursor($1, 5000000, 'let me in')`, 912006)
		assertPermissionDenied(t, err)
	})

	t.Run("the owner can seed high, with a reason, and it is recorded", func(t *testing.T) {
		const chainID = int64(912007)
		_, err := pool.Exec(ctx,
			`SELECT ledger_seed_chain_cursor($1, 5000000, 'chain X launched at 4.9M; scanning from genesis is infeasible')`, chainID)
		require.NoError(t, err)

		got, err := cursors.GetCursor(ctx, chainID)
		require.NoError(t, err)
		assert.Equal(t, int64(5000000), got.LastScannedBlock,
			"a deployment whose chain really starts high must have a way to say so")

		var reason, changedBy, block string
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT new_row->>'reason', changed_by, new_row->>'last_scanned_block'
			FROM config_table_changes
			WHERE table_name = 'ledger_seed_chain_cursor' AND new_row->>'chain_id' = $1
			ORDER BY id DESC LIMIT 1
		`, fmt.Sprint(chainID)).Scan(&reason, &changedBy, &block))
		assert.Equal(t, "chain X launched at 4.9M; scanning from genesis is infeasible", reason)
		assert.Equal(t, "5000000", block)
		assert.NotEmpty(t, changedBy)
	})

	t.Run("seeding without a reason is refused", func(t *testing.T) {
		_, err := pool.Exec(ctx, `SELECT ledger_seed_chain_cursor($1, 5000000, '  ')`, 912008)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "requires a reason")
	})

	t.Run("seeding a chain that already has a cursor fails loudly", func(t *testing.T) {
		const chainID = int64(912009)
		require.NoError(t, cursors.SetCursor(ctx, chainID, 500))
		_, err := pool.Exec(ctx, `SELECT ledger_seed_chain_cursor($1, 5000000, 'move it along')`, chainID)
		require.Error(t, err, "seeding is for a chain with no cursor; anything else is an advance or a rewind")
		assert.Contains(t, err.Error(), "already has a cursor")

		// And it really did not move -- a repair that reports success having
		// done nothing is the failure mode this repo keeps finding.
		got, err := cursors.GetCursor(ctx, chainID)
		require.NoError(t, err)
		assert.Equal(t, int64(500), got.LastScannedBlock)
	})

	t.Run("the door's flag does not survive the transaction that opened it", func(t *testing.T) {
		// set_config(..., true) is transaction-local, so a second INSERT in
		// a later statement cannot ride the flag the door set.
		_, err := pool.Exec(ctx,
			`INSERT INTO chain_cursors (chain_id, last_scanned_block) VALUES ($1, 6000000)`, 912010)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "by a raw INSERT, even as owner")
	})

	t.Run("a seeded cursor is still an ordinary cursor afterwards", func(t *testing.T) {
		const chainID = int64(912011)
		_, err := pool.Exec(ctx, `SELECT ledger_seed_chain_cursor($1, 4000000, 'established chain')`, chainID)
		require.NoError(t, err)

		require.NoError(t, cursors.SetCursor(ctx, chainID, 4001999), "the watcher advances it normally")
		got, err := cursors.GetCursor(ctx, chainID)
		require.NoError(t, err)
		assert.Equal(t, int64(4001999), got.LastScannedBlock)

		_, err = appPool.Exec(ctx, `UPDATE chain_cursors SET last_scanned_block = 99999999 WHERE chain_id = $1`, chainID)
		require.Error(t, err, "and 029's jump bound still applies to it")
		assert.Contains(t, err.Error(), "blocks in one write")
	})

	// Sanity: the store's own contract is unchanged by any of this.
	_, err := cursors.GetCursor(ctx, 912999)
	assert.ErrorIs(t, err, core.ErrNotFound)
}
