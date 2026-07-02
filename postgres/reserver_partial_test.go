package postgres_test

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// seedFundedReservation funds holder 21 with 100 USDT and reserves reserveAmt
// of it, returning the stores and reservation.
func seedFundedReservation(t *testing.T, reserveAmt decimal.Decimal) (*postgres.ReserverStore, *postgres.LedgerStore, context.Context, *core.Reservation, int64) {
	t.Helper()
	pool := postgrestest.SetupDB(t)
	ledgerStore := postgres.NewLedgerStore(pool)
	reserver := postgres.NewReserverStore(pool, ledgerStore)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	jtID := postgrestest.SeedJournalType(t, pool, "deposit", "Deposit")
	clsWallet := postgrestest.SeedClassification(t, pool, "main_wallet", "Main Wallet", "credit", false)
	clsCustodial := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "debit", true)

	_, err := ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jtID,
		IdempotencyKey: postgrestest.UniqueKey("psettle-fund"),
		Entries: []core.EntryInput{
			{AccountHolder: 21, CurrencyID: curID, ClassificationID: clsWallet, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -21, CurrencyID: curID, ClassificationID: clsCustodial, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
		},
	})
	require.NoError(t, err)

	res, err := reserver.Reserve(ctx, core.ReserveInput{
		AccountHolder:  21,
		CurrencyID:     curID,
		Amount:         reserveAmt,
		IdempotencyKey: postgrestest.UniqueKey("psettle-res"),
	})
	require.NoError(t, err)
	return reserver, ledgerStore, ctx, res, curID
}

func TestReserverStore_SettlePartial_AccumulatesAndFinalizes(t *testing.T) {
	reserver, _, ctx, res, curID := seedFundedReservation(t, decimal.NewFromInt(60))

	// Two partial settlements accumulate.
	require.NoError(t, reserver.SettlePartial(ctx, res.ID, decimal.RequireFromString("12.5")))
	require.NoError(t, reserver.SettlePartial(ctx, res.ID, decimal.RequireFromString("20")))

	// Over-cumulative (32.5 + 30 > 60) is rejected and changes nothing.
	err := reserver.SettlePartial(ctx, res.ID, decimal.NewFromInt(30))
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)

	// One-shot Settle on a settling reservation is rejected — it would
	// overwrite the accumulated settled_amount.
	err = reserver.Settle(ctx, res.ID, decimal.NewFromInt(40))
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidTransition)

	// Finalize: settling → settled; the hold is fully released.
	require.NoError(t, reserver.FinalizeSettlement(ctx, res.ID))
	held, err := reserver.HeldAmount(ctx, 21, curID)
	require.NoError(t, err)
	assert.True(t, held.IsZero(), "hold must be zero after finalize, got %s", held)

	// Terminal: no further partial settlement or finalize.
	err = reserver.SettlePartial(ctx, res.ID, decimal.NewFromInt(1))
	assert.ErrorIs(t, err, core.ErrInvalidTransition)
	err = reserver.FinalizeSettlement(ctx, res.ID)
	assert.ErrorIs(t, err, core.ErrInvalidTransition)
}

// Pins the hold-accounting fix: a settling reservation still holds its
// unsettled remainder. Without the CASE in SumActiveReservations, the first
// SettlePartial would drop the whole hold from availability and let a
// concurrent Reserve over-commit the balance (the I-4/I-11 TOCTOU class).
func TestReserverStore_SettlePartial_RemainderStillHeld(t *testing.T) {
	reserver, _, ctx, res, curID := seedFundedReservation(t, decimal.NewFromInt(60))

	// Balance 100, reserved 60 → available 40.
	require.NoError(t, reserver.SettlePartial(ctx, res.ID, decimal.NewFromInt(15)))

	// Hold = 60 - 15 = 45 (NOT zero, NOT 60).
	held, err := reserver.HeldAmount(ctx, 21, curID)
	require.NoError(t, err)
	assert.True(t, held.Equal(decimal.NewFromInt(45)), "settling remainder must stay held: got %s", held)

	// available = 100 - 45 = 55 → reserving 56 must fail, 55 must succeed.
	_, err = reserver.Reserve(ctx, core.ReserveInput{
		AccountHolder: 21, CurrencyID: curID,
		Amount:         decimal.NewFromInt(56),
		IdempotencyKey: postgrestest.UniqueKey("psettle-over"),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInsufficientBalance)

	_, err = reserver.Reserve(ctx, core.ReserveInput{
		AccountHolder: 21, CurrencyID: curID,
		Amount:         decimal.NewFromInt(55),
		IdempotencyKey: postgrestest.UniqueKey("psettle-fit"),
	})
	require.NoError(t, err)
}

func TestReserverStore_FinalizeSettlement_OnActiveRejected(t *testing.T) {
	reserver, _, ctx, res, _ := seedFundedReservation(t, decimal.NewFromInt(30))

	// Finalizing a reservation that never had a partial settlement is a
	// contract misuse (nothing was settled — Release is the right call).
	err := reserver.FinalizeSettlement(ctx, res.ID)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidTransition)

	// The reservation is untouched: a one-shot Settle still works.
	require.NoError(t, reserver.Settle(ctx, res.ID, decimal.NewFromInt(30)))
}

func TestReserverStore_SettlePartial_NonPositiveRejected(t *testing.T) {
	reserver, _, ctx, res, _ := seedFundedReservation(t, decimal.NewFromInt(30))

	err := reserver.SettlePartial(ctx, res.ID, decimal.Zero)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)

	err = reserver.SettlePartial(ctx, res.ID, decimal.NewFromInt(-5))
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)
}
