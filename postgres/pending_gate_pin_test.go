package postgres_test

import (
	"context"
	"fmt"
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

// forgeUnsignedPendingCredit inserts, as ledger_app over a real socket, an
// AddPending-shaped journal that nothing signed: DR suspense (system) / CR
// pending (user). Two INSERTs, both within what the application's own
// credential holds (migration 008 gives it column-scoped INSERT on
// journal_entries), and the standing threat model assumes that credential is
// leaked. Running it as the container superuser would prove nothing.
func forgeUnsignedPendingCredit(t *testing.T, app *pgxpool.Pool, ctx context.Context, f pendingGateFixture, holder int64, key string, amount int64) int64 {
	t.Helper()

	var suspenseID, journalTypeID int64
	require.NoError(t, app.QueryRow(ctx, `SELECT id FROM classifications WHERE code = 'suspense'`).Scan(&suspenseID))
	require.NoError(t, app.QueryRow(ctx, `SELECT id FROM journal_types WHERE code = 'deposit_pending'`).Scan(&journalTypeID))

	tx, err := app.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	var journalID int64
	require.NoError(t, tx.QueryRow(ctx, `
		INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit, uid)
		VALUES ($1, $2, $3, $3, gen_random_uuid())
		RETURNING id`, journalTypeID, key, amount).Scan(&journalID),
		"setup: ledger_app must be able to INSERT a journal for this pin to mean anything")
	_, err = tx.Exec(ctx, `
		INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
		VALUES ($1, $2, $3, $4, 'debit', $7), ($1, $5, $3, $6, 'credit', $7)`,
		journalID, core.SystemAccountHolder(holder), f.currencyID, suspenseID, holder, f.pendingID, amount)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	var status string
	require.NoError(t, app.QueryRow(ctx, `SELECT auth_status FROM journals WHERE id = $1`, journalID).Scan(&status))
	require.Equal(t, "unsigned_no_attestor", status, "setup: the forged journal must be unsigned -- that is what V is supposed to catch")
	return journalID
}

// TestConfirmPending_RefusesForgedPendingEntries is the V term's pin
// (contract §7.20). It is the reversal of the boundary this task first
// measured and reported: reading entries instead of the checkpoint made the
// gate's figure real, but not authorized, and ConfirmPending was the one call
// that could launder an unauthorized figure into an authorized one.
//
// The laundering, in full: `pending` is credited by a forged unsigned journal;
// `ConfirmPending` moves it to `main_wallet` on a journal this store signs
// with the deployment's real Attestor; the withdrawal gate (I-49) then sees
// `main_wallet` funded by one genuinely signed journal and both of its terms
// accept it. Forging into `main_wallet` directly does not work — that journal
// is unsigned and I-49's V refuses it — so closing this call closes the path.
//
// One unauthorized contributor refuses the whole confirm. There is no
// "exclude the forged one and confirm the rest": that is I-32's UNDEFINED
// rule, and the reason for it is that excluding an unauthorized entry can
// report MORE money, not less.
func TestConfirmPending_RefusesForgedPendingEntries(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupPendingGateFixture(t, pool, ctx, "USDT-PGFE")
	const holder int64 = 9404
	app := ledgerAppPool(t, ctx, pool, "w4-pending-gate-forged")

	// A genuine, signed deposit first, so the refusal below cannot be "the
	// gate refuses everything" and so the forged journal is not the dimension's
	// only contributor.
	_, err := f.writer.AddPending(ctx, core.AddPendingInput{
		AccountHolder: holder, CurrencyUID: f.currencyUID, Amount: decimal.NewFromInt(100),
		IdempotencyKey: postgrestest.UniqueKey("pgfe-add"), Source: "test",
	})
	require.NoError(t, err)

	// Control: the honest 100 confirms while it is the only contributor.
	_, err = f.writer.ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder: holder, CurrencyUID: f.currencyUID, Amount: decimal.NewFromInt(40),
		IdempotencyKey: postgrestest.UniqueKey("pgfe-honest"), Source: "test",
	})
	require.NoError(t, err, "the V term must not refuse a dimension whose journals are all genuinely signed")

	forgeUnsignedPendingCredit(t, app, ctx, f, holder, "pgfe-forged", 5000)

	entriesOnly, err := f.svc.CheckpointIntegrity().RecomputeBalance(ctx, holder, f.currencyUID, f.pendingUID)
	require.NoError(t, err)
	require.True(t, entriesOnly.Equal(decimal.NewFromInt(5060)),
		"setup: the forged entries are real entries, so E counts them -- that is exactly why E alone was not enough; got %s", entriesOnly)

	// The pin: the amount is now present in journal_entries and E would allow
	// it, so only the signature check can refuse.
	_, err = f.writer.ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder: holder, CurrencyUID: f.currencyUID, Amount: decimal.NewFromInt(5000),
		IdempotencyKey: postgrestest.UniqueKey("pgfe-launder"), Source: "test",
	})
	require.Error(t, err, "ConfirmPending must verify every journal contributing to the pending dimension before it signs anything into main_wallet")
	require.ErrorIs(t, err, core.ErrUnauthorizedJournal)

	// UNDEFINED, not "confirm the authorized remainder": even the 60 that is
	// genuinely there is refused while an unauthorized contributor exists.
	_, err = f.writer.ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder: holder, CurrencyUID: f.currencyUID, Amount: decimal.NewFromInt(60),
		IdempotencyKey: postgrestest.UniqueKey("pgfe-remainder"), Source: "test",
	})
	require.ErrorIs(t, err, core.ErrUnauthorizedJournal,
		"one unauthorized contributor makes the whole dimension UNDEFINED (I-32); confirming the 'authorized remainder' is the excluded behaviour")

	// And nothing reached main_wallet beyond the honest 40.
	mainWallet, err := f.svc.Classifications().GetByCode(ctx, "main_wallet")
	require.NoError(t, err)
	spendable, err := f.svc.CheckpointIntegrity().RecomputeBalance(ctx, holder, f.currencyUID, mainWallet.UID)
	require.NoError(t, err)
	require.True(t, spendable.Equal(decimal.NewFromInt(40)), "expected the honest 40 only, got %s", spendable)
}

// TestConfirmPending_VerifiedBaseCapsEntriesForgedInTheWindow is why the gate
// takes min(V, E) rather than using V as a yes/no and letting E decide the
// amount.
//
// V has to be computed before the transaction opens (a core.AuthVerifier may
// be remote, and financial.md forbids external calls inside a transaction), so
// there is a window between the verification and the advisory lock. An
// attacker who lands a forged credit inside that window is not seen by V at
// all, and E — pure SQL under the lock, no authorization check by
// construction — counts it. "Verify, then let an unverified number decide how
// much" is that window left open, and it is retryable until it hits.
//
// Constructed like RecomputesUnderLock, except the competing transaction takes
// the (holder, currency) balance lock directly and forges inside it, so the
// forgery becomes visible to E and only to E.
func TestConfirmPending_VerifiedBaseCapsEntriesForgedInTheWindow(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupPendingGateFixture(t, pool, ctx, "USDT-PGWD")
	const holder int64 = 9405
	app := ledgerAppPool(t, ctx, pool, "w4-pending-gate-window")

	_, err := f.writer.AddPending(ctx, core.AddPendingInput{
		AccountHolder: holder, CurrencyUID: f.currencyUID, Amount: decimal.NewFromInt(100),
		IdempotencyKey: postgrestest.UniqueKey("pgwd-add"), Source: "test",
	})
	require.NoError(t, err)

	lockHeld := make(chan struct{})
	committed := make(chan error, 1)
	go func() {
		tx, err := app.Begin(ctx)
		if err != nil {
			committed <- err
			close(lockHeld)
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()

		// The same lock ConfirmPending will queue on, taken by key rather than
		// by posting a journal: this transaction must hold the lock while
		// writing something the gate has already looked past.
		if _, err := tx.Exec(ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended('bal:' || $1::text, 0))`,
			fmt.Sprintf("balance:%d:%d", holder, f.currencyID),
		); err != nil {
			committed <- err
			close(lockHeld)
			return
		}

		var suspenseID, journalTypeID int64
		if err := tx.QueryRow(ctx, `SELECT id FROM classifications WHERE code = 'suspense'`).Scan(&suspenseID); err != nil {
			committed <- err
			close(lockHeld)
			return
		}
		if err := tx.QueryRow(ctx, `SELECT id FROM journal_types WHERE code = 'deposit_pending'`).Scan(&journalTypeID); err != nil {
			committed <- err
			close(lockHeld)
			return
		}
		var journalID int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit, uid)
			VALUES ($1, 'pgwd-window-forged', 5000, 5000, gen_random_uuid())
			RETURNING id`, journalTypeID).Scan(&journalID); err != nil {
			committed <- err
			close(lockHeld)
			return
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
			VALUES ($1, $2, $3, $4, 'debit', 5000), ($1, $5, $3, $6, 'credit', 5000)`,
			journalID, core.SystemAccountHolder(holder), f.currencyID, suspenseID, holder, f.pendingID); err != nil {
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

	// V runs now, outside any transaction: it sees only the committed, signed
	// 100, and the forgery is still invisible. Then this blocks on the lock,
	// the forgery commits, and E comes back 5100.
	_, err = f.writer.ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder: holder, CurrencyUID: f.currencyUID, Amount: decimal.NewFromInt(1000),
		IdempotencyKey: postgrestest.UniqueKey("pgwd-gated"), Source: "test",
	})
	require.NoError(t, <-committed, "test setup: the window forgery must commit")
	require.Error(t, err, "the confirm must be capped by V, which never saw the forgery; using E alone here leaves the verify-then-trust window open")
	require.ErrorIs(t, err, core.ErrInsufficientBalance)

	entriesOnly, err := f.svc.CheckpointIntegrity().RecomputeBalance(ctx, holder, f.currencyUID, f.pendingUID)
	require.NoError(t, err)
	require.True(t, entriesOnly.Equal(decimal.NewFromInt(5100)),
		"the forgery really did land and E really would have allowed 1000; got %s", entriesOnly)

	mainWallet, err := f.svc.Classifications().GetByCode(ctx, "main_wallet")
	require.NoError(t, err)
	spendable, err := f.svc.CheckpointIntegrity().RecomputeBalance(ctx, holder, f.currencyUID, mainWallet.UID)
	require.NoError(t, err)
	require.True(t, spendable.IsZero(), "no spendable balance may have been created, got %s", spendable)
}

// TestConfirmPending_TxBoundStoreFailsClosed pins the RunInTx half of §7.20:
// with the gate configured, a ConfirmPending composed inside a caller's
// transaction is refused rather than run ungated.
//
// The alternative — degrade to E-only inside RunInTx — is the silent downgrade
// working-agreements §3 exists to prevent: the same call would be gated or not
// depending on how the consumer happened to compose it, with nothing in the
// result saying which. Deliberately asserted BEFORE the callback writes
// anything, and asserted again from outside, so "refused" means "the whole
// transaction produced nothing", not "returned an error after posting".
func TestConfirmPending_TxBoundStoreFailsClosed(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupPendingGateFixture(t, pool, ctx, "USDT-PGTX")
	const holder int64 = 9406

	_, err := f.writer.AddPending(ctx, core.AddPendingInput{
		AccountHolder: holder, CurrencyUID: f.currencyUID, Amount: decimal.NewFromInt(100),
		IdempotencyKey: postgrestest.UniqueKey("pgtx-add"), Source: "test",
	})
	require.NoError(t, err)

	err = f.svc.RunInTx(ctx, func(tx *ledger.Service) error {
		_, err := tx.PendingBalanceWriter().ConfirmPending(ctx, core.ConfirmPendingInput{
			AccountHolder: holder, CurrencyUID: f.currencyUID, Amount: decimal.NewFromInt(100),
			IdempotencyKey: postgrestest.UniqueKey("pgtx-inside"), Source: "test",
		})
		return err
	})
	require.Error(t, err, "a tx-bound ConfirmPending must fail closed while the verified-balance gate is configured")
	require.ErrorIs(t, err, core.ErrInvalidInput)

	// Nothing moved.
	pending, err := f.svc.CheckpointIntegrity().RecomputeBalance(ctx, holder, f.currencyUID, f.pendingUID)
	require.NoError(t, err)
	require.True(t, pending.Equal(decimal.NewFromInt(100)), "expected the pending 100 untouched, got %s", pending)

	// The same confirm outside RunInTx is the documented remedy, and works.
	_, err = f.writer.ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder: holder, CurrencyUID: f.currencyUID, Amount: decimal.NewFromInt(100),
		IdempotencyKey: postgrestest.UniqueKey("pgtx-outside"), Source: "test",
	})
	require.NoError(t, err, "the error message tells the caller to confirm before opening RunInTx; that must actually work")

	// CancelPending has no V term, so it is NOT refused in tx mode -- the
	// asymmetry is deliberate (it creates no spendable balance) and is
	// asserted rather than left to be discovered.
	_, err = f.writer.AddPending(ctx, core.AddPendingInput{
		AccountHolder: holder, CurrencyUID: f.currencyUID, Amount: decimal.NewFromInt(70),
		IdempotencyKey: postgrestest.UniqueKey("pgtx-add2"), Source: "test",
	})
	require.NoError(t, err)
	require.NoError(t, f.svc.RunInTx(ctx, func(tx *ledger.Service) error {
		_, err := tx.PendingBalanceWriter().CancelPending(ctx, core.CancelPendingInput{
			AccountHolder: holder, CurrencyUID: f.currencyUID, Amount: decimal.NewFromInt(70),
			IdempotencyKey: postgrestest.UniqueKey("pgtx-cancel"), Source: "test",
		})
		return err
	}), "CancelPending composes inside RunInTx as it always did")
}

// TestConfirmPending_WithoutAttestorIsEntriesOnly pins the other side of the
// wiring: a deployment that configured no core.Attestor gets no V term, and
// I-64 says so out loud instead of implying the gate is tamper-resistant
// everywhere.
//
// There is nothing to verify in that deployment — its own journals are
// unsigned, so a verifier would refuse every confirm, honest ones included.
// The gate is E alone: real entries, unauthenticated. This test holds that
// boundary as measured behaviour, and must go red if the wiring ever starts
// verifying without an Attestor (at which point the honest path breaks, which
// is precisely what this pin would catch).
func TestConfirmPending_WithoutAttestorIsEntriesOnly(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	svc, err := ledger.New(pool) // no WithAttestor
	require.NoError(t, err)
	require.NoError(t, presets.InstallPendingBundle(ctx, svc.Classifications(), svc.JournalTypes(), svc.Templates()))

	currencyUID := postgrestest.SeedCurrency(t, pool, "USDT-PGNA", "Pending gate no attestor")
	pendingCls, err := svc.Classifications().GetByCode(ctx, "pending")
	require.NoError(t, err)

	f := pendingGateFixture{svc: svc, writer: svc.PendingBalanceWriter(), currencyUID: currencyUID, pendingUID: pendingCls.UID}
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM currencies WHERE uid = $1`, currencyUID).Scan(&f.currencyID))
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM classifications WHERE uid = $1`, pendingCls.UID).Scan(&f.pendingID))

	const holder int64 = 9407
	app := ledgerAppPool(t, ctx, pool, "w4-pending-gate-noattestor")

	// The honest path works, which is the whole reason the gate is off here:
	// with no Attestor these journals are unsigned, so a V term would refuse
	// this too.
	_, err = f.writer.AddPending(ctx, core.AddPendingInput{
		AccountHolder: holder, CurrencyUID: currencyUID, Amount: decimal.NewFromInt(100),
		IdempotencyKey: postgrestest.UniqueKey("pgna-add"), Source: "test",
	})
	require.NoError(t, err)
	_, err = f.writer.ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder: holder, CurrencyUID: currencyUID, Amount: decimal.NewFromInt(100),
		IdempotencyKey: postgrestest.UniqueKey("pgna-confirm"), Source: "test",
	})
	require.NoError(t, err, "an unsigned deployment must still be able to confirm its own deposits")

	// E still holds: a forged CHECKPOINT buys nothing even here.
	inflatePendingCheckpoint(t, pool, ctx, f, holder, 1_000_000)
	_, err = f.writer.ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder: holder, CurrencyUID: currencyUID, Amount: decimal.NewFromInt(1000),
		IdempotencyKey: postgrestest.UniqueKey("pgna-checkpoint"), Source: "test",
	})
	require.ErrorIs(t, err, core.ErrInsufficientBalance, "the entries-only term applies with or without an Attestor")

	// BOUNDARY: forged ENTRIES do buy something, because nothing here can tell
	// a signed journal from an unsigned one. Stated in I-64, pinned here.
	forgeUnsignedPendingCredit(t, app, ctx, f, holder, "pgna-forged", 5000)
	_, err = f.writer.ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder: holder, CurrencyUID: currencyUID, Amount: decimal.NewFromInt(5000),
		IdempotencyKey: postgrestest.UniqueKey("pgna-launder"), Source: "test",
	})
	require.NoError(t, err,
		"BOUNDARY (I-64): with no Attestor there is no V term and forged entries pass. If this now errors, the wiring changed -- update I-64's boundary section rather than this assertion")

	// And a tx-bound confirm is NOT refused here, because there is no remote
	// call to keep out of the transaction.
	_, err = f.writer.AddPending(ctx, core.AddPendingInput{
		AccountHolder: holder, CurrencyUID: currencyUID, Amount: decimal.NewFromInt(20),
		IdempotencyKey: postgrestest.UniqueKey("pgna-add2"), Source: "test",
	})
	require.NoError(t, err)
	require.NoError(t, svc.RunInTx(ctx, func(tx *ledger.Service) error {
		_, err := tx.PendingBalanceWriter().ConfirmPending(ctx, core.ConfirmPendingInput{
			AccountHolder: holder, CurrencyUID: currencyUID, Amount: decimal.NewFromInt(20),
			IdempotencyKey: postgrestest.UniqueKey("pgna-tx"), Source: "test",
		})
		return err
	}), "with no gate configured there is nothing to fail closed on, and RunInTx composition keeps working")
}
