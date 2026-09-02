package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/presets"
)

// TestPendingStore_AddPending verifies that AddPending shifts the amount from
// suspense (system) to pending (user) classification.
func TestPendingStore_AddPending(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()

	cs := postgres.NewClassificationStore(p)
	ls := postgres.NewLedgerStore(p)
	ts := postgres.NewTemplateStore(p)
	require.NoError(t, presets.InstallPendingBundle(ctx, cs, cs, ts))

	curID := postgrestest.SeedCurrency(t, p, "USDT-ADD", "Test USDT")
	ps := postgres.NewPendingStore(p, ls, cs)

	userID := int64(1001)
	amount := decimal.NewFromInt(500)

	j, err := ps.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         amount,
		IdempotencyKey: postgrestest.UniqueKey("add-pending"),
		Source:         "test",
	})
	require.NoError(t, err)
	require.NotNil(t, j)
	assert.True(t, j.TotalDebit.Equal(amount))
	assert.True(t, j.TotalCredit.Equal(amount))

	// Verify pending classification balance for user.
	pendingCls, err := cs.GetByCode(ctx, "pending")
	require.NoError(t, err)
	pendingBal, err := ls.GetBalance(ctx, userID, curID, pendingCls.UID)
	require.NoError(t, err)
	assert.True(t, pendingBal.Equal(amount), "pending balance should equal added amount, got %s", pendingBal)

	// Verify suspense classification balance for system counterpart.
	suspenseCls, err := cs.GetByCode(ctx, "suspense")
	require.NoError(t, err)
	systemHolder := core.SystemAccountHolder(userID)
	suspenseBal, err := ls.GetBalance(ctx, systemHolder, curID, suspenseCls.UID)
	require.NoError(t, err)
	assert.True(t, suspenseBal.Equal(amount), "suspense balance should equal added amount, got %s", suspenseBal)
}

// TestPendingStore_AddPending_Idempotent verifies that posting the same
// idempotency key twice returns the same journal without creating a duplicate.
func TestPendingStore_AddPending_Idempotent(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()

	cs := postgres.NewClassificationStore(p)
	ls := postgres.NewLedgerStore(p)
	ts := postgres.NewTemplateStore(p)
	require.NoError(t, presets.InstallPendingBundle(ctx, cs, cs, ts))

	curID := postgrestest.SeedCurrency(t, p, "USDT-IDEM", "Test USDT")
	ps := postgres.NewPendingStore(p, ls, cs)

	userID := int64(1002)
	amount := decimal.NewFromInt(200)
	key := postgrestest.UniqueKey("add-pending-idem")

	j1, err := ps.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         amount,
		IdempotencyKey: key,
		Source:         "test",
	})
	require.NoError(t, err)

	j2, err := ps.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         amount,
		IdempotencyKey: key,
		Source:         "test",
	})
	require.NoError(t, err)

	assert.Equal(t, j1.UID, j2.UID, "idempotent calls should return the same journal ID")

	// Balance should only reflect one addition.
	pendingCls, err := cs.GetByCode(ctx, "pending")
	require.NoError(t, err)
	pendingBal, err := ls.GetBalance(ctx, userID, curID, pendingCls.UID)
	require.NoError(t, err)
	assert.True(t, pendingBal.Equal(amount), "balance should only reflect one addition")
}

// TestPendingStore_ConfirmPending verifies the happy path: AddPending then
// ConfirmPending shifts funds from pending → main_wallet and clears suspense →
// custodial.
func TestPendingStore_ConfirmPending(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()

	cs := postgres.NewClassificationStore(p)
	ls := postgres.NewLedgerStore(p)
	ts := postgres.NewTemplateStore(p)
	require.NoError(t, presets.InstallPendingBundle(ctx, cs, cs, ts))

	curID := postgrestest.SeedCurrency(t, p, "USDT-CONF", "Test USDT")
	ps := postgres.NewPendingStore(p, ls, cs)

	userID := int64(1003)
	addAmount := decimal.NewFromInt(1000)
	confirmAmount := decimal.NewFromInt(950) // partial confirm (tolerance scenario)

	// Step 1: Add pending
	_, err := ps.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         addAmount,
		IdempotencyKey: postgrestest.UniqueKey("confirm-add"),
		Source:         "test",
	})
	require.NoError(t, err)

	// Step 2: Confirm with a smaller amount (partial)
	j, err := ps.ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         confirmAmount,
		IdempotencyKey: postgrestest.UniqueKey("confirm-confirm"),
		Source:         "test",
	})
	require.NoError(t, err)
	require.NotNil(t, j)

	// Verify balances
	pendingCls, err := cs.GetByCode(ctx, "pending")
	require.NoError(t, err)
	mainWalletCls, err := cs.GetByCode(ctx, "main_wallet")
	require.NoError(t, err)
	custodialCls, err := cs.GetByCode(ctx, "custodial")
	require.NoError(t, err)
	suspenseCls, err := cs.GetByCode(ctx, "suspense")
	require.NoError(t, err)

	systemHolder := core.SystemAccountHolder(userID)
	remaining := addAmount.Sub(confirmAmount)

	pendingBal, err := ls.GetBalance(ctx, userID, curID, pendingCls.UID)
	require.NoError(t, err)
	assert.True(t, pendingBal.Equal(remaining), "pending should be %s after partial confirm, got %s", remaining, pendingBal)

	walletBal, err := ls.GetBalance(ctx, userID, curID, mainWalletCls.UID)
	require.NoError(t, err)
	assert.True(t, walletBal.Equal(confirmAmount), "main_wallet should equal confirmed amount, got %s", walletBal)

	suspenseBal, err := ls.GetBalance(ctx, systemHolder, curID, suspenseCls.UID)
	require.NoError(t, err)
	assert.True(t, suspenseBal.Equal(remaining), "suspense should be %s after partial confirm, got %s", remaining, suspenseBal)

	custodialBal, err := ls.GetBalance(ctx, systemHolder, curID, custodialCls.UID)
	require.NoError(t, err)
	assert.True(t, custodialBal.Equal(confirmAmount), "custodial should equal confirmed amount, got %s", custodialBal)
}

// TestPendingStore_ConfirmPending_Idempotent verifies that calling ConfirmPending
// twice with the same idempotency key returns the same journal and does not
// double-credit the wallet.
func TestPendingStore_ConfirmPending_Idempotent(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()

	cs := postgres.NewClassificationStore(p)
	ls := postgres.NewLedgerStore(p)
	ts := postgres.NewTemplateStore(p)
	require.NoError(t, presets.InstallPendingBundle(ctx, cs, cs, ts))

	curID := postgrestest.SeedCurrency(t, p, "USDT-CONFIDEM", "Test USDT")
	ps := postgres.NewPendingStore(p, ls, cs)

	userID := int64(1004)
	amount := decimal.NewFromInt(300)

	_, err := ps.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         amount,
		IdempotencyKey: postgrestest.UniqueKey("confidem-add"),
		Source:         "test",
	})
	require.NoError(t, err)

	key := postgrestest.UniqueKey("confidem-confirm")
	j1, err := ps.ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         amount,
		IdempotencyKey: key,
		Source:         "test",
	})
	require.NoError(t, err)

	j2, err := ps.ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         amount,
		IdempotencyKey: key,
		Source:         "test",
	})
	require.NoError(t, err)
	assert.Equal(t, j1.UID, j2.UID)

	// Balance should only reflect one confirmation.
	mainWalletCls, err := cs.GetByCode(ctx, "main_wallet")
	require.NoError(t, err)
	walletBal, err := ls.GetBalance(ctx, userID, curID, mainWalletCls.UID)
	require.NoError(t, err)
	assert.True(t, walletBal.Equal(amount), "wallet should reflect exactly one confirmation")
}

// TestPendingStore_CancelPending_Idempotent verifies that calling CancelPending
// twice with the same idempotency key returns the same journal and does not
// double-release the pending balance.
func TestPendingStore_CancelPending_Idempotent(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()

	cs := postgres.NewClassificationStore(p)
	ls := postgres.NewLedgerStore(p)
	ts := postgres.NewTemplateStore(p)
	require.NoError(t, presets.InstallPendingBundle(ctx, cs, cs, ts))

	curID := postgrestest.SeedCurrency(t, p, "USDT-CANCELIDEM", "Test USDT")
	ps := postgres.NewPendingStore(p, ls, cs)

	userID := int64(10041)
	amount := decimal.NewFromInt(300)

	_, err := ps.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         amount,
		IdempotencyKey: postgrestest.UniqueKey("cancelidem-add"),
		Source:         "test",
	})
	require.NoError(t, err)

	key := postgrestest.UniqueKey("cancelidem-cancel")
	j1, err := ps.CancelPending(ctx, core.CancelPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         amount,
		Reason:         "timeout",
		IdempotencyKey: key,
		Source:         "test",
	})
	require.NoError(t, err)

	j2, err := ps.CancelPending(ctx, core.CancelPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         amount,
		Reason:         "timeout",
		IdempotencyKey: key,
		Source:         "test",
	})
	require.NoError(t, err)
	assert.Equal(t, j1.UID, j2.UID)

	pendingCls, err := cs.GetByCode(ctx, "pending")
	require.NoError(t, err)
	pendingBal, err := ls.GetBalance(ctx, userID, curID, pendingCls.UID)
	require.NoError(t, err)
	assert.True(t, pendingBal.IsZero(), "pending should be released exactly once")
}

// TestPendingStore_CancelPending verifies that CancelPending posts a compensating
// journal and the original AddPending journal is not mutated.
func TestPendingStore_CancelPending(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()

	cs := postgres.NewClassificationStore(p)
	ls := postgres.NewLedgerStore(p)
	ts := postgres.NewTemplateStore(p)
	require.NoError(t, presets.InstallPendingBundle(ctx, cs, cs, ts))

	curID := postgrestest.SeedCurrency(t, p, "USDT-CANCEL", "Test USDT")
	ps := postgres.NewPendingStore(p, ls, cs)

	userID := int64(1005)
	amount := decimal.NewFromInt(400)

	addJournal, err := ps.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         amount,
		IdempotencyKey: postgrestest.UniqueKey("cancel-add"),
		Source:         "test",
	})
	require.NoError(t, err)

	cancelJournal, err := ps.CancelPending(ctx, core.CancelPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         amount,
		Reason:         "test_cancellation",
		IdempotencyKey: postgrestest.UniqueKey("cancel-cancel"),
		Source:         "test",
	})
	require.NoError(t, err)
	require.NotNil(t, cancelJournal)

	// Cancel journal must be a different journal from the original.
	assert.NotEqual(t, addJournal.UID, cancelJournal.UID, "cancel must create a new journal")

	// Balances must be zero after cancel.
	pendingCls, err := cs.GetByCode(ctx, "pending")
	require.NoError(t, err)
	suspenseCls, err := cs.GetByCode(ctx, "suspense")
	require.NoError(t, err)

	systemHolder := core.SystemAccountHolder(userID)

	pendingBal, err := ls.GetBalance(ctx, userID, curID, pendingCls.UID)
	require.NoError(t, err)
	assert.True(t, pendingBal.IsZero(), "pending balance must be zero after cancel, got %s", pendingBal)

	suspenseBal, err := ls.GetBalance(ctx, systemHolder, curID, suspenseCls.UID)
	require.NoError(t, err)
	assert.True(t, suspenseBal.IsZero(), "suspense balance must be zero after cancel, got %s", suspenseBal)
}

// TestPendingStore_CancelPending_OriginalNotMutated verifies the compensating
// journal has a different ID from the original (append-only guarantee).
func TestPendingStore_CancelPending_OriginalNotMutated(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()

	cs := postgres.NewClassificationStore(p)
	ls := postgres.NewLedgerStore(p)
	ts := postgres.NewTemplateStore(p)
	require.NoError(t, presets.InstallPendingBundle(ctx, cs, cs, ts))

	curID := postgrestest.SeedCurrency(t, p, "USDT-NOMUT", "Test USDT")
	ps := postgres.NewPendingStore(p, ls, cs)

	userID := int64(1006)
	amount := decimal.NewFromInt(100)

	addJ, err := ps.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         amount,
		IdempotencyKey: postgrestest.UniqueKey("nomut-add"),
		Source:         "test",
	})
	require.NoError(t, err)

	cancelJ, err := ps.CancelPending(ctx, core.CancelPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         amount,
		Reason:         "expired",
		IdempotencyKey: postgrestest.UniqueKey("nomut-cancel"),
		Source:         "test",
	})
	require.NoError(t, err)

	// The cancel must write a NEW journal, never touch the original.
	assert.NotEqual(t, addJ.UID, cancelJ.UID, "cancel journal ID must differ from add journal ID")
	// The original journal must NOT reference the cancel journal.
	assert.Empty(t, addJ.ReversalOfUID, "add journal must not have reversal_of set")
}

// TestPendingStore_CancelPending_InsufficientBalance ensures cancelling more
// than the available pending balance returns ErrInsufficientBalance.
func TestPendingStore_CancelPending_InsufficientBalance(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()

	cs := postgres.NewClassificationStore(p)
	ls := postgres.NewLedgerStore(p)
	ts := postgres.NewTemplateStore(p)
	require.NoError(t, presets.InstallPendingBundle(ctx, cs, cs, ts))

	curID := postgrestest.SeedCurrency(t, p, "USDT-INSUF", "Test USDT")
	ps := postgres.NewPendingStore(p, ls, cs)

	userID := int64(1007)
	amount := decimal.NewFromInt(50)

	_, err := ps.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         amount,
		IdempotencyKey: postgrestest.UniqueKey("insuf-add"),
		Source:         "test",
	})
	require.NoError(t, err)

	// Attempt to cancel more than pending.
	_, err = ps.CancelPending(ctx, core.CancelPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         decimal.NewFromInt(100), // > 50
		Reason:         "test",
		IdempotencyKey: postgrestest.UniqueKey("insuf-cancel"),
		Source:         "test",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInsufficientBalance)
}

// TestPendingStore_ExpirePendingOlderThan verifies that the sweeper cancels
// pending deposits older than the threshold and leaves newer ones untouched.
//
// Design note: the journals append-only trigger (migration 018) prevents
// UPDATE on journals.created_at.  To deterministically place a journal in the
// "stale" bucket we pass a negative threshold (e.g. -1 second), which makes
// cutoff = now() + 1s — every journal created up to this point satisfies
// created_at < cutoff.  A "fresh" journal for user B is added AFTER the
// sweeper call to verify it would have been left alone.
func TestPendingStore_ExpirePendingOlderThan(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()

	cs := postgres.NewClassificationStore(p)
	ls := postgres.NewLedgerStore(p)
	ts := postgres.NewTemplateStore(p)
	require.NoError(t, presets.InstallPendingBundle(ctx, cs, cs, ts))

	curID := postgrestest.SeedCurrency(t, p, "USDT-EXP", "Test USDT")
	ps := postgres.NewPendingStore(p, ls, cs)

	// Add a "stale" deposit for user A.
	userA := int64(2001)
	amountA := decimal.NewFromInt(300)

	_, err := ps.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  userA,
		CurrencyUID:    curID,
		Amount:         amountA,
		IdempotencyKey: postgrestest.UniqueKey("expire-add-a"),
		Source:         "test",
	})
	require.NoError(t, err)

	// Pass threshold=-1s → cutoff=now()+1s → every existing journal is "stale".
	cancelled, err := ps.ExpirePendingOlderThan(ctx, -1*time.Second)
	require.NoError(t, err)
	assert.Equal(t, 1, cancelled, "exactly one stale deposit should be expired")

	// Verify user A's pending balance is now zero.
	pendingCls, err := cs.GetByCode(ctx, "pending")
	require.NoError(t, err)

	balA, err := ls.GetBalance(ctx, userA, curID, pendingCls.UID)
	require.NoError(t, err)
	assert.True(t, balA.IsZero(), "user A pending balance must be zero after expiry, got %s", balA)

	// Add a "fresh" deposit for user B AFTER the sweep — should not be affected
	// by a subsequent sweep with a 24-hour threshold (which would not match a
	// just-inserted journal).
	userB := int64(2002)
	amountB := decimal.NewFromInt(200)
	_, err = ps.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  userB,
		CurrencyUID:    curID,
		Amount:         amountB,
		IdempotencyKey: postgrestest.UniqueKey("expire-add-b"),
		Source:         "test",
	})
	require.NoError(t, err)

	// Sweep with a real 24-hour threshold — user B's just-inserted journal
	// should NOT be expired.
	cancelled2, err := ps.ExpirePendingOlderThan(ctx, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 0, cancelled2, "fresh deposit must not be expired with 24h threshold")

	balB, err := ls.GetBalance(ctx, userB, curID, pendingCls.UID)
	require.NoError(t, err)
	assert.True(t, balB.Equal(amountB), "user B pending balance should be intact, got %s", balB)
}

// TestPendingStore_ExpirePendingOlderThan_AlreadySettled verifies that the sweeper
// skips accounts whose pending balance is already zero (already confirmed or cancelled).
func TestPendingStore_ExpirePendingOlderThan_AlreadySettled(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()

	cs := postgres.NewClassificationStore(p)
	ls := postgres.NewLedgerStore(p)
	ts := postgres.NewTemplateStore(p)
	require.NoError(t, presets.InstallPendingBundle(ctx, cs, cs, ts))

	curID := postgrestest.SeedCurrency(t, p, "USDT-SETTLED", "Test USDT")
	ps := postgres.NewPendingStore(p, ls, cs)

	userID := int64(3001)
	amount := decimal.NewFromInt(150)

	_, err := ps.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         amount,
		IdempotencyKey: postgrestest.UniqueKey("settled-add"),
		Source:         "test",
	})
	require.NoError(t, err)

	// Confirm the deposit (clears pending balance).
	_, err = ps.ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         amount,
		IdempotencyKey: postgrestest.UniqueKey("settled-confirm"),
		Source:         "test",
	})
	require.NoError(t, err)

	// Use threshold=-1s (cutoff in the future) so any journal would match on
	// created_at — but the account's pending balance is zero so the sweeper
	// must skip it.
	cancelled, err := ps.ExpirePendingOlderThan(ctx, -1*time.Second)
	require.NoError(t, err)
	assert.Equal(t, 0, cancelled, "already-settled account should not be counted as expired")
}

// TestPendingStore_AccountingEquation verifies that after a full Add → Confirm
// cycle the sum of all debits equals the sum of all credits (double-entry invariant).
func TestPendingStore_AccountingEquation(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()

	cs := postgres.NewClassificationStore(p)
	ls := postgres.NewLedgerStore(p)
	ts := postgres.NewTemplateStore(p)
	require.NoError(t, presets.InstallPendingBundle(ctx, cs, cs, ts))

	curID := postgrestest.SeedCurrency(t, p, "USDT-EQ", "Test USDT")
	ps := postgres.NewPendingStore(p, ls, cs)

	userID := int64(4001)
	amount := decimal.NewFromInt(777)

	_, err := ps.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         amount,
		IdempotencyKey: postgrestest.UniqueKey("eq-add"),
		Source:         "test",
	})
	require.NoError(t, err)

	_, err = ps.ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         amount,
		IdempotencyKey: postgrestest.UniqueKey("eq-confirm"),
		Source:         "test",
	})
	require.NoError(t, err)

	var totalDebits, totalCredits string
	err = p.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN entry_type = 'debit'  THEN amount ELSE 0 END), 0)::text,
			COALESCE(SUM(CASE WHEN entry_type = 'credit' THEN amount ELSE 0 END), 0)::text
		FROM journal_entries
	`).Scan(&totalDebits, &totalCredits)
	require.NoError(t, err)
	assert.Equal(t, totalDebits, totalCredits,
		"accounting equation violated: debits=%s credits=%s", totalDebits, totalCredits)
}

// Pins I-3 for ConfirmPending under concurrency: N racers replaying the SAME
// idempotency key must all resolve to the one posted journal — never
// ErrInsufficientBalance. Before the under-lock idempotency recheck, a retry
// racing its original could pass the pre-check, then find the pending
// balance already consumed by the original and be told "insufficient
// balance" for a confirm that in fact succeeded.
func TestConfirmPending_ConcurrentSameKey_NeverInsufficientBalance(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()

	cs := postgres.NewClassificationStore(p)
	ls := postgres.NewLedgerStore(p)
	ts := postgres.NewTemplateStore(p)
	require.NoError(t, presets.InstallPendingBundle(ctx, cs, cs, ts))

	curUID := postgrestest.SeedCurrency(t, p, "USDT-RACE", "Test USDT")
	store := postgres.NewPendingStore(p, ls, cs)
	holder := int64(4242)

	// Stage exactly the amount that one confirm will fully consume.
	_, err := store.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  holder,
		CurrencyUID:    curUID,
		Amount:         decimal.NewFromInt(70),
		IdempotencyKey: postgrestest.UniqueKey("race-add"),
		Source:         "test",
	})
	require.NoError(t, err)

	key := postgrestest.UniqueKey("race-confirm")
	const racers = 6
	var wg sync.WaitGroup
	errs := make(chan error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.ConfirmPending(ctx, core.ConfirmPendingInput{
				AccountHolder:  holder,
				CurrencyUID:    curUID,
				Amount:         decimal.NewFromInt(70),
				IdempotencyKey: key,
				Source:         "test",
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err, "a same-key replay must resolve idempotently, never fail")
	}
}

// TestPendingStore_ConfirmPending_SignsWhenAttestorConfigured pins the fix
// to a gap the audit found: ConfirmPending posts through
// checkPendingBalanceAndPost, which opens its own transaction around
// LedgerStore.PostJournal even in "pool mode" -- so from PostJournal's own
// point of view it always looked tx-bound, and its tx-mode branch never
// signs regardless of whether an Attestor is configured. The journal came
// back core.AuthStatusUnsignedTxMode forever (journals are append-only), and
// VerifiedBalanceReader treats any dimension with such a contributing
// journal as permanently UNDEFINED -- exactly the same failure mode
// service/onchain.go's postDepositConfirmedJournal was built to avoid for
// its own RunInTx-composed journal, just reached through a different call
// path.
func TestPendingStore_ConfirmPending_SignsWhenAttestorConfigured(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()

	cs := postgres.NewClassificationStore(p)
	ts := postgres.NewTemplateStore(p)
	require.NoError(t, presets.InstallPendingBundle(ctx, cs, cs, ts))

	attestor, _ := newTestAttestor(t, "pending-confirm-key")
	ls := postgres.NewLedgerStore(p).WithAuth(attestor)

	curID := postgrestest.SeedCurrency(t, p, "USDT-CSIGN", "Test USDT")
	ps := postgres.NewPendingStore(p, ls, cs)

	userID := int64(5001)
	amount := decimal.NewFromInt(400)

	_, err := ps.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         amount,
		IdempotencyKey: postgrestest.UniqueKey("confirm-sign-add"),
		Source:         "test",
	})
	require.NoError(t, err)

	j, err := ps.ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         amount,
		IdempotencyKey: postgrestest.UniqueKey("confirm-sign-confirm"),
		Source:         "test",
	})
	require.NoError(t, err)
	require.NotNil(t, j)
	assert.Equal(t, core.AuthStatusSigned, j.AuthStatus,
		"ConfirmPending must sign its journal when an Attestor is configured -- "+
			"got %q, which is what a WithDB-bound LedgerStore.PostJournal call always produces "+
			"regardless of Attestor configuration", j.AuthStatus)

	digest, signature, keyID, _ := fetchAuthColumns(t, p, ctx, j.UID)
	assert.NotEmpty(t, digest, "signed journal must have a stored digest")
	assert.NotEmpty(t, signature, "signed journal must have a stored signature")
	assert.NotEmpty(t, keyID, "signed journal must have a stored key id")
}

// TestPendingStore_CancelPending_SignsWhenAttestorConfigured is
// TestPendingStore_ConfirmPending_SignsWhenAttestorConfigured's sibling for
// the other write path checkPendingBalanceAndPost backs.
func TestPendingStore_CancelPending_SignsWhenAttestorConfigured(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()

	cs := postgres.NewClassificationStore(p)
	ts := postgres.NewTemplateStore(p)
	require.NoError(t, presets.InstallPendingBundle(ctx, cs, cs, ts))

	attestor, _ := newTestAttestor(t, "pending-cancel-key")
	ls := postgres.NewLedgerStore(p).WithAuth(attestor)

	curID := postgrestest.SeedCurrency(t, p, "USDT-XSIGN", "Test USDT")
	ps := postgres.NewPendingStore(p, ls, cs)

	userID := int64(5002)
	amount := decimal.NewFromInt(150)

	_, err := ps.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         amount,
		IdempotencyKey: postgrestest.UniqueKey("cancel-sign-add"),
		Source:         "test",
	})
	require.NoError(t, err)

	j, err := ps.CancelPending(ctx, core.CancelPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curID,
		Amount:         amount,
		Reason:         "test-cancel",
		IdempotencyKey: postgrestest.UniqueKey("cancel-sign-cancel"),
		Source:         "test",
	})
	require.NoError(t, err)
	require.NotNil(t, j)
	assert.Equal(t, core.AuthStatusSigned, j.AuthStatus,
		"CancelPending must sign its journal when an Attestor is configured, got %q", j.AuthStatus)
}

// --- 2026-09-02 deep audit: pending-path concurrency + error surface ------

// TestPendingStore_ConfirmPending_GlobalLockOrder_NoDeadlockWithOrdinaryPost
// pins B-M1: checkPendingBalanceAndPost used to pre-acquire ONLY the user's
// (holder, currency) balance lock, then hand the journal to PostJournal --
// which takes the full set in global (holder, currency_id) order, i.e. the
// system counterpart -holder FIRST because it is negative. The pending path
// therefore held H and asked for -H while every other write path in the
// repository holds -H and asks for H. Two entirely ordinary calls (an
// AddPending and a ConfirmPending on the same user, or a ConfirmPending
// racing a deposit_confirm) were enough to close an ABBA cycle: SQLSTATE
// 40P01 on the deposit money-path, no malicious input required.
//
// The probe transaction below stands in for "any other writer", holding
// bal(-H) -- the lock every canonical path takes first. Post-fix,
// ConfirmPending pre-acquires the whole sorted set, so it blocks on bal(-H)
// holding nothing and the probe's later bal(H) is uncontested.
//
// Falsification: put the pre-lock in checkPendingBalanceAndPost back to the
// single {holder, currencyID} pair. The probe then loses a real 40P01.
func TestPendingStore_ConfirmPending_GlobalLockOrder_NoDeadlockWithOrdinaryPost(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()

	cs := postgres.NewClassificationStore(p)
	ls := postgres.NewLedgerStore(p)
	ts := postgres.NewTemplateStore(p)
	require.NoError(t, presets.InstallPendingBundle(ctx, cs, cs, ts))

	curUID := postgrestest.SeedCurrency(t, p, "USDT-LOCKORD", "Test USDT")
	currencyID := postgrestest.InternalID(t, p, "currencies", curUID)
	ps := postgres.NewPendingStore(p, ls, cs)

	const userID = int64(1201)
	amount := decimal.NewFromInt(100)

	_, err := ps.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curUID,
		Amount:         amount,
		IdempotencyKey: postgrestest.UniqueKey("lockord-add"),
		Source:         "test",
	})
	require.NoError(t, err)

	probeTx, err := p.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = probeTx.Rollback(ctx) }()
	probeQ := postgres.NewQueriesForTest(probeTx)

	require.NoError(t, postgres.AcquireBalanceLocksForTest(ctx, probeQ,
		[]postgres.BalancePair{postgres.NewBalancePair(core.SystemAccountHolder(userID), currencyID)}),
		"probe takes the system counterpart first, exactly as every canonical write path does")

	var wg sync.WaitGroup
	var confirmErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, confirmErr = ps.ConfirmPending(ctx, core.ConfirmPendingInput{
			AccountHolder:  userID,
			CurrencyUID:    curUID,
			Amount:         amount,
			IdempotencyKey: postgrestest.UniqueKey("lockord-confirm"),
			Source:         "test",
		})
	}()

	// Give ConfirmPending time to reach its blocking acquisition. It cannot
	// produce a false PASS: if it had not got that far, the probe's second
	// lock would simply be uncontested -- the same observation asserted below.
	time.Sleep(1500 * time.Millisecond)

	probeErr := postgres.AcquireBalanceLocksForTest(ctx, probeQ,
		[]postgres.BalancePair{postgres.NewBalancePair(userID, currencyID)})

	_ = probeTx.Rollback(ctx)
	wg.Wait()

	require.NoError(t, probeErr,
		"a 40P01 here means ConfirmPending held the user's balance lock while waiting for the system counterpart's -- the reverse of every other path")
	require.NoError(t, confirmErr, "ConfirmPending must serialize behind the probe, not deadlock with it")
}

// TestPendingStore_ConfirmPending_InsufficientBalance pins F-m6: until now the
// shared balance gate in checkPendingBalanceAndPost was covered only through
// CancelPending, so breaking it turned exactly one test red and the confirm
// half -- the one that moves money into the spendable wallet -- had no direct
// pin at all. Also asserts the rejection is side-effect free.
func TestPendingStore_ConfirmPending_InsufficientBalance(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()

	cs := postgres.NewClassificationStore(p)
	ls := postgres.NewLedgerStore(p)
	ts := postgres.NewTemplateStore(p)
	require.NoError(t, presets.InstallPendingBundle(ctx, cs, cs, ts))

	curUID := postgrestest.SeedCurrency(t, p, "USDT-CONFINSUF", "Test USDT")
	ps := postgres.NewPendingStore(p, ls, cs)

	const userID = int64(1202)

	_, err := ps.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curUID,
		Amount:         decimal.NewFromInt(50),
		IdempotencyKey: postgrestest.UniqueKey("confinsuf-add"),
		Source:         "test",
	})
	require.NoError(t, err)

	pendingCls, err := cs.GetByCode(ctx, "pending")
	require.NoError(t, err)
	walletCls, err := cs.GetByCode(ctx, "main_wallet")
	require.NoError(t, err)

	_, err = ps.ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curUID,
		Amount:         decimal.NewFromInt(100), // > 50
		IdempotencyKey: postgrestest.UniqueKey("confinsuf-confirm"),
		Source:         "test",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInsufficientBalance)

	pendingBal, err := ls.GetBalance(ctx, userID, curUID, pendingCls.UID)
	require.NoError(t, err)
	assert.True(t, pendingBal.Equal(decimal.NewFromInt(50)), "a rejected confirm must not consume pending, got %s", pendingBal)
	walletBal, err := ls.GetBalance(ctx, userID, curUID, walletCls.UID)
	require.NoError(t, err)
	assert.True(t, walletBal.IsZero(), "a rejected confirm must not credit the wallet, got %s", walletBal)
}

// TestPendingStore_ExpirePendingOlderThan_JoinsSentinelErrors pins E-m13: the
// sweeper aggregated its per-account failures with %v, which flattens them to
// text. PendingTimeoutSweeper is the only surface a consumer's worker sees, so
// a %v there means errors.Is / core.IsRetryable are dead for the entire path
// -- a frozen account and a transient DB failure become indistinguishable
// strings, and a retry loop cannot tell which it got.
//
// The failure injected here is the ordinary one: an account frozen between
// the deposit and the sweep. The sweeper must still report it, and the
// sentinel must still be reachable through errors.Is.
//
// Falsification: change the errors.Join back to %v.
func TestPendingStore_ExpirePendingOlderThan_JoinsSentinelErrors(t *testing.T) {
	p := postgrestest.SetupDB(t)
	ctx := context.Background()

	cs := postgres.NewClassificationStore(p)
	ls := postgres.NewLedgerStore(p)
	ts := postgres.NewTemplateStore(p)
	require.NoError(t, presets.InstallPendingBundle(ctx, cs, cs, ts))

	curUID := postgrestest.SeedCurrency(t, p, "USDT-EXPJOIN", "Test USDT")
	ps := postgres.NewPendingStore(p, ls, cs)
	policies := postgres.NewAccountPolicyStore(p)

	const userID = int64(1203)

	_, err := ps.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  userID,
		CurrencyUID:    curUID,
		Amount:         decimal.NewFromInt(30),
		IdempotencyKey: postgrestest.UniqueKey("expjoin-add"),
		Source:         "test",
	})
	require.NoError(t, err)

	_, err = policies.SetPolicy(ctx, core.AccountPolicyInput{
		AccountHolder: userID,
		Status:        core.AccountPolicyStatusFrozen,
		Note:          "compliance hold arriving after the deposit",
	})
	require.NoError(t, err)

	// Negative threshold => cutoff in the future => the journal above is stale.
	cancelled, err := ps.ExpirePendingOlderThan(ctx, -time.Second)
	require.Error(t, err, "the sweeper must report the account it could not expire")
	assert.Zero(t, cancelled)
	assert.ErrorIs(t, err, core.ErrAccountFrozen,
		"aggregating sub-errors with %%v breaks errors.Is for the sweeper's only return surface")
}
