package postgres_test

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/presets"
)

func TestInstallDefaultTemplatePresets(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	tmplStore := postgres.NewTemplateStore(pool)
	ledgerStore := postgres.NewLedgerStore(pool)

	require.NoError(t, presets.InstallDefaultTemplatePresets(ctx, classStore, classStore, tmplStore))
	require.NoError(t, presets.InstallDefaultTemplatePresets(ctx, classStore, classStore, tmplStore))

	for _, classificationPreset := range presets.DefaultTemplateClassifications {
		classification, err := classStore.GetByCode(ctx, classificationPreset.Code)
		require.NoError(t, err)
		assert.Equal(t, classificationPreset.Code, classification.Code)
	}

	for _, journalTypePreset := range presets.DefaultTemplateJournalTypes {
		journalType, err := classStore.GetJournalTypeByCode(ctx, journalTypePreset.Code)
		require.NoError(t, err)
		assert.Equal(t, journalTypePreset.Code, journalType.Code)
	}

	template, err := tmplStore.GetTemplate(ctx, "deposit_confirm")
	require.NoError(t, err)
	assert.Len(t, template.Lines, 2)

	withdrawFeeTemplate, err := tmplStore.GetTemplate(ctx, "withdraw_fee")
	require.NoError(t, err)
	assert.Len(t, withdrawFeeTemplate.Lines, 4)

	stagedDepositTemplate, err := tmplStore.GetTemplate(ctx, "deposit_confirm_pending")
	require.NoError(t, err)
	assert.Len(t, stagedDepositTemplate.Lines, 4)

	curID := seedCurrency(t, pool, "USDT", "Tether USD")
	userID := int64(42)

	journal, err := ledgerStore.ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
		HolderID:       userID,
		CurrencyID:     curID,
		IdempotencyKey: uniqueKey("preset-deposit"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(500)},
		Source:         "test",
	})
	require.NoError(t, err)
	assert.True(t, journal.TotalDebit.Equal(decimal.NewFromInt(500)))

	_, err = ledgerStore.ExecuteTemplate(ctx, "lock_funds", core.TemplateParams{
		HolderID:       userID,
		CurrencyID:     curID,
		IdempotencyKey: uniqueKey("preset-lock-release"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(40)},
		Source:         "test",
	})
	require.NoError(t, err)

	_, err = ledgerStore.ExecuteTemplate(ctx, "unlock_funds", core.TemplateParams{
		HolderID:       userID,
		CurrencyID:     curID,
		IdempotencyKey: uniqueKey("preset-unlock"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(40)},
		Source:         "test",
	})
	require.NoError(t, err)

	_, err = ledgerStore.ExecuteTemplate(ctx, "lock_funds", core.TemplateParams{
		HolderID:       userID,
		CurrencyID:     curID,
		IdempotencyKey: uniqueKey("preset-lock-withdraw"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(105)},
		Source:         "test",
	})
	require.NoError(t, err)

	_, err = ledgerStore.ExecuteTemplate(ctx, "withdraw_fee", core.TemplateParams{
		HolderID:       userID,
		CurrencyID:     curID,
		IdempotencyKey: uniqueKey("preset-withdraw-fee"),
		Amounts:        map[string]decimal.Decimal{"fee": decimal.NewFromInt(5)},
		Source:         "test",
	})
	require.NoError(t, err)

	_, err = ledgerStore.ExecuteTemplate(ctx, "withdraw_confirm", core.TemplateParams{
		HolderID:       userID,
		CurrencyID:     curID,
		IdempotencyKey: uniqueKey("preset-withdraw-confirm"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(100)},
		Source:         "test",
	})
	require.NoError(t, err)

	mainWallet, err := classStore.GetByCode(ctx, "main_wallet")
	require.NoError(t, err)
	locked, err := classStore.GetByCode(ctx, "locked")
	require.NoError(t, err)
	feeExpense, err := classStore.GetByCode(ctx, "fee_expense")
	require.NoError(t, err)
	custodial, err := classStore.GetByCode(ctx, "custodial")
	require.NoError(t, err)
	feeRevenue, err := classStore.GetByCode(ctx, "fee_revenue")
	require.NoError(t, err)
	pending, err := classStore.GetByCode(ctx, "pending")
	require.NoError(t, err)
	suspense, err := classStore.GetByCode(ctx, "suspense")
	require.NoError(t, err)

	walletBal, err := ledgerStore.GetBalance(ctx, userID, curID, mainWallet.ID)
	require.NoError(t, err)
	assert.True(t, walletBal.Equal(decimal.NewFromInt(395)))

	lockedBal, err := ledgerStore.GetBalance(ctx, userID, curID, locked.ID)
	require.NoError(t, err)
	assert.True(t, lockedBal.IsZero())

	feeExpenseBal, err := ledgerStore.GetBalance(ctx, userID, curID, feeExpense.ID)
	require.NoError(t, err)
	assert.True(t, feeExpenseBal.Equal(decimal.NewFromInt(5)))

	custodialBal, err := ledgerStore.GetBalance(ctx, -userID, curID, custodial.ID)
	require.NoError(t, err)
	assert.True(t, custodialBal.Equal(decimal.NewFromInt(395)))

	feeRevenueBal, err := ledgerStore.GetBalance(ctx, -userID, curID, feeRevenue.ID)
	require.NoError(t, err)
	assert.True(t, feeRevenueBal.Equal(decimal.NewFromInt(5)))

	stagedUserID := int64(99)

	_, err = ledgerStore.ExecuteTemplate(ctx, "deposit_pending", core.TemplateParams{
		HolderID:       stagedUserID,
		CurrencyID:     curID,
		IdempotencyKey: uniqueKey("preset-staged-deposit-pending"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(100)},
		Source:         "test",
	})
	require.NoError(t, err)

	_, err = ledgerStore.ExecuteTemplate(ctx, "deposit_confirm_pending", core.TemplateParams{
		HolderID:       stagedUserID,
		CurrencyID:     curID,
		IdempotencyKey: uniqueKey("preset-staged-deposit-confirm"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(95)},
		Source:         "test",
	})
	require.NoError(t, err)

	stagedWalletBal, err := ledgerStore.GetBalance(ctx, stagedUserID, curID, mainWallet.ID)
	require.NoError(t, err)
	assert.True(t, stagedWalletBal.Equal(decimal.NewFromInt(95)))

	stagedPendingBal, err := ledgerStore.GetBalance(ctx, stagedUserID, curID, pending.ID)
	require.NoError(t, err)
	assert.True(t, stagedPendingBal.Equal(decimal.NewFromInt(5)))

	stagedSuspenseBal, err := ledgerStore.GetBalance(ctx, -stagedUserID, curID, suspense.ID)
	require.NoError(t, err)
	assert.True(t, stagedSuspenseBal.Equal(decimal.NewFromInt(5)))

	stagedCustodialBal, err := ledgerStore.GetBalance(ctx, -stagedUserID, curID, custodial.ID)
	require.NoError(t, err)
	assert.True(t, stagedCustodialBal.Equal(decimal.NewFromInt(95)))

	_, err = ledgerStore.ExecuteTemplate(ctx, "deposit_release_pending", core.TemplateParams{
		HolderID:       stagedUserID,
		CurrencyID:     curID,
		IdempotencyKey: uniqueKey("preset-staged-deposit-release"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(5)},
		Source:         "test",
	})
	require.NoError(t, err)

	stagedPendingBal, err = ledgerStore.GetBalance(ctx, stagedUserID, curID, pending.ID)
	require.NoError(t, err)
	assert.True(t, stagedPendingBal.IsZero())

	stagedSuspenseBal, err = ledgerStore.GetBalance(ctx, -stagedUserID, curID, suspense.ID)
	require.NoError(t, err)
	assert.True(t, stagedSuspenseBal.IsZero())
}

func TestExecuteDepositTolerancePlan(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	tmplStore := postgres.NewTemplateStore(pool)
	ledgerStore := postgres.NewLedgerStore(pool)

	require.NoError(t, presets.InstallDefaultTemplatePresets(ctx, classStore, classStore, tmplStore))

	curID := seedCurrency(t, pool, "USDT", "Tether USD")
	mainWallet, err := classStore.GetByCode(ctx, "main_wallet")
	require.NoError(t, err)
	pending, err := classStore.GetByCode(ctx, "pending")
	require.NoError(t, err)
	suspense, err := classStore.GetByCode(ctx, "suspense")
	require.NoError(t, err)
	custodial, err := classStore.GetByCode(ctx, "custodial")
	require.NoError(t, err)

	shortUserID := int64(501)
	_, err = ledgerStore.ExecuteTemplate(ctx, "deposit_pending", core.TemplateParams{
		HolderID:       shortUserID,
		CurrencyID:     curID,
		IdempotencyKey: uniqueKey("tolerance-short-pending"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(100)},
		Source:         "test",
	})
	require.NoError(t, err)

	shortPlan, err := presets.BuildDepositTolerancePlan(
		decimal.NewFromInt(100),
		decimal.NewFromInt(98),
		presets.DepositToleranceConfig{Amount: decimal.NewFromInt(5)},
	)
	require.NoError(t, err)
	_, err = presets.ExecuteDepositTolerancePlan(ctx, ledgerStore, core.TemplateParams{
		HolderID:       shortUserID,
		CurrencyID:     curID,
		IdempotencyKey: uniqueKey("tolerance-short"),
		Source:         "test",
	}, shortPlan)
	require.NoError(t, err)

	shortWalletBal, err := ledgerStore.GetBalance(ctx, shortUserID, curID, mainWallet.ID)
	require.NoError(t, err)
	assert.True(t, shortWalletBal.Equal(decimal.NewFromInt(98)))

	shortPendingBal, err := ledgerStore.GetBalance(ctx, shortUserID, curID, pending.ID)
	require.NoError(t, err)
	assert.True(t, shortPendingBal.IsZero())

	shortSuspenseBal, err := ledgerStore.GetBalance(ctx, -shortUserID, curID, suspense.ID)
	require.NoError(t, err)
	assert.True(t, shortSuspenseBal.IsZero())

	shortCustodialBal, err := ledgerStore.GetBalance(ctx, -shortUserID, curID, custodial.ID)
	require.NoError(t, err)
	assert.True(t, shortCustodialBal.Equal(decimal.NewFromInt(98)))

	overUserID := int64(502)
	_, err = ledgerStore.ExecuteTemplate(ctx, "deposit_pending", core.TemplateParams{
		HolderID:       overUserID,
		CurrencyID:     curID,
		IdempotencyKey: uniqueKey("tolerance-over-pending"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(100)},
		Source:         "test",
	})
	require.NoError(t, err)

	overPlan, err := presets.BuildDepositTolerancePlan(
		decimal.NewFromInt(100),
		decimal.NewFromInt(110),
		presets.DepositToleranceConfig{Amount: decimal.NewFromInt(5)},
	)
	require.NoError(t, err)
	require.True(t, overPlan.RequiresManualReview)

	_, err = presets.ExecuteDepositTolerancePlan(ctx, ledgerStore, core.TemplateParams{
		HolderID:       overUserID,
		CurrencyID:     curID,
		IdempotencyKey: uniqueKey("tolerance-over"),
		Source:         "test",
	}, overPlan)
	require.NoError(t, err)

	overWalletBal, err := ledgerStore.GetBalance(ctx, overUserID, curID, mainWallet.ID)
	require.NoError(t, err)
	assert.True(t, overWalletBal.Equal(decimal.NewFromInt(100)))

	overPendingBal, err := ledgerStore.GetBalance(ctx, overUserID, curID, pending.ID)
	require.NoError(t, err)
	assert.True(t, overPendingBal.IsZero())

	overSuspenseBal, err := ledgerStore.GetBalance(ctx, -overUserID, curID, suspense.ID)
	require.NoError(t, err)
	assert.True(t, overSuspenseBal.Equal(decimal.NewFromInt(10)))

	overCustodialBal, err := ledgerStore.GetBalance(ctx, -overUserID, curID, custodial.ID)
	require.NoError(t, err)
	assert.True(t, overCustodialBal.Equal(decimal.NewFromInt(110)))

	_, err = ledgerStore.ExecuteTemplate(ctx, "deposit_resolve_overage", core.TemplateParams{
		HolderID:       overUserID,
		CurrencyID:     curID,
		IdempotencyKey: uniqueKey("tolerance-over-resolve"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(10)},
		Source:         "test",
	})
	require.NoError(t, err)

	overWalletBal, err = ledgerStore.GetBalance(ctx, overUserID, curID, mainWallet.ID)
	require.NoError(t, err)
	assert.True(t, overWalletBal.Equal(decimal.NewFromInt(110)))

	overSuspenseBal, err = ledgerStore.GetBalance(ctx, -overUserID, curID, suspense.ID)
	require.NoError(t, err)
	assert.True(t, overSuspenseBal.IsZero())
}
