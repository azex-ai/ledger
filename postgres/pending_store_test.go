package postgres_test

import (
	"context"
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
		CurrencyID:     curID,
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
	pendingBal, err := ls.GetBalance(ctx, userID, curID, pendingCls.ID)
	require.NoError(t, err)
	assert.True(t, pendingBal.Equal(amount), "pending balance should equal added amount, got %s", pendingBal)

	// Verify suspense classification balance for system counterpart.
	suspenseCls, err := cs.GetByCode(ctx, "suspense")
	require.NoError(t, err)
	systemHolder := core.SystemAccountHolder(userID)
	suspenseBal, err := ls.GetBalance(ctx, systemHolder, curID, suspenseCls.ID)
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
		CurrencyID:     curID,
		Amount:         amount,
		IdempotencyKey: key,
		Source:         "test",
	})
	require.NoError(t, err)

	j2, err := ps.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  userID,
		CurrencyID:     curID,
		Amount:         amount,
		IdempotencyKey: key,
		Source:         "test",
	})
	require.NoError(t, err)

	assert.Equal(t, j1.ID, j2.ID, "idempotent calls should return the same journal ID")

	// Balance should only reflect one addition.
	pendingCls, err := cs.GetByCode(ctx, "pending")
	require.NoError(t, err)
	pendingBal, err := ls.GetBalance(ctx, userID, curID, pendingCls.ID)
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
		CurrencyID:     curID,
		Amount:         addAmount,
		IdempotencyKey: postgrestest.UniqueKey("confirm-add"),
		Source:         "test",
	})
	require.NoError(t, err)

	// Step 2: Confirm with a smaller amount (partial)
	j, err := ps.ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder:  userID,
		CurrencyID:     curID,
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

	pendingBal, err := ls.GetBalance(ctx, userID, curID, pendingCls.ID)
	require.NoError(t, err)
	assert.True(t, pendingBal.Equal(remaining), "pending should be %s after partial confirm, got %s", remaining, pendingBal)

	walletBal, err := ls.GetBalance(ctx, userID, curID, mainWalletCls.ID)
	require.NoError(t, err)
	assert.True(t, walletBal.Equal(confirmAmount), "main_wallet should equal confirmed amount, got %s", walletBal)

	suspenseBal, err := ls.GetBalance(ctx, systemHolder, curID, suspenseCls.ID)
	require.NoError(t, err)
	assert.True(t, suspenseBal.Equal(remaining), "suspense should be %s after partial confirm, got %s", remaining, suspenseBal)

	custodialBal, err := ls.GetBalance(ctx, systemHolder, curID, custodialCls.ID)
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
		CurrencyID:     curID,
		Amount:         amount,
		IdempotencyKey: postgrestest.UniqueKey("confidem-add"),
		Source:         "test",
	})
	require.NoError(t, err)

	key := postgrestest.UniqueKey("confidem-confirm")
	j1, err := ps.ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder:  userID,
		CurrencyID:     curID,
		Amount:         amount,
		IdempotencyKey: key,
		Source:         "test",
	})
	require.NoError(t, err)

	j2, err := ps.ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder:  userID,
		CurrencyID:     curID,
		Amount:         amount,
		IdempotencyKey: key,
		Source:         "test",
	})
	require.NoError(t, err)
	assert.Equal(t, j1.ID, j2.ID)

	// Balance should only reflect one confirmation.
	mainWalletCls, err := cs.GetByCode(ctx, "main_wallet")
	require.NoError(t, err)
	walletBal, err := ls.GetBalance(ctx, userID, curID, mainWalletCls.ID)
	require.NoError(t, err)
	assert.True(t, walletBal.Equal(amount), "wallet should reflect exactly one confirmation")
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
		CurrencyID:     curID,
		Amount:         amount,
		IdempotencyKey: postgrestest.UniqueKey("cancel-add"),
		Source:         "test",
	})
	require.NoError(t, err)

	cancelJournal, err := ps.CancelPending(ctx, core.CancelPendingInput{
		AccountHolder:  userID,
		CurrencyID:     curID,
		Amount:         amount,
		Reason:         "test_cancellation",
		IdempotencyKey: postgrestest.UniqueKey("cancel-cancel"),
		Source:         "test",
	})
	require.NoError(t, err)
	require.NotNil(t, cancelJournal)

	// Cancel journal must be a different journal from the original.
	assert.NotEqual(t, addJournal.ID, cancelJournal.ID, "cancel must create a new journal")

	// Balances must be zero after cancel.
	pendingCls, err := cs.GetByCode(ctx, "pending")
	require.NoError(t, err)
	suspenseCls, err := cs.GetByCode(ctx, "suspense")
	require.NoError(t, err)

	systemHolder := core.SystemAccountHolder(userID)

	pendingBal, err := ls.GetBalance(ctx, userID, curID, pendingCls.ID)
	require.NoError(t, err)
	assert.True(t, pendingBal.IsZero(), "pending balance must be zero after cancel, got %s", pendingBal)

	suspenseBal, err := ls.GetBalance(ctx, systemHolder, curID, suspenseCls.ID)
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
		CurrencyID:     curID,
		Amount:         amount,
		IdempotencyKey: postgrestest.UniqueKey("nomut-add"),
		Source:         "test",
	})
	require.NoError(t, err)

	cancelJ, err := ps.CancelPending(ctx, core.CancelPendingInput{
		AccountHolder:  userID,
		CurrencyID:     curID,
		Amount:         amount,
		Reason:         "expired",
		IdempotencyKey: postgrestest.UniqueKey("nomut-cancel"),
		Source:         "test",
	})
	require.NoError(t, err)

	// The cancel must write a NEW journal, never touch the original.
	assert.NotEqual(t, addJ.ID, cancelJ.ID, "cancel journal ID must differ from add journal ID")
	// The original journal must NOT reference the cancel journal.
	assert.Equal(t, int64(0), addJ.ReversalOf, "add journal must not have reversal_of set")
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
		CurrencyID:     curID,
		Amount:         amount,
		IdempotencyKey: postgrestest.UniqueKey("insuf-add"),
		Source:         "test",
	})
	require.NoError(t, err)

	// Attempt to cancel more than pending.
	_, err = ps.CancelPending(ctx, core.CancelPendingInput{
		AccountHolder:  userID,
		CurrencyID:     curID,
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
		CurrencyID:     curID,
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

	balA, err := ls.GetBalance(ctx, userA, curID, pendingCls.ID)
	require.NoError(t, err)
	assert.True(t, balA.IsZero(), "user A pending balance must be zero after expiry, got %s", balA)

	// Add a "fresh" deposit for user B AFTER the sweep — should not be affected
	// by a subsequent sweep with a 24-hour threshold (which would not match a
	// just-inserted journal).
	userB := int64(2002)
	amountB := decimal.NewFromInt(200)
	_, err = ps.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  userB,
		CurrencyID:     curID,
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

	balB, err := ls.GetBalance(ctx, userB, curID, pendingCls.ID)
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
		CurrencyID:     curID,
		Amount:         amount,
		IdempotencyKey: postgrestest.UniqueKey("settled-add"),
		Source:         "test",
	})
	require.NoError(t, err)

	// Confirm the deposit (clears pending balance).
	_, err = ps.ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder:  userID,
		CurrencyID:     curID,
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
		CurrencyID:     curID,
		Amount:         amount,
		IdempotencyKey: postgrestest.UniqueKey("eq-add"),
		Source:         "test",
	})
	require.NoError(t, err)

	_, err = ps.ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder:  userID,
		CurrencyID:     curID,
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
