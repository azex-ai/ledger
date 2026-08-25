package ledger_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
)

// TestPresetDirection_MovesMoneyTheRightWay pins what the preset tests could
// not: the balance each template actually produces.
//
// transfer_out, transfer_in and fee_charge shipped with their holder leg
// inverted against main_wallet's declared polarity. A P2P transfer of 100 left
// the sender 100 richer and the receiver 100 in debt, and charging a fee paid
// the payer. Every preset test passed throughout, because both legs of these
// templates draw on the same amount key, which makes "total debits equal total
// credits" true no matter which side each classification lands on. The
// assertion could not fail, so it never did.
//
// This test asserts the outcome instead of the arithmetic identity: post
// through the templates and read the balances back. It needs real Postgres --
// the preset suite runs against an in-memory fake, which is also why the
// extended bundle had never been posted through at all.
func TestPresetDirection_MovesMoneyTheRightWay(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	svc, err := ledger.New(pool)
	require.NoError(t, err)
	require.NoError(t, svc.InstallExtendedPresets(ctx))

	cur, err := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{
		Code: "DIR", Name: "Direction Test Unit", Exponent: 2,
	})
	require.NoError(t, err)
	wallet, err := svc.Classifications().GetByCode(ctx, "main_wallet")
	require.NoError(t, err)

	balance := func(holder int64) decimal.Decimal {
		t.Helper()
		b, err := svc.BalanceReader().GetBalance(ctx, holder, cur.UID, wallet.UID)
		require.NoError(t, err)
		return b
	}
	post := func(template string, holder int64, key string, amount decimal.Decimal) {
		t.Helper()
		_, err := svc.JournalWriter().ExecuteTemplate(ctx, template, core.TemplateParams{
			HolderID:       holder,
			CurrencyUID:    cur.UID,
			IdempotencyKey: postgrestest.UniqueKey(key),
			Amounts:        map[string]decimal.Decimal{"amount": amount},
			Source:         "preset-direction-test",
		})
		require.NoError(t, err)
	}

	const sender, receiver, payer = int64(9301), int64(9302), int64(9303)

	// deposit_confirm is the reference: it has always been right, and it is
	// what establishes that debiting main_wallet is how a holder gains money.
	post("deposit_confirm", sender, "seed-sender", decimal.NewFromInt(500))
	post("deposit_confirm", payer, "seed-payer", decimal.NewFromInt(500))
	require.True(t, balance(sender).Equal(decimal.NewFromInt(500)),
		"deposit_confirm must credit the holder 500, got %s", balance(sender))

	t.Run("a transfer moves money from the sender to the receiver", func(t *testing.T) {
		post("transfer_out", sender, "xfer-out", decimal.NewFromInt(100))
		post("transfer_in", receiver, "xfer-in", decimal.NewFromInt(100))

		require.True(t, balance(sender).Equal(decimal.NewFromInt(400)),
			"the sender must be 100 poorer, got %s -- if this reads 600 the holder leg is inverted again", balance(sender))
		require.True(t, balance(receiver).Equal(decimal.NewFromInt(100)),
			"the receiver must be 100 richer, got %s -- if this reads -100 the holder leg is inverted again", balance(receiver))
	})

	t.Run("a fee takes money from the payer", func(t *testing.T) {
		before := balance(payer)
		post("fee_charge", payer, "fee", decimal.NewFromInt(25))

		require.True(t, balance(payer).Equal(before.Sub(decimal.NewFromInt(25))),
			"charging a 25 fee must leave the payer 25 poorer (%s -> %s), got %s",
			before, before.Sub(decimal.NewFromInt(25)), balance(payer))
	})
}

// TestReserveSettleCharge_ChargesInsideOneTransaction pins the pattern
// examples/billing teaches, because the example is not run by anything.
//
// Settle closes the hold; it writes no entries. The charge is a journal the
// caller posts, and the two belong in one transaction: a crash between them
// frees the hold and bills nobody, which the ledger reports as success because
// from its side nothing failed. The example used to omit the charge entirely
// and printed a final balance of 100 under a line claiming it expected 84.25.
//
// The assertion is the holder's balance, not that the calls returned nil.
// Returning nil is exactly what the broken version did.
func TestReserveSettleCharge_ChargesInsideOneTransaction(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	svc, err := ledger.New(pool)
	require.NoError(t, err)
	require.NoError(t, svc.InstallExtendedPresets(ctx))

	cur, err := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{
		Code: "BIL", Name: "Billing Unit", Exponent: 2,
	})
	require.NoError(t, err)
	wallet, err := svc.Classifications().GetByCode(ctx, "main_wallet")
	require.NoError(t, err)

	const holder = int64(9501)
	balance := func() decimal.Decimal {
		t.Helper()
		b, err := svc.BalanceReader().GetBalance(ctx, holder, cur.UID, wallet.UID)
		require.NoError(t, err)
		return b
	}

	_, err = svc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
		HolderID: holder, CurrencyUID: cur.UID,
		IdempotencyKey: postgrestest.UniqueKey("bill-topup"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.RequireFromString("100.00")},
		Source:         "billing-pattern-test",
	})
	require.NoError(t, err)
	require.True(t, balance().Equal(decimal.RequireFromString("100.00")))

	rsv, err := svc.Reserver().Reserve(ctx, core.ReserveInput{
		AccountHolder: holder, CurrencyUID: cur.UID,
		Amount:         decimal.RequireFromString("20.00"),
		IdempotencyKey: postgrestest.UniqueKey("bill-reserve"),
		ExpiresIn:      time.Hour,
	})
	require.NoError(t, err)

	actual := decimal.RequireFromString("15.75")
	require.NoError(t, svc.RunInTx(ctx, func(tx *ledger.Service) error {
		if err := tx.Reserver().Settle(ctx, core.SettleInput{ReservationUID: rsv.UID, Amount: actual}); err != nil {
			return err
		}
		_, err := tx.JournalWriter().ExecuteTemplate(ctx, "fee_charge", core.TemplateParams{
			HolderID: holder, CurrencyUID: cur.UID,
			IdempotencyKey: postgrestest.UniqueKey("bill-charge"),
			Amounts:        map[string]decimal.Decimal{"amount": actual},
			Source:         "billing-pattern-test",
		})
		return err
	}))

	require.True(t, balance().Equal(decimal.RequireFromString("84.25")),
		"100.00 topped up and 15.75 charged must leave 84.25, got %s -- 100.00 here means the hold was released and nobody was billed", balance())
}
