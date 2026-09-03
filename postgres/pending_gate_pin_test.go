package postgres_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	ledger "github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/presets"
)

// pendingGateFixture assembles the pending two-phase API the way a consumer
// does -- through ledger.New's composition root and the PendingBalanceWriter
// accessor -- so these pins exercise the wiring, not a hand-assembled store
// (contracts §3 rule 6: a pin that builds its own stores can stay green while
// the facade hands out something else).
type pendingGateFixture struct {
	svc         *ledger.Service
	writer      core.PendingBalanceWriter
	currencyUID string
	currencyID  int64
	pendingUID  string
	pendingID   int64
}

func setupPendingGateFixture(t *testing.T, pool *pgxpool.Pool, ctx context.Context, currencyCode string) pendingGateFixture {
	t.Helper()

	attestor, verifier := newTestAttestor(t, "ed25519-pending-gate")
	svc, err := ledger.New(pool, ledger.WithAttestor(attestor, verifier))
	require.NoError(t, err)
	require.NoError(t, presets.InstallPendingBundle(ctx, svc.Classifications(), svc.JournalTypes(), svc.Templates()))

	currencyUID := postgrestest.SeedCurrency(t, pool, currencyCode, "Pending gate "+currencyCode)

	pendingCls, err := svc.Classifications().GetByCode(ctx, "pending")
	require.NoError(t, err)

	f := pendingGateFixture{
		svc:         svc,
		writer:      svc.PendingBalanceWriter(),
		currencyUID: currencyUID,
		pendingUID:  pendingCls.UID,
	}
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM currencies WHERE uid = $1`, currencyUID).Scan(&f.currencyID))
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM classifications WHERE uid = $1`, pendingCls.UID).Scan(&f.pendingID))
	return f
}

// inflatePendingCheckpoint writes the one statement the standing threat model
// grants an attacker holding the application's DB credential: a raised
// balance_checkpoints row. last_entry_id = 0 keeps every real entry inside the
// delta window, so checkpoint + delta reports the forged figure ON TOP of the
// true one -- the shape I-49's checkpoint pin uses, and the shape
// w3-review/money-path.md M-1 measured.
func inflatePendingCheckpoint(t *testing.T, pool *pgxpool.Pool, ctx context.Context, f pendingGateFixture, holder int64, amount int64) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO balance_checkpoints (account_holder, currency_id, classification_id, balance, last_entry_id, last_entry_at)
		VALUES ($1, $2, $3, $4, 0, now())
		ON CONFLICT (account_holder, currency_id, classification_id)
		DO UPDATE SET balance = EXCLUDED.balance, last_entry_id = 0, last_entry_at = now()
	`, holder, f.currencyID, f.pendingID, amount)
	require.NoError(t, err)
}

// TestConfirmPending_RejectsInflatedCheckpoint is I-64's pin: the amount
// ConfirmPending is willing to move out of the pending classification comes
// from journal_entries alone.
//
// Why this path and not only Reserve's (I-49): ConfirmPending mints spendable
// balance. Its journal debits `pending` and credits the holder's `main_wallet`
// (a balance_role='available' classification), and that journal is signed by
// the application's own Attestor -- so once it commits, BOTH terms of I-49's
// withdrawal gate accept it. E sums it because it is a real entry; V accepts it
// because the signature is genuine. A forged balance_checkpoints row on the
// pending dimension was therefore laundered into verified, withdrawable money
// by one ordinary API call, and the tamper-evidence machinery reported nothing,
// because nothing it watches was wrong.
func TestConfirmPending_RejectsInflatedCheckpoint(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupPendingGateFixture(t, pool, ctx, "USDT-PGCP")
	const holder int64 = 9401

	_, err := f.writer.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  holder,
		CurrencyUID:    f.currencyUID,
		Amount:         decimal.NewFromInt(100),
		IdempotencyKey: postgrestest.UniqueKey("pgcp-add"),
		Source:         "test",
	})
	require.NoError(t, err)

	// Control 1: the gate still confirms what the entries genuinely carry.
	// Without this the pin could pass by refusing everything.
	_, err = f.writer.ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder:  holder,
		CurrencyUID:    f.currencyUID,
		Amount:         decimal.NewFromInt(40),
		IdempotencyKey: postgrestest.UniqueKey("pgcp-honest"),
		Source:         "test",
	})
	require.NoError(t, err, "a partial confirm the real pending balance covers must still succeed")

	// The attack: one INSERT on the pending dimension's checkpoint row.
	inflatePendingCheckpoint(t, pool, ctx, f, holder, 1_000_000)

	// Control 2: the tampering really is in effect, and it really is what the
	// pre-fix gate read. BalanceReader.GetBalance IS checkpoint + delta -- the
	// exact call ConfirmPending used to make -- so this asserts the attack
	// landed rather than assuming it.
	tampered, err := f.svc.BalanceReader().GetBalance(ctx, holder, f.currencyUID, f.pendingUID)
	require.NoError(t, err)
	require.True(t, tampered.GreaterThanOrEqual(decimal.NewFromInt(1_000_000)),
		"setup: checkpoint+delta must report the forged figure, got %s", tampered)

	// The pin.
	_, err = f.writer.ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder:  holder,
		CurrencyUID:    f.currencyUID,
		Amount:         decimal.NewFromInt(1000),
		IdempotencyKey: postgrestest.UniqueKey("pgcp-gated"),
		Source:         "test",
	})
	require.Error(t, err, "ConfirmPending must size the confirm off the entries-only recompute, not off balance_checkpoints")
	require.ErrorIs(t, err, core.ErrInsufficientBalance)

	// And no money was created: main_wallet is still the honest 40.
	mainWallet, err := f.svc.Classifications().GetByCode(ctx, "main_wallet")
	require.NoError(t, err)
	spendable, err := f.svc.CheckpointIntegrity().RecomputeBalance(ctx, holder, f.currencyUID, mainWallet.UID)
	require.NoError(t, err)
	require.True(t, spendable.Equal(decimal.NewFromInt(40)),
		"the forged pending balance must not have reached main_wallet, got %s", spendable)
}

// TestCancelPending_RejectsInflatedCheckpoint is the sibling half. Cancel is
// not itself a money-creating path -- it moves pending back to the system's
// suspense account -- but it shares checkPendingBalanceAndPost with confirm,
// and an over-large cancel drives the holder's pending balance negative, which
// a later honest confirm would then read as a debt. It reads entries for the
// same reason, and this pin keeps the two halves from drifting apart.
func TestCancelPending_RejectsInflatedCheckpoint(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupPendingGateFixture(t, pool, ctx, "USDT-PGCX")
	const holder int64 = 9402

	_, err := f.writer.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  holder,
		CurrencyUID:    f.currencyUID,
		Amount:         decimal.NewFromInt(100),
		IdempotencyKey: postgrestest.UniqueKey("pgcx-add"),
		Source:         "test",
	})
	require.NoError(t, err)

	// Control: an honest partial cancel still works.
	_, err = f.writer.CancelPending(ctx, core.CancelPendingInput{
		AccountHolder:  holder,
		CurrencyUID:    f.currencyUID,
		Amount:         decimal.NewFromInt(30),
		IdempotencyKey: postgrestest.UniqueKey("pgcx-honest"),
		Source:         "test",
	})
	require.NoError(t, err)

	inflatePendingCheckpoint(t, pool, ctx, f, holder, 1_000_000)

	tampered, err := f.svc.BalanceReader().GetBalance(ctx, holder, f.currencyUID, f.pendingUID)
	require.NoError(t, err)
	require.True(t, tampered.GreaterThanOrEqual(decimal.NewFromInt(1_000_000)),
		"setup: checkpoint+delta must report the forged figure, got %s", tampered)

	_, err = f.writer.CancelPending(ctx, core.CancelPendingInput{
		AccountHolder:  holder,
		CurrencyUID:    f.currencyUID,
		Amount:         decimal.NewFromInt(1000),
		IdempotencyKey: postgrestest.UniqueKey("pgcx-gated"),
		Source:         "test",
	})
	require.Error(t, err, "CancelPending must size the cancel off the entries-only recompute, not off balance_checkpoints")
	require.ErrorIs(t, err, core.ErrInsufficientBalance)

	// The true pending balance is untouched by the refusal: 100 - 30.
	truth, err := f.svc.CheckpointIntegrity().RecomputeBalance(ctx, holder, f.currencyUID, f.pendingUID)
	require.NoError(t, err)
	require.True(t, truth.Equal(decimal.NewFromInt(70)), "expected 70 pending, got %s", truth)
}

// TestPendingGate_LegitimatePathUnchanged is the other direction of the same
// claim: swapping the gate's source from checkpoint + delta to an entries-only
// recompute must be invisible to every honest caller. Full confirm, split
// confirm, cancel of the remainder, and the refusal of an over-confirm all
// behave exactly as before -- on a database where the two figures agree, which
// is every database nobody has tampered with.
func TestPendingGate_LegitimatePathUnchanged(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupPendingGateFixture(t, pool, ctx, "USDT-PGOK")

	t.Run("full confirm", func(t *testing.T) {
		const holder int64 = 9411
		_, err := f.writer.AddPending(ctx, core.AddPendingInput{
			AccountHolder: holder, CurrencyUID: f.currencyUID, Amount: decimal.NewFromInt(250),
			IdempotencyKey: postgrestest.UniqueKey("pgok-full-add"), Source: "test",
		})
		require.NoError(t, err)

		_, err = f.writer.ConfirmPending(ctx, core.ConfirmPendingInput{
			AccountHolder: holder, CurrencyUID: f.currencyUID, Amount: decimal.NewFromInt(250),
			IdempotencyKey: postgrestest.UniqueKey("pgok-full-confirm"), Source: "test",
		})
		require.NoError(t, err, "a confirm for exactly the pending balance must be allowed")

		pending, err := f.svc.CheckpointIntegrity().RecomputeBalance(ctx, holder, f.currencyUID, f.pendingUID)
		require.NoError(t, err)
		require.True(t, pending.IsZero(), "pending must be drained, got %s", pending)
	})

	t.Run("partial confirms then cancel the remainder", func(t *testing.T) {
		const holder int64 = 9412
		_, err := f.writer.AddPending(ctx, core.AddPendingInput{
			AccountHolder: holder, CurrencyUID: f.currencyUID, Amount: decimal.NewFromInt(300),
			IdempotencyKey: postgrestest.UniqueKey("pgok-part-add"), Source: "test",
		})
		require.NoError(t, err)

		for _, amount := range []int64{120, 80} {
			_, err = f.writer.ConfirmPending(ctx, core.ConfirmPendingInput{
				AccountHolder: holder, CurrencyUID: f.currencyUID, Amount: decimal.NewFromInt(amount),
				IdempotencyKey: postgrestest.UniqueKey("pgok-part-confirm"), Source: "test",
			})
			require.NoErrorf(t, err, "partial confirm of %d must be allowed", amount)
		}

		_, err = f.writer.CancelPending(ctx, core.CancelPendingInput{
			AccountHolder: holder, CurrencyUID: f.currencyUID, Amount: decimal.NewFromInt(100),
			IdempotencyKey: postgrestest.UniqueKey("pgok-part-cancel"), Source: "test",
		})
		require.NoError(t, err, "cancelling exactly the remainder must be allowed")

		pending, err := f.svc.CheckpointIntegrity().RecomputeBalance(ctx, holder, f.currencyUID, f.pendingUID)
		require.NoError(t, err)
		require.True(t, pending.IsZero(), "pending must be drained, got %s", pending)
	})

	t.Run("over-confirm refused", func(t *testing.T) {
		const holder int64 = 9413
		_, err := f.writer.AddPending(ctx, core.AddPendingInput{
			AccountHolder: holder, CurrencyUID: f.currencyUID, Amount: decimal.NewFromInt(10),
			IdempotencyKey: postgrestest.UniqueKey("pgok-over-add"), Source: "test",
		})
		require.NoError(t, err)

		_, err = f.writer.ConfirmPending(ctx, core.ConfirmPendingInput{
			AccountHolder: holder, CurrencyUID: f.currencyUID, Amount: decimal.NewFromInt(11),
			IdempotencyKey: postgrestest.UniqueKey("pgok-over-confirm"), Source: "test",
		})
		require.ErrorIs(t, err, core.ErrInsufficientBalance)
	})
}

// TestConfirmPending_RecomputesUnderLock pins the placement of the recompute,
// which is the half a reader is most likely to "optimize" away: the
// entries-only sum is taken INSIDE the (holder, currency) advisory lock, on
// the transaction that posts the journal, not once before it.
//
// Same construction as I-49's RechecksUnderLock. A second goroutine holds the
// balance lock by posting a genuine journal that drains the pending balance on
// its own transaction, and does not commit until the ConfirmPending under test
// is observably queued behind that lock in pg_locks. Moving the recompute
// outside the transaction would read the pre-drain 1000 and confirm 500 out of
// a pending balance of 50 -- the over-sell race I-4's lock exists to close.
//
// Unlike the two checkpoint pins above, this one was already green before the
// entries-only change: the old checkpoint + delta read was under the lock too.
// It is here to keep it that way -- the fix swapped the gate's SOURCE, and the
// obvious way to lose its PLACEMENT is to hoist the now-heavier full-history
// recompute out of the transaction as an optimization.
func TestConfirmPending_RecomputesUnderLock(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupPendingGateFixture(t, pool, ctx, "USDT-PGLK")
	const holder int64 = 9403

	_, err := f.writer.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  holder,
		CurrencyUID:    f.currencyUID,
		Amount:         decimal.NewFromInt(1000),
		IdempotencyKey: postgrestest.UniqueKey("pglk-add"),
		Source:         "test",
	})
	require.NoError(t, err)

	suspense, err := f.svc.Classifications().GetByCode(ctx, "suspense")
	require.NoError(t, err)
	releaseType, err := f.svc.JournalTypes().GetJournalTypeByCode(ctx, "deposit_release_pending")
	require.NoError(t, err)

	// The competing drain: deposit_release_pending's own shape (DR pending
	// user / CR suspense system), posted directly so the test owns the
	// transaction and therefore the lock.
	drain := core.JournalInput{
		JournalTypeUID: releaseType.UID,
		IdempotencyKey: postgrestest.UniqueKey("pglk-drain"),
		Source:         "test",
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: f.currencyUID, ClassificationUID: f.pendingUID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(950)},
			{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: f.currencyUID, ClassificationUID: suspense.UID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(950)},
		},
	}

	lockHeld := make(chan struct{})
	committed := make(chan error, 1)
	go func() {
		tx, err := pool.Begin(ctx)
		if err != nil {
			committed <- err
			close(lockHeld)
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()

		if _, err := postgres.NewLedgerStore(pool).WithDB(tx).PostJournal(ctx, drain); err != nil {
			committed <- err
			close(lockHeld)
			return
		}
		close(lockHeld)

		if err := waitForBlockedAdvisoryLock(ctx, pool); err != nil {
			committed <- err
			return
		}
		committed <- tx.Commit(ctx)
	}()

	<-lockHeld

	_, err = f.writer.ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder:  holder,
		CurrencyUID:    f.currencyUID,
		Amount:         decimal.NewFromInt(500),
		IdempotencyKey: postgrestest.UniqueKey("pglk-gated"),
		Source:         "test",
	})
	require.NoError(t, <-committed, "test setup: the concurrent drain must commit")
	require.Error(t, err, "ConfirmPending must recompute the pending balance under the balance lock, after the drain became visible")
	require.ErrorIs(t, err, core.ErrInsufficientBalance)

	pending, err := f.svc.CheckpointIntegrity().RecomputeBalance(ctx, holder, f.currencyUID, f.pendingUID)
	require.NoError(t, err)
	require.True(t, pending.Equal(decimal.NewFromInt(50)),
		"the drain really did land, so the refusal is about the recomputed 50; got %s", pending)
}

// TestConfirmPending_EntriesOnlyGateDoesNotAuthenticateEntries pins I-64's
// stated boundary as measured behaviour rather than as a caveat in prose.
//
// The gate reads entries instead of the checkpoint, and entries carry no
// authorization check -- summing them under the balance lock is pure SQL
// precisely so it makes no external call while the transaction is open
// (financial.md), which is the same trade I-49 names for its E term. I-49
// covers it with a second term, V, verified before the transaction opens.
// ConfirmPending has no V term, so the discipline it enforces is "the amount
// exists in journal_entries", not "the amount was authorized".
//
// That leaves ledger_app able to forge an AddPending-shaped journal (two
// INSERTs, nothing signs it) and then launder it: ConfirmPending moves it into
// main_wallet on a journal this store signs with the real Attestor, so the
// withdrawal gate sees main_wallet funded by one genuinely signed journal and
// both of its terms accept it. Forging into main_wallet directly does not work
// -- that journal is unsigned and V refuses it -- so this call is the only
// laundry on the path, which is exactly why the boundary is worth pinning.
//
// This test asserts the CURRENT boundary, deliberately. It must go RED the day
// a V term is added to this gate, and whoever makes it red is expected to
// delete it and rewrite I-64's "What this does not close" section rather than
// adjust the assertion. It is not a claim that the behaviour is desirable.
func TestConfirmPending_EntriesOnlyGateDoesNotAuthenticateEntries(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupPendingGateFixture(t, pool, ctx, "USDT-PGFE")
	const holder int64 = 9404
	app := ledgerAppPool(t, ctx, pool, "w4-pending-gate-residual")

	suspense, err := f.svc.Classifications().GetByCode(ctx, "suspense")
	require.NoError(t, err)
	mainWallet, err := f.svc.Classifications().GetByCode(ctx, "main_wallet")
	require.NoError(t, err)

	var suspenseID, journalTypeID int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM classifications WHERE uid = $1`, suspense.UID).Scan(&suspenseID))
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM journal_types WHERE code = 'deposit_pending'`).Scan(&journalTypeID))

	// The forgery, issued over a real socket AS ledger_app -- the claim is
	// about what the application's own credential can reach, so running it as
	// the container superuser would prove nothing (same rule I-49's hold pin
	// follows).
	tx, err := app.Begin(ctx)
	require.NoError(t, err)
	var forgedJournalID int64
	require.NoError(t, tx.QueryRow(ctx, `
		INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit, uid)
		VALUES ($1, 'w4-residual-forged', 5000, 5000, gen_random_uuid())
		RETURNING id`, journalTypeID).Scan(&forgedJournalID),
		"ledger_app must be able to INSERT a journal -- if this now fails the residual is closed and this pin should be deleted")
	_, err = tx.Exec(ctx, `
		INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
		VALUES ($1, $2, $3, $4, 'debit', 5000), ($1, $5, $3, $6, 'credit', 5000)`,
		forgedJournalID, core.SystemAccountHolder(holder), f.currencyID, suspenseID, holder, f.pendingID)
	require.NoError(t, err, "ledger_app holds column-scoped INSERT on journal_entries (migration 008)")
	require.NoError(t, tx.Commit(ctx))

	var forgedStatus string
	require.NoError(t, pool.QueryRow(ctx, `SELECT auth_status FROM journals WHERE id = $1`, forgedJournalID).Scan(&forgedStatus))
	require.Equal(t, "unsigned_no_attestor", forgedStatus, "setup: the forged journal is unsigned, which is why V would refuse it directly")

	entriesOnly, err := f.svc.CheckpointIntegrity().RecomputeBalance(ctx, holder, f.currencyUID, f.pendingUID)
	require.NoError(t, err)
	require.True(t, entriesOnly.Equal(decimal.NewFromInt(5000)),
		"the forged entries are real entries, so the entries-only recompute counts them; got %s", entriesOnly)

	confirmed, err := f.writer.ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder:  holder,
		CurrencyUID:    f.currencyUID,
		Amount:         decimal.NewFromInt(5000),
		IdempotencyKey: postgrestest.UniqueKey("pgfe-launder"),
		Source:         "test",
	})
	require.NoError(t, err, "BOUNDARY: the gate authenticates nothing, so a forged entry passes it. If this now errors, see this test's doc comment")

	var confirmStatus string
	require.NoError(t, pool.QueryRow(ctx, `SELECT auth_status FROM journals WHERE uid = $1`, confirmed.UID).Scan(&confirmStatus))
	require.Equal(t, "signed", confirmStatus,
		"BOUNDARY: and the laundering journal is genuinely signed, which is what makes the result indistinguishable from a real deposit downstream")

	spendable, err := f.svc.CheckpointIntegrity().RecomputeBalance(ctx, holder, f.currencyUID, mainWallet.UID)
	require.NoError(t, err)
	require.True(t, spendable.Equal(decimal.NewFromInt(5000)),
		"BOUNDARY: 5000 of spendable balance now exists behind one signed journal; got %s", spendable)
}
