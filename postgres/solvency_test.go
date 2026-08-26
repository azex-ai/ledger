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
	"github.com/azex-ai/ledger/presets"
)

// TestSolvencyCheck_WithdrawFee_DoesNotManufactureDeficit pins a Major from
// the 2026-08-25 financial-engineering audit
// (postgres/sql/queries/platform_balances.sql:60-91, GetTotalUserSideBalance;
// financial-correctness.md): SolvencyCheck's Liability figure summed EVERY
// user-side classification, including role-less debit-normal cost accounts
// like fee_expense. fee_expense is booked to the user's own holder id purely
// for per-user fee reporting -- it is not money the platform owes back. Under
// the old query, every dollar of cumulative withdrawal fees showed up as a
// dollar of manufactured insolvency, even though the platform was fully
// solvent (custodial == every reservable user balance).
//
// This replays exactly the flow postgres/presets_install_test.go's
// TestInstallDefaultTemplatePresets already asserts the balances for
// (deposit 500 -> lock 105 -> withdraw_fee 5 -> withdraw_confirm 100, leaving
// main_wallet=395, locked=0, fee_expense=5, custodial=395), so the "before"
// math is independently checkable against that test's own assertions:
//
//	Liability (buggy) = main_wallet(395) + locked(0) + fee_expense(5) = 400
//	Custodial          = 395
//	Margin  (buggy)    = 395 - 400 = -5  =>  Solvent = false
//
// After the fix, Liability excludes fee_expense (BalanceRole == "" / none):
//
//	Liability (fixed)  = main_wallet(395) + locked(0) = 395
//	Margin  (fixed)    = 395 - 395 = 0  =>  Solvent = true
func TestSolvencyCheck_WithdrawFee_DoesNotManufactureDeficit(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	tmplStore := postgres.NewTemplateStore(pool)
	ledgerStore := postgres.NewLedgerStore(pool)
	pbStore := postgres.NewPlatformBalanceStore(pool)

	require.NoError(t, presets.InstallDefaultTemplatePresets(ctx, classStore, classStore, tmplStore))

	curID := postgrestest.SeedCurrency(t, pool, "USDT-SOLV", "Tether USD Solvency")
	userID := int64(9101)

	_, err := ledgerStore.ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
		HolderID:       userID,
		CurrencyUID:    curID,
		IdempotencyKey: postgrestest.UniqueKey("solv-deposit"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(500)},
		Source:         "test",
	})
	require.NoError(t, err)

	_, err = ledgerStore.ExecuteTemplate(ctx, "lock_funds", core.TemplateParams{
		HolderID:       userID,
		CurrencyUID:    curID,
		IdempotencyKey: postgrestest.UniqueKey("solv-lock"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(105)},
		Source:         "test",
	})
	require.NoError(t, err)

	_, err = ledgerStore.ExecuteTemplate(ctx, "withdraw_fee", core.TemplateParams{
		HolderID:       userID,
		CurrencyUID:    curID,
		IdempotencyKey: postgrestest.UniqueKey("solv-fee"),
		Amounts:        map[string]decimal.Decimal{"fee": decimal.NewFromInt(5)},
		Source:         "test",
	})
	require.NoError(t, err)

	_, err = ledgerStore.ExecuteTemplate(ctx, "withdraw_confirm", core.TemplateParams{
		HolderID:       userID,
		CurrencyUID:    curID,
		IdempotencyKey: postgrestest.UniqueKey("solv-confirm"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(100)},
		Source:         "test",
	})
	require.NoError(t, err)

	report, err := pbStore.SolvencyCheck(ctx, curID)
	require.NoError(t, err)

	assert.True(t, report.Custodial.Equal(decimal.NewFromInt(395)), "custodial: got %s", report.Custodial)
	assert.True(t, report.Liability.Equal(decimal.NewFromInt(395)), "liability must exclude fee_expense: got %s", report.Liability)
	assert.True(t, report.Margin.IsZero(), "margin: got %s", report.Margin)
	assert.True(t, report.Solvent, "platform must report solvent -- custodial covers every reservable user balance")

	liability, err := pbStore.GetTotalLiabilityByAsset(ctx, curID)
	require.NoError(t, err)
	assert.True(t, liability.Equal(decimal.NewFromInt(395)), "GetTotalLiabilityByAsset must agree with SolvencyCheck: got %s", liability)
}
