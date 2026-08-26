package postgres_test

// DB-backed tests for CheckpointIntegrityStore (P2 of the integrity-hardening
// wave). See docs/plans/2026-08-21-tamper-evident-ledger-design.md §4,
// docs/INVARIANTS.md I-23.

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

// setupPoisonedCheckpoint posts one journal, materializes its checkpoint via
// the real rollup path, then directly corrupts the checkpoint row (the class
// of attack this whole check exists to defend against: direct SQL against a
// leaked DB credential). Returns the fixture plus the true (uncorrupted)
// balance for assertions.
func setupPoisonedCheckpoint(t *testing.T) (fixture invariantsFixture, store *postgres.LedgerStore, pool *pgxpool.Pool, holderID int64, trueBalance decimal.Decimal) {
	t.Helper()
	pool = postgrestest.SetupDB(t)
	ctx := context.Background()

	store, deps := setupInvariantsFixture(t, pool, ctx)
	holderID = 8001

	trueBalance = decimal.NewFromInt(500)
	_, err := store.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: deps.JournalType,
		IdempotencyKey: postgrestest.UniqueKey("ci-dep"),
		Source:         "checkpoint-integrity-test",
		Entries: []core.EntryInput{
			{AccountHolder: holderID, CurrencyUID: deps.Currency, ClassificationUID: deps.MainWallet, EntryType: core.EntryTypeDebit, Amount: trueBalance},
			{AccountHolder: core.SystemAccountHolder(holderID), CurrencyUID: deps.Currency, ClassificationUID: deps.Custodial, EntryType: core.EntryTypeCredit, Amount: trueBalance},
		},
	})
	require.NoError(t, err)

	// Materialize the checkpoint via the real rollup path so we start from a
	// genuinely correct checkpoint, not a hand-crafted one. PostJournal
	// already auto-enqueues both entries' dimensions (main_wallet and its
	// custodial counterpart) -- no manual EnqueueRollup needed here.
	rollup := postgres.NewRollupAdapter(pool)
	currencyID := postgrestest.InternalID(t, pool, "currencies", deps.Currency)
	mainWalletID := postgrestest.InternalID(t, pool, "classifications", deps.MainWallet)
	engine := core.NewEngine()
	rollupSvc := service.NewRollupService(rollup, rollup, rollup, rollup, engine)
	processed, err := rollupSvc.ProcessBatch(ctx, 10)
	require.NoError(t, err)
	require.Equal(t, 2, processed, "both entries' dimensions (main_wallet + custodial) materialize")

	// Sanity: checkpoint+delta reads the true balance before corruption.
	got, err := store.GetBalance(ctx, holderID, deps.Currency, deps.MainWallet)
	require.NoError(t, err)
	require.True(t, got.Equal(trueBalance))

	// Corrupt the checkpoint directly, independent of journal_entries.
	_, err = pool.Exec(ctx,
		"UPDATE balance_checkpoints SET balance = balance + 999 WHERE account_holder=$1 AND currency_id=$2 AND classification_id=$3",
		holderID, currencyID, mainWalletID,
	)
	require.NoError(t, err)

	// Sanity: GetBalance (checkpoint+delta) now reads the poisoned value —
	// this is exactly the vulnerability this store exists to route around.
	poisoned, err := store.GetBalance(ctx, holderID, deps.Currency, deps.MainWallet)
	require.NoError(t, err)
	require.True(t, poisoned.Equal(trueBalance.Add(decimal.NewFromInt(999))),
		"sanity check: GetBalance must read the poisoned checkpoint before we test the fix")

	return deps, store, pool, holderID, trueBalance
}

// TestCheckpointIntegrity_RecomputeBalance_IgnoresCheckpointTampering pins
// I-23's read-only half: RecomputeBalance must return the true entries-based
// balance regardless of what balance_checkpoints says, because it never
// references that table. Contrasted directly against GetBalance (which DOES
// trust the checkpoint) on the exact same poisoned dimension.
func TestCheckpointIntegrity_RecomputeBalance_IgnoresCheckpointTampering(t *testing.T) {
	deps, store, pool, holderID, trueBalance := setupPoisonedCheckpoint(t)
	ctx := context.Background()

	poisoned, err := store.GetBalance(ctx, holderID, deps.Currency, deps.MainWallet)
	require.NoError(t, err)
	assert.False(t, poisoned.Equal(trueBalance), "GetBalance must still read the poisoned checkpoint")

	ci := postgres.NewCheckpointIntegrityStore(pool)
	recomputed, err := ci.RecomputeBalance(ctx, holderID, deps.Currency, deps.MainWallet)
	require.NoError(t, err)
	assert.True(t, recomputed.Equal(trueBalance),
		"RecomputeBalance must ignore the poisoned checkpoint and return the true entries-based balance; got %s want %s", recomputed, trueBalance)
}

// TestCheckpointIntegrity_RebuildCheckpoint_OvercomesMonotonicGuard pins the
// core reason RebuildCheckpoint exists and cannot be replaced by calling the
// rollup worker's normal UpsertCheckpoint: the monotonic guard
// (last_entry_id can only advance) refuses to write a CORRECT value back
// once the checkpoint's last_entry_id has been tampered to look "ahead" of
// the true watermark. This test poisons both balance AND last_entry_id, then
// demonstrates the normal upsert path fails to repair it while
// RebuildCheckpoint succeeds.
func TestCheckpointIntegrity_RebuildCheckpoint_OvercomesMonotonicGuard(t *testing.T) {
	deps, store, pool, holderID, trueBalance := setupPoisonedCheckpoint(t)
	ctx := context.Background()

	currencyID := postgrestest.InternalID(t, pool, "currencies", deps.Currency)
	mainWalletID := postgrestest.InternalID(t, pool, "classifications", deps.MainWallet)

	// Additionally poison last_entry_id far ahead of the true watermark —
	// the scenario docs/plans/2026-08-21-tamper-evident-ledger-design.md §4
	// calls out explicitly: "现 upsert 只在 watermark 前进时写...无法修复污染".
	_, err := pool.Exec(ctx,
		"UPDATE balance_checkpoints SET last_entry_id = last_entry_id + 100000 WHERE account_holder=$1 AND currency_id=$2 AND classification_id=$3",
		holderID, currencyID, mainWalletID,
	)
	require.NoError(t, err)

	rollup := postgres.NewRollupAdapter(pool)

	// Demonstrate the normal path CANNOT fix this: compute the correct
	// balance/watermark from entries, then try the monotonic-guarded upsert
	// the rollup worker uses every day.
	ci := postgres.NewCheckpointIntegrityStore(pool)
	correctBalance, err := ci.RecomputeBalance(ctx, holderID, deps.Currency, deps.MainWallet)
	require.NoError(t, err)
	require.True(t, correctBalance.Equal(trueBalance))

	require.NoError(t, rollup.UpsertCheckpoint(ctx, core.BalanceCheckpoint{
		AccountHolder:    holderID,
		CurrencyID:       currencyID,
		ClassificationID: mainWalletID,
		Balance:          correctBalance,
		LastEntryID:      1, // true watermark is small; poisoned one is now +100000
		LastEntryAt:      time.Now(),
	}))

	stillPoisoned, err := store.GetBalance(ctx, holderID, deps.Currency, deps.MainWallet)
	require.NoError(t, err)
	assert.False(t, stillPoisoned.Equal(trueBalance),
		"pin: the monotonic-guarded upsert must NOT have repaired the checkpoint -- if this assertion fails, RebuildCheckpoint's unconditional overwrite is no longer necessary and this test's premise is wrong")

	// Now the actual fix: RebuildCheckpoint takes the dimension lock,
	// recomputes from entry 0, and unconditionally overwrites.
	cp, err := ci.RebuildCheckpoint(ctx, holderID, deps.Currency, deps.MainWallet, 424242)
	require.NoError(t, err)
	assert.True(t, cp.Balance.Equal(trueBalance))

	fixed, err := store.GetBalance(ctx, holderID, deps.Currency, deps.MainWallet)
	require.NoError(t, err)
	assert.True(t, fixed.Equal(trueBalance),
		"RebuildCheckpoint must repair the checkpoint so GetBalance reads the true balance again; got %s want %s", fixed, trueBalance)
}

// TestCheckpointIntegrity_RebuildCheckpoint_RefusesWhenRollupPending pins the
// precondition that guards against a subtler race: if a rollup_queue item is
// still pending/claimed for the exact dimension being rebuilt, a worker may
// already hold the (possibly poisoned) checkpoint in memory and would
// otherwise re-clobber the fix the moment its write lands.
func TestCheckpointIntegrity_RebuildCheckpoint_RefusesWhenRollupPending(t *testing.T) {
	deps, _, pool, holderID, _ := setupPoisonedCheckpoint(t)
	ctx := context.Background()

	rollup := postgres.NewRollupAdapter(pool)
	currencyID := postgrestest.InternalID(t, pool, "currencies", deps.Currency)
	mainWalletID := postgrestest.InternalID(t, pool, "classifications", deps.MainWallet)

	// Re-dirty the dimension without processing it, leaving a pending
	// rollup_queue row.
	require.NoError(t, rollup.EnqueueRollup(ctx, holderID, currencyID, mainWalletID))

	ci := postgres.NewCheckpointIntegrityStore(pool)
	_, err := ci.RebuildCheckpoint(ctx, holderID, deps.Currency, deps.MainWallet, 424242)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrRollupPending)
}

// TestCheckpointIntegrity_RebuildCheckpoint_RecordsAuditRow pins the
// team-lead review follow-up: a manual repair has the exact same
// evidence-destroying property automatic repair does (the drift vanishes
// from balance_checkpoints the moment it's overwritten), so every
// RebuildCheckpoint call must durably record the before/after values and
// drift in checkpoint_rebuilds -- a log line alone is not durable enough to
// survive log rotation or retention limits.
func TestCheckpointIntegrity_RebuildCheckpoint_RecordsAuditRow(t *testing.T) {
	deps, _, pool, holderID, trueBalance := setupPoisonedCheckpoint(t)
	ctx := context.Background()

	currencyID := postgrestest.InternalID(t, pool, "currencies", deps.Currency)
	mainWalletID := postgrestest.InternalID(t, pool, "classifications", deps.MainWallet)

	ci := postgres.NewCheckpointIntegrityStore(pool)
	const actorID = int64(9001)
	_, err := ci.RebuildCheckpoint(ctx, holderID, deps.Currency, deps.MainWallet, actorID)
	require.NoError(t, err)

	var (
		previousBalance, newBalance, drift decimal.Decimal
		gotActorID                         int64
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT previous_balance, new_balance, drift, actor_id FROM checkpoint_rebuilds
		 WHERE account_holder=$1 AND currency_id=$2 AND classification_id=$3
		 ORDER BY created_at DESC LIMIT 1`,
		holderID, currencyID, mainWalletID,
	).Scan(&previousBalance, &newBalance, &drift, &gotActorID))

	// setupPoisonedCheckpoint corrupted the checkpoint by +999.
	assert.True(t, previousBalance.Equal(trueBalance.Add(decimal.NewFromInt(999))),
		"previous_balance must record the poisoned value, got %s", previousBalance)
	assert.True(t, newBalance.Equal(trueBalance), "new_balance must record the repaired (true) value, got %s", newBalance)
	assert.True(t, drift.Equal(decimal.NewFromInt(999)), "drift must be non-zero and equal to the injected poison amount, got %s", drift)
	assert.Equal(t, actorID, gotActorID)
}

// TestCheckpointIntegrity_CheckpointRebuilds_IsAppendOnly pins the other half
// of the same follow-up: the audit trail itself must not be editable or
// deletable, or it would be exactly as trustworthy as the checkpoint it
// exists to hold accountable.
func TestCheckpointIntegrity_CheckpointRebuilds_IsAppendOnly(t *testing.T) {
	deps, _, pool, holderID, _ := setupPoisonedCheckpoint(t)
	ctx := context.Background()

	ci := postgres.NewCheckpointIntegrityStore(pool)
	_, err := ci.RebuildCheckpoint(ctx, holderID, deps.Currency, deps.MainWallet, 1)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, "UPDATE checkpoint_rebuilds SET drift = 0")
	assert.Error(t, err, "checkpoint_rebuilds must reject UPDATE")

	_, err = pool.Exec(ctx, "DELETE FROM checkpoint_rebuilds")
	assert.Error(t, err, "checkpoint_rebuilds must reject DELETE")
}

// TestCheckpointIntegrity_RebuildCheckpoint_ReturnsUIDsNotInternalIDs pins
// I-18 on the one place a checkpoint crosses the library API boundary:
// RebuildCheckpoint takes currencyUID/classificationUID strings in, and
// (docs/audits/2026-08-25-financial-engineering/structure.md's "Major" on
// core.BalanceCheckpoint, test-credibility.md:140) used to hand back internal
// BIGSERIAL ids for the very same dimension, with no field letting the
// caller map the returned ids back to the uids it passed in. This asserts
// both the value (round-trips the same uids) and the shape (no
// internal-id-looking json tag survives on the result type, so a future
// regression that adds one back fails here even before anyone notices the
// value is wrong).
func TestCheckpointIntegrity_RebuildCheckpoint_ReturnsUIDsNotInternalIDs(t *testing.T) {
	deps, _, pool, holderID, _ := setupPoisonedCheckpoint(t)
	ctx := context.Background()

	ci := postgres.NewCheckpointIntegrityStore(pool)
	cp, err := ci.RebuildCheckpoint(ctx, holderID, deps.Currency, deps.MainWallet, 777)
	require.NoError(t, err)

	assert.Equal(t, deps.Currency, cp.CurrencyUID, "must echo back the currency uid the caller passed in, not its internal id")
	assert.Equal(t, deps.MainWallet, cp.ClassificationUID, "must echo back the classification uid the caller passed in, not its internal id")
	assert.Equal(t, holderID, cp.AccountHolder)

	typ := reflect.TypeOf(*cp)
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name := strings.SplitN(f.Tag.Get("json"), ",", 2)[0]
		if name == "id" || strings.HasSuffix(name, "_id") {
			t.Errorf("%s.%s has internal-id-shaped json tag %q -- RebuildCheckpoint's result must speak uids exclusively (I-18)", typ, f.Name, name)
		}
	}
}
