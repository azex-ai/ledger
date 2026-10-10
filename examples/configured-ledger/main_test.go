package main

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
)

func fixture(t *testing.T) (*ledger.Service, *pgxpool.Pool, map[string]string) {
	t.Helper()
	admin := postgrestest.SetupDB(t)
	cfg := admin.Config().Copy()
	cfg.ConnConfig.RuntimeParams["role"] = "ledger_app"
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	svc, err := ledger.New(pool)
	require.NoError(t, err)
	require.NoError(t, svc.AssertRuntimeRole(t.Context()))
	units, err := setup(t.Context(), svc)
	require.NoError(t, err)
	return svc, admin, units
}

func counts(t *testing.T, admin *pgxpool.Pool, journals, reservations int) {
	t.Helper()
	var gotJournals, gotReservations int
	require.NoError(t, admin.QueryRow(t.Context(),
		"SELECT (SELECT count(*) FROM journals), (SELECT count(*) FROM reservations)",
	).Scan(&gotJournals, &gotReservations))
	require.Equal(t, journals, gotJournals)
	require.Equal(t, reservations, gotReservations)
}

func TestConfiguredScenario_EconomicsAndSameEventReplay(t *testing.T) {
	svc, admin, units := fixture(t)
	ctx := t.Context()
	original, err := scenario(ctx, svc, units)
	require.NoError(t, err)
	require.Len(t, original, 7)
	require.NoError(t, checkJournalBalances(ctx, svc, original))
	require.NoError(t, checkBalances(ctx, svc, units, finalBalances()))
	counts(t, admin, 7, 2)

	// Verify role/normal-side semantics, including fee expense being excluded
	// from spendable balance. A +25 memo must not undo a -25 wallet charge.
	for _, want := range []struct {
		code string
		side core.NormalSide
		role core.BalanceRole
	}{
		{"main_wallet", core.NormalSideDebit, core.BalanceRoleAvailable},
		{"fee_expense", core.NormalSideDebit, core.BalanceRoleMemo},
		{"custodial", core.NormalSideCredit, core.BalanceRoleNone},
		{"fees", core.NormalSideCredit, core.BalanceRoleNone},
		{"settlement", core.NormalSideCredit, core.BalanceRoleNone},
		{"points_issued", core.NormalSideCredit, core.BalanceRoleNone},
	} {
		class, err := svc.Classifications().GetByCode(ctx, want.code)
		require.NoError(t, err)
		require.Equal(t, want.side, class.NormalSide, want.code)
		require.Equal(t, want.role, class.BalanceRole, want.code)
	}
	for code, quantity := range map[string]string{"USDC": "74", "CREDITS": "980", "POINTS": "100", "ROSE": "2"} {
		balance, err := svc.BalanceReader().GetBalanceBreakdown(ctx, userID, units[code])
		require.NoError(t, err)
		require.True(t, balance.Available.Equal(decimal.RequireFromString(quantity)), "%s: %+v", code, balance)
		held, err := svc.Reserver().HeldAmount(ctx, userID, units[code])
		require.NoError(t, err)
		require.True(t, held.IsZero(), "%s: held %s", code, held)
	}

	// A process restart installs matching configuration and re-delivers the
	// exact same event IDs. It reuses journals and completed holds.
	reloaded, err := setup(ctx, svc)
	require.NoError(t, err)
	require.Equal(t, units, reloaded)
	replayed, err := scenario(ctx, svc, reloaded)
	require.NoError(t, err)
	require.Equal(t, original, replayed)
	counts(t, admin, 7, 2)
	require.NoError(t, checkBalances(ctx, svc, units, finalBalances()))

	changed := giftRate()
	changed.Rate = decimal.RequireFromString("0.2")
	_, err = exchangeGift(ctx, svc, units, changed)
	require.ErrorIs(t, err, core.ErrConflict)
	counts(t, admin, 7, 2)
	require.NoError(t, checkBalances(ctx, svc, units, finalBalances()))
}

func TestAcceptanceRejectsReversedFeeEvenThoughBalanced(t *testing.T) {
	svc, admin, units := fixture(t)
	ctx := t.Context()
	_, err := svc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", params(units["USDC"], "deposit", "100"))
	require.NoError(t, err)
	stored, err := svc.Templates().GetTemplate(ctx, "fee_charge")
	require.NoError(t, err)
	// Mutate only a test candidate. Never replace the installed preset.
	candidate := *stored
	candidate.Lines = append([]core.EntryTemplateLine(nil), stored.Lines...)
	for i := range candidate.Lines {
		if candidate.Lines[i].EntryType == core.EntryTypeDebit {
			candidate.Lines[i].EntryType = core.EntryTypeCredit
		} else {
			candidate.Lines[i].EntryType = core.EntryTypeDebit
		}
	}
	input, err := candidate.Render(params(units["USDC"], "bad-fee", "25"))
	require.NoError(t, err) // Render's structural balance check accepts this.
	require.NoError(t, input.Validate())
	system := core.SystemAccountHolder(userID)
	err = svc.RunInTx(ctx, func(tx *ledger.Service) error {
		journal, err := tx.JournalWriter().PostJournal(ctx, *input)
		if err != nil {
			return err
		}
		require.NoError(t, checkJournalBalances(ctx, tx, []string{journal.UID}))
		require.NoError(t, checkBalances(ctx, tx, units, []balanceExpectation{
			{userID, "USDC", "main_wallet", "125"},
			{userID, "USDC", "fee_expense", "-25"},
			{system, "USDC", "custodial", "125"},
			{system, "USDC", "fees", "-25"},
		}))
		// This independent oracle requires a fee charge, not a refund.
		return checkBalances(ctx, tx, units, []balanceExpectation{
			{userID, "USDC", "main_wallet", "75"},
			{userID, "USDC", "fee_expense", "25"},
			{system, "USDC", "custodial", "75"},
			{system, "USDC", "fees", "25"},
		})
	})
	require.ErrorIs(t, err, errEconomics)
	counts(t, admin, 1, 0)
	require.NoError(t, checkBalances(ctx, svc, units, []balanceExpectation{
		{userID, "USDC", "main_wallet", "100"},
		{userID, "USDC", "fee_expense", "0"},
		{system, "USDC", "custodial", "100"},
		{system, "USDC", "fees", "0"},
	}))
}

func TestAcceptanceRejectsWrongGiftRateEvenThoughBothCurrenciesBalance(t *testing.T) {
	svc, admin, units := fixture(t)
	ctx := t.Context()
	// Test-only opening inventory isolates the gift purchase from other events.
	_, err := svc.JournalWriter().ExecuteTemplate(ctx, "fx_buy", params(units["CREDITS"], "opening-credits", "1000"))
	require.NoError(t, err)
	wrong := giftRate()
	wrong.Rate = decimal.RequireFromString("0.5") // Valid math, wrong product price.
	system := core.SystemAccountHolder(userID)
	err = svc.RunInTx(ctx, func(tx *ledger.Service) error {
		gift, err := exchangeGift(ctx, tx, units, wrong)
		if err != nil {
			return err
		}
		require.True(t, gift.Quote.TargetAmount.Equal(decimal.NewFromInt(10)))
		require.NoError(t, checkJournalBalances(ctx, tx, []string{gift.SellJournalUID, gift.BuyJournalUID}))
		require.NoError(t, checkBalances(ctx, tx, units, []balanceExpectation{
			{userID, "CREDITS", "main_wallet", "980"},
			{system, "CREDITS", "settlement", "980"},
			{userID, "ROSE", "main_wallet", "10"},
			{system, "ROSE", "settlement", "10"},
		}))
		// Business requirement: 20 credits buy TWO roses. Do not derive this
		// oracle from wrong.Rate or gift.Quote: that would certify the defect.
		return checkBalances(ctx, tx, units, []balanceExpectation{
			{userID, "CREDITS", "main_wallet", "980"},
			{system, "CREDITS", "settlement", "980"},
			{userID, "ROSE", "main_wallet", "2"},
			{system, "ROSE", "settlement", "2"},
		})
	})
	require.ErrorIs(t, err, errEconomics)
	counts(t, admin, 1, 0)
	require.NoError(t, checkBalances(ctx, svc, units, []balanceExpectation{
		{userID, "CREDITS", "main_wallet", "1000"},
		{system, "CREDITS", "settlement", "1000"},
		{userID, "ROSE", "main_wallet", "0"},
		{system, "ROSE", "settlement", "0"},
	}))
	held, err := svc.Reserver().HeldAmount(ctx, userID, units["CREDITS"])
	require.NoError(t, err)
	require.True(t, held.IsZero())
}
