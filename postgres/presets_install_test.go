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

// TestInstallExtendedPresets_PostsAgainstRealPostgres pins the Extended
// preset bundle (FX, Capital, Settlement, Spread -- installed together by
// presets.InstallExtendedPresets, same as ledger.Service.InstallExtendedPresets)
// against a REAL Postgres store. Before this test, every *_test.go in the
// presets package that exercises these four bundles does so only against
// presets_test.go's in-memory fake stores (newFakeClassificationStore /
// newFakeJournalTypeStore / newFakeTemplateStore), which are strictly more
// permissive than the real postgres.ClassificationStore/TemplateStore (e.g.
// the fake's SetBalanceRole allows switching role back and forth freely;
// the real one only allows a one-time empty->non-empty upgrade). A bundle
// that passes every fake-store test could still fail to install, or install
// with a subtly different amount split, against the real schema and its
// triggers/constraints -- and nothing in the repo would catch it, because
// the only two real-Postgres examples that call
// svc.InstallExtendedPresets(ctx) (examples/credits-topup, examples/billing)
// have zero automated tests (`find examples -name "*_test.go"` is empty).
//
// This does not adjudicate whether the FX/settlement templates' DR/CR
// assignment is business-correct -- presets/fx.go's own doc comment
// disagrees with its own code on that point, and TODO.md §1 already tracks
// it separately ("presets/fx.go 的文档与自己的代码符号相反", deferred to
// W3-sign, out of this task's scope). The assertions below pin the actual
// current behavior of the real store (so a regression in precision
// validation, classification wiring, or template rendering trips this
// test), not an aspirational "correct" sign.
func TestInstallExtendedPresets_PostsAgainstRealPostgres(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	tmplStore := postgres.NewTemplateStore(pool)
	ledgerStore := postgres.NewLedgerStore(pool)

	require.NoError(t, presets.InstallExtendedPresets(ctx, classStore, classStore, tmplStore))
	// Idempotent re-install: existing rows are validated and reused, not
	// duplicated or rejected.
	require.NoError(t, presets.InstallExtendedPresets(ctx, classStore, classStore, tmplStore))

	spread, err := classStore.GetByCode(ctx, "spread")
	require.NoError(t, err)
	assert.Equal(t, core.NormalSideCredit, spread.NormalSide)
	assert.True(t, spread.IsSystem)

	mainWallet, err := classStore.GetByCode(ctx, "main_wallet")
	require.NoError(t, err)
	settlement, err := classStore.GetByCode(ctx, "settlement")
	require.NoError(t, err)
	equity, err := classStore.GetByCode(ctx, "equity")
	require.NoError(t, err)
	custodial, err := classStore.GetByCode(ctx, "custodial")
	require.NoError(t, err)
	fees, err := classStore.GetByCode(ctx, "fees")
	require.NoError(t, err)

	// --- FX: fund a user in currency A, then run the fx_sell / fx_buy legs
	// for real against Postgres -- this is the exact composition FXBundle's
	// doc comment describes as the caller workflow for a CCY-A -> CCY-B swap.
	curA := postgrestest.SeedCurrency(t, pool, postgrestest.UniqueKey("FXA"), "FX Currency A")
	curB := postgrestest.SeedCurrency(t, pool, postgrestest.UniqueKey("FXB"), "FX Currency B")
	fxUser := int64(910001)

	_, err = ledgerStore.ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
		HolderID:       fxUser,
		CurrencyUID:    curA,
		IdempotencyKey: postgrestest.UniqueKey("fx-fund"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(500)},
		Source:         "test",
	})
	require.NoError(t, err)

	_, err = ledgerStore.ExecuteTemplate(ctx, "fx_sell", core.TemplateParams{
		HolderID:       fxUser,
		CurrencyUID:    curA,
		IdempotencyKey: postgrestest.UniqueKey("fx-sell"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(100)},
		Source:         "test",
	})
	require.NoError(t, err, "fx_sell must post against the real store (precision + classification wiring must be intact)")

	walletA, err := ledgerStore.GetBalance(ctx, fxUser, curA, mainWallet.UID)
	require.NoError(t, err)
	assert.True(t, walletA.Equal(decimal.NewFromInt(400)), "user's CCY-A wallet must reflect the fx_sell leg, want 400 got %s", walletA)

	_, err = ledgerStore.ExecuteTemplate(ctx, "fx_buy", core.TemplateParams{
		HolderID:       fxUser,
		CurrencyUID:    curB,
		IdempotencyKey: postgrestest.UniqueKey("fx-buy"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(250)},
		Source:         "test",
	})
	require.NoError(t, err, "fx_buy must post against the real store")

	walletB, err := ledgerStore.GetBalance(ctx, fxUser, curB, mainWallet.UID)
	require.NoError(t, err)
	assert.True(t, walletB.Equal(decimal.NewFromInt(250)), "user's CCY-B wallet must reflect the fx_buy leg, want 250 got %s", walletB)

	settlementB, err := ledgerStore.GetBalance(ctx, -fxUser, curB, settlement.UID)
	require.NoError(t, err)
	assert.True(t, settlementB.Equal(decimal.NewFromInt(250)), "settlement's CCY-B leg must move with the fx_buy posting, want 250 got %s", settlementB)

	// --- Capital: injection then withdrawal, real store, own currency so
	// equity/custodial start clean.
	curC := postgrestest.SeedCurrency(t, pool, postgrestest.UniqueKey("CAP"), "Capital Currency")
	sysHolder := int64(910002)

	_, err = ledgerStore.ExecuteTemplate(ctx, "capital_injection", core.TemplateParams{
		HolderID:       sysHolder,
		CurrencyUID:    curC,
		IdempotencyKey: postgrestest.UniqueKey("capital-inject"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(1000)},
		Source:         "test",
	})
	require.NoError(t, err)

	equityBal, err := ledgerStore.GetBalance(ctx, -sysHolder, curC, equity.UID)
	require.NoError(t, err)
	assert.True(t, equityBal.Equal(decimal.NewFromInt(1000)), "equity after injection, want 1000 got %s", equityBal)

	// The other entry of capital_injection must land on custodial specifically
	// (not, say, settlement or fees) -- a template line pointed at the wrong
	// classification code still renders and posts a perfectly balanced
	// journal (Render only checks debit==credit per currency, not which
	// classification received it), so only a per-classification balance
	// check like this one would catch a wrong-classification-code bug.
	//
	// Inverted pin (2026-09-02 audit A-C1): this line used to demand -1000
	// and even explained itself with "(credit-normal debited)" -- a faithful
	// description of the template that was reversed. Injecting platform
	// capital must RAISE the custody position; it is the only action in the
	// catalogue that can improve the solvency margin, and it was driving it
	// down by twice the injected amount.
	custodialBalCapital, err := ledgerStore.GetBalance(ctx, -sysHolder, curC, custodial.UID)
	require.NoError(t, err)
	assert.True(t, custodialBalCapital.Equal(decimal.NewFromInt(1000)), "custodial after injection, want 1000 got %s", custodialBalCapital)

	_, err = ledgerStore.ExecuteTemplate(ctx, "capital_withdraw", core.TemplateParams{
		HolderID:       sysHolder,
		CurrencyUID:    curC,
		IdempotencyKey: postgrestest.UniqueKey("capital-withdraw"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(300)},
		Source:         "test",
	})
	require.NoError(t, err)

	equityBal, err = ledgerStore.GetBalance(ctx, -sysHolder, curC, equity.UID)
	require.NoError(t, err)
	assert.True(t, equityBal.Equal(decimal.NewFromInt(700)), "equity after a 300 withdrawal from 1000, want 700 got %s", equityBal)

	// --- Settlement: gross (no fee) and net (with fee) legs, real store.
	merchant := int64(910003)

	_, err = ledgerStore.ExecuteTemplate(ctx, "checkout_settlement_gross", core.TemplateParams{
		HolderID:       merchant,
		CurrencyUID:    curC,
		IdempotencyKey: postgrestest.UniqueKey("settle-gross"),
		Amounts:        map[string]decimal.Decimal{"gross_amount": decimal.NewFromInt(200)},
		Source:         "test",
	})
	require.NoError(t, err)

	// "must move" was all this asserted, so it stayed green while the entry
	// moved custody the wrong way (audit A-M2). A merchant settlement brings
	// money IN: custody rises by gross.
	custodialAfterGross, err := ledgerStore.GetBalance(ctx, -merchant, curC, custodial.UID)
	require.NoError(t, err)
	assert.True(t, custodialAfterGross.Equal(decimal.NewFromInt(200)), "custodial after checkout_settlement_gross, want 200 got %s", custodialAfterGross)

	merchantAfterGross, err := ledgerStore.GetBalance(ctx, merchant, curC, mainWallet.UID)
	require.NoError(t, err)
	assert.True(t, merchantAfterGross.Equal(decimal.NewFromInt(200)), "the settled merchant must be credited, want 200 got %s", merchantAfterGross)

	_, err = ledgerStore.ExecuteTemplate(ctx, "checkout_settlement_net", core.TemplateParams{
		HolderID:       merchant,
		CurrencyUID:    curC,
		IdempotencyKey: postgrestest.UniqueKey("settle-net"),
		Amounts: map[string]decimal.Decimal{
			"gross_amount": decimal.NewFromInt(100),
			"net_amount":   decimal.NewFromInt(95),
			"fee_amount":   decimal.NewFromInt(5),
		},
		Source: "test",
	})
	require.NoError(t, err)

	feesBal, err := ledgerStore.GetBalance(ctx, -merchant, curC, fees.UID)
	require.NoError(t, err)
	assert.True(t, feesBal.Equal(decimal.NewFromInt(5)), "fees classification must record the fee_amount entry, want 5 got %s", feesBal)

	custodialAfterNet, err := ledgerStore.GetBalance(ctx, -merchant, curC, custodial.UID)
	require.NoError(t, err)
	assert.True(t, custodialAfterNet.Equal(decimal.NewFromInt(295)), "custody holds net (200+95); the 5 of fee is the platform's own money in `fees`, want 295 got %s", custodialAfterNet)

	merchantAfterNet, err := ledgerStore.GetBalance(ctx, merchant, curC, mainWallet.UID)
	require.NoError(t, err)
	assert.True(t, merchantAfterNet.Equal(decimal.NewFromInt(295)), "merchant receives net, want 295 got %s", merchantAfterNet)
}

func TestInstallDefaultTemplatePresets(t *testing.T) {
	pool := postgrestest.SetupDB(t)
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

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	userID := int64(42)

	journal, err := ledgerStore.ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
		HolderID:       userID,
		CurrencyUID:    curID,
		IdempotencyKey: postgrestest.UniqueKey("preset-deposit"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(500)},
		Source:         "test",
	})
	require.NoError(t, err)
	assert.True(t, journal.TotalDebit.Equal(decimal.NewFromInt(500)))

	_, err = ledgerStore.ExecuteTemplate(ctx, "lock_funds", core.TemplateParams{
		HolderID:       userID,
		CurrencyUID:    curID,
		IdempotencyKey: postgrestest.UniqueKey("preset-lock-release"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(40)},
		Source:         "test",
	})
	require.NoError(t, err)

	_, err = ledgerStore.ExecuteTemplate(ctx, "unlock_funds", core.TemplateParams{
		HolderID:       userID,
		CurrencyUID:    curID,
		IdempotencyKey: postgrestest.UniqueKey("preset-unlock"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(40)},
		Source:         "test",
	})
	require.NoError(t, err)

	_, err = ledgerStore.ExecuteTemplate(ctx, "lock_funds", core.TemplateParams{
		HolderID:       userID,
		CurrencyUID:    curID,
		IdempotencyKey: postgrestest.UniqueKey("preset-lock-withdraw"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(105)},
		Source:         "test",
	})
	require.NoError(t, err)

	_, err = ledgerStore.ExecuteTemplate(ctx, "withdraw_fee", core.TemplateParams{
		HolderID:       userID,
		CurrencyUID:    curID,
		IdempotencyKey: postgrestest.UniqueKey("preset-withdraw-fee"),
		Amounts:        map[string]decimal.Decimal{"fee": decimal.NewFromInt(5)},
		Source:         "test",
	})
	require.NoError(t, err)

	_, err = ledgerStore.ExecuteTemplate(ctx, "withdraw_confirm", core.TemplateParams{
		HolderID:       userID,
		CurrencyUID:    curID,
		IdempotencyKey: postgrestest.UniqueKey("preset-withdraw-confirm"),
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

	walletBal, err := ledgerStore.GetBalance(ctx, userID, curID, mainWallet.UID)
	require.NoError(t, err)
	assert.True(t, walletBal.Equal(decimal.NewFromInt(395)))

	lockedBal, err := ledgerStore.GetBalance(ctx, userID, curID, locked.UID)
	require.NoError(t, err)
	assert.True(t, lockedBal.IsZero())

	feeExpenseBal, err := ledgerStore.GetBalance(ctx, userID, curID, feeExpense.UID)
	require.NoError(t, err)
	assert.True(t, feeExpenseBal.Equal(decimal.NewFromInt(5)))

	custodialBal, err := ledgerStore.GetBalance(ctx, -userID, curID, custodial.UID)
	require.NoError(t, err)
	assert.True(t, custodialBal.Equal(decimal.NewFromInt(395)))

	feeRevenueBal, err := ledgerStore.GetBalance(ctx, -userID, curID, feeRevenue.UID)
	require.NoError(t, err)
	assert.True(t, feeRevenueBal.Equal(decimal.NewFromInt(5)))

	stagedUserID := int64(99)

	_, err = ledgerStore.ExecuteTemplate(ctx, "deposit_pending", core.TemplateParams{
		HolderID:       stagedUserID,
		CurrencyUID:    curID,
		IdempotencyKey: postgrestest.UniqueKey("preset-staged-deposit-pending"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(100)},
		Source:         "test",
	})
	require.NoError(t, err)

	_, err = ledgerStore.ExecuteTemplate(ctx, "deposit_confirm_pending", core.TemplateParams{
		HolderID:       stagedUserID,
		CurrencyUID:    curID,
		IdempotencyKey: postgrestest.UniqueKey("preset-staged-deposit-confirm"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(95)},
		Source:         "test",
	})
	require.NoError(t, err)

	stagedWalletBal, err := ledgerStore.GetBalance(ctx, stagedUserID, curID, mainWallet.UID)
	require.NoError(t, err)
	assert.True(t, stagedWalletBal.Equal(decimal.NewFromInt(95)))

	stagedPendingBal, err := ledgerStore.GetBalance(ctx, stagedUserID, curID, pending.UID)
	require.NoError(t, err)
	assert.True(t, stagedPendingBal.Equal(decimal.NewFromInt(5)))

	stagedSuspenseBal, err := ledgerStore.GetBalance(ctx, -stagedUserID, curID, suspense.UID)
	require.NoError(t, err)
	assert.True(t, stagedSuspenseBal.Equal(decimal.NewFromInt(5)))

	stagedCustodialBal, err := ledgerStore.GetBalance(ctx, -stagedUserID, curID, custodial.UID)
	require.NoError(t, err)
	assert.True(t, stagedCustodialBal.Equal(decimal.NewFromInt(95)))

	_, err = ledgerStore.ExecuteTemplate(ctx, "deposit_release_pending", core.TemplateParams{
		HolderID:       stagedUserID,
		CurrencyUID:    curID,
		IdempotencyKey: postgrestest.UniqueKey("preset-staged-deposit-release"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(5)},
		Source:         "test",
	})
	require.NoError(t, err)

	stagedPendingBal, err = ledgerStore.GetBalance(ctx, stagedUserID, curID, pending.UID)
	require.NoError(t, err)
	assert.True(t, stagedPendingBal.IsZero())

	stagedSuspenseBal, err = ledgerStore.GetBalance(ctx, -stagedUserID, curID, suspense.UID)
	require.NoError(t, err)
	assert.True(t, stagedSuspenseBal.IsZero())
}

func TestExecuteDepositTolerancePlan(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	tmplStore := postgres.NewTemplateStore(pool)
	ledgerStore := postgres.NewLedgerStore(pool)

	require.NoError(t, presets.InstallDefaultTemplatePresets(ctx, classStore, classStore, tmplStore))

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
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
		CurrencyUID:    curID,
		IdempotencyKey: postgrestest.UniqueKey("tolerance-short-pending"),
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
		CurrencyUID:    curID,
		IdempotencyKey: postgrestest.UniqueKey("tolerance-short"),
		Source:         "test",
	}, shortPlan)
	require.NoError(t, err)

	shortWalletBal, err := ledgerStore.GetBalance(ctx, shortUserID, curID, mainWallet.UID)
	require.NoError(t, err)
	assert.True(t, shortWalletBal.Equal(decimal.NewFromInt(98)))

	shortPendingBal, err := ledgerStore.GetBalance(ctx, shortUserID, curID, pending.UID)
	require.NoError(t, err)
	assert.True(t, shortPendingBal.IsZero())

	shortSuspenseBal, err := ledgerStore.GetBalance(ctx, -shortUserID, curID, suspense.UID)
	require.NoError(t, err)
	assert.True(t, shortSuspenseBal.IsZero())

	shortCustodialBal, err := ledgerStore.GetBalance(ctx, -shortUserID, curID, custodial.UID)
	require.NoError(t, err)
	assert.True(t, shortCustodialBal.Equal(decimal.NewFromInt(98)))

	overUserID := int64(502)
	_, err = ledgerStore.ExecuteTemplate(ctx, "deposit_pending", core.TemplateParams{
		HolderID:       overUserID,
		CurrencyUID:    curID,
		IdempotencyKey: postgrestest.UniqueKey("tolerance-over-pending"),
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
		CurrencyUID:    curID,
		IdempotencyKey: postgrestest.UniqueKey("tolerance-over"),
		Source:         "test",
	}, overPlan)
	require.NoError(t, err)

	overWalletBal, err := ledgerStore.GetBalance(ctx, overUserID, curID, mainWallet.UID)
	require.NoError(t, err)
	assert.True(t, overWalletBal.Equal(decimal.NewFromInt(100)))

	overPendingBal, err := ledgerStore.GetBalance(ctx, overUserID, curID, pending.UID)
	require.NoError(t, err)
	assert.True(t, overPendingBal.IsZero())

	overSuspenseBal, err := ledgerStore.GetBalance(ctx, -overUserID, curID, suspense.UID)
	require.NoError(t, err)
	assert.True(t, overSuspenseBal.Equal(decimal.NewFromInt(10)))

	overCustodialBal, err := ledgerStore.GetBalance(ctx, -overUserID, curID, custodial.UID)
	require.NoError(t, err)
	assert.True(t, overCustodialBal.Equal(decimal.NewFromInt(110)))

	_, err = ledgerStore.ExecuteTemplate(ctx, "deposit_resolve_overage", core.TemplateParams{
		HolderID:       overUserID,
		CurrencyUID:    curID,
		IdempotencyKey: postgrestest.UniqueKey("tolerance-over-resolve"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(10)},
		Source:         "test",
	})
	require.NoError(t, err)

	overWalletBal, err = ledgerStore.GetBalance(ctx, overUserID, curID, mainWallet.UID)
	require.NoError(t, err)
	assert.True(t, overWalletBal.Equal(decimal.NewFromInt(110)))

	overSuspenseBal, err = ledgerStore.GetBalance(ctx, -overUserID, curID, suspense.UID)
	require.NoError(t, err)
	assert.True(t, overSuspenseBal.IsZero())
}

func TestExecuteDepositTolerancePlan_BatchRollbackOnFailure(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	tmplStore := postgres.NewTemplateStore(pool)
	ledgerStore := postgres.NewLedgerStore(pool)

	require.NoError(t, presets.InstallDefaultTemplatePresets(ctx, classStore, classStore, tmplStore))

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	mainWallet, err := classStore.GetByCode(ctx, "main_wallet")
	require.NoError(t, err)
	pending, err := classStore.GetByCode(ctx, "pending")
	require.NoError(t, err)
	suspense, err := classStore.GetByCode(ctx, "suspense")
	require.NoError(t, err)
	custodial, err := classStore.GetByCode(ctx, "custodial")
	require.NoError(t, err)

	userID := int64(777)
	_, err = ledgerStore.ExecuteTemplate(ctx, "deposit_pending", core.TemplateParams{
		HolderID:       userID,
		CurrencyUID:    curID,
		IdempotencyKey: postgrestest.UniqueKey("tolerance-batch-pending"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(100)},
		Source:         "test",
	})
	require.NoError(t, err)

	_, err = presets.ExecuteDepositTolerancePlan(ctx, ledgerStore, core.TemplateParams{
		HolderID:       userID,
		CurrencyUID:    curID,
		IdempotencyKey: postgrestest.UniqueKey("tolerance-batch-fail"),
		Source:         "test",
	}, &presets.DepositTolerancePlan{
		ExpectedAmount:  decimal.NewFromInt(100),
		ActualAmount:    decimal.NewFromInt(100),
		ToleranceAmount: decimal.Zero,
		Outcome:         presets.DepositToleranceExactMatch,
		Steps: []presets.TemplateExecution{
			{
				TemplateCode:      "deposit_confirm_pending",
				IdempotencySuffix: "confirm-pending",
				Amounts:           map[string]decimal.Decimal{"amount": decimal.NewFromInt(100)},
			},
			{
				TemplateCode:      "missing_template",
				IdempotencySuffix: "missing-step",
				Amounts:           map[string]decimal.Decimal{"amount": decimal.NewFromInt(1)},
			},
		},
	})
	require.Error(t, err)

	walletBal, err := ledgerStore.GetBalance(ctx, userID, curID, mainWallet.UID)
	require.NoError(t, err)
	assert.True(t, walletBal.IsZero())

	pendingBal, err := ledgerStore.GetBalance(ctx, userID, curID, pending.UID)
	require.NoError(t, err)
	assert.True(t, pendingBal.Equal(decimal.NewFromInt(100)))

	suspenseBal, err := ledgerStore.GetBalance(ctx, -userID, curID, suspense.UID)
	require.NoError(t, err)
	assert.True(t, suspenseBal.Equal(decimal.NewFromInt(100)))

	custodialBal, err := ledgerStore.GetBalance(ctx, -userID, curID, custodial.UID)
	require.NoError(t, err)
	assert.True(t, custodialBal.IsZero())
}

// Pins the expand-safe balance_role upgrade: a classification created before
// balance_role existed (role ”) is retagged in place by preset install; a
// conflicting non-empty role is rejected instead of silently re-bucketed.
func TestInstallPresets_BalanceRoleUpgradeAndConflict(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	tmplStore := postgres.NewTemplateStore(pool)

	// Pre-existing row without a role (pre-032 install). Seeded via raw SQL,
	// not classStore.CreateClassification: ClassificationInput.Validate now
	// refuses a non-system classification with no balance_role (M-4 fix) --
	// exactly the shape this test needs to simulate as PRE-EXISTING data
	// that predates Validate ever having run.
	postgrestest.SeedClassification(t, pool, "main_wallet", "Main Wallet", "debit", false)
	preexisting, err := classStore.GetByCode(ctx, "main_wallet")
	require.NoError(t, err)
	require.Equal(t, core.BalanceRoleNone, preexisting.BalanceRole)

	require.NoError(t, presets.InstallDefaultTemplatePresets(ctx, classStore, classStore, tmplStore))

	upgraded, err := classStore.GetByCode(ctx, "main_wallet")
	require.NoError(t, err)
	assert.Equal(t, core.BalanceRoleAvailable, upgraded.BalanceRole)

	locked, err := classStore.GetByCode(ctx, "locked")
	require.NoError(t, err)
	assert.Equal(t, core.BalanceRoleLocked, locked.BalanceRole)

	pending, err := classStore.GetByCode(ctx, "pending")
	require.NoError(t, err)
	assert.Equal(t, core.BalanceRolePending, pending.BalanceRole)

	feeExpense, err := classStore.GetByCode(ctx, "fee_expense")
	require.NoError(t, err)
	// M-4 fix: fee_expense is now explicitly tagged BalanceRoleMemo instead
	// of relying on BalanceRoleNone to mean both "deliberate memo account"
	// and "nobody tagged this yet" (docs/INVARIANTS.md I-37 addendum).
	assert.Equal(t, core.BalanceRoleMemo, feeExpense.BalanceRole)

	// Re-install is a no-op (roles already match).
	require.NoError(t, presets.InstallDefaultTemplatePresets(ctx, classStore, classStore, tmplStore))
}

// A pre-existing classification whose balance_role is already non-empty and
// DIFFERENT from what the preset wants must be rejected, not silently
// re-bucketed. Constructed via CreateClassification (INSERT), not via
// SetBalanceRole on an already-upgraded row: migration 045's mutation guard
// (I-25) now makes SetBalanceRole itself refuse a non-empty -> non-empty
// transition, so the only way a real deployment reaches this conflict is a
// row that was tagged with a non-” role from the start (e.g. a different,
// earlier preset install choosing a different role for the same code).
func TestInstallPresets_BalanceRoleConflictAtCreation(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	tmplStore := postgres.NewTemplateStore(pool)

	preexisting, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "main_wallet", Name: "Main Wallet", NormalSide: core.NormalSideDebit,
		BalanceRole: core.BalanceRoleLocked, // preset wants 'available'
	})
	require.NoError(t, err)
	require.Equal(t, core.BalanceRoleLocked, preexisting.BalanceRole)

	err = presets.InstallDefaultTemplatePresets(ctx, classStore, classStore, tmplStore)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)
}

// TestInstallPresets_HolderKindUpgradeAndConflict pins the M-7 fix
// (docs/INVARIANTS.md I-44): a pre-existing, untagged journal type row is
// retagged in place on install (expand-safe upgrade, mirroring
// BalanceRole's), and a pre-existing row whose holder_kind already
// disagrees with the preset is rejected rather than silently overwritten.
func TestInstallPresets_HolderKindUpgradeAndConflict(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	tmplStore := postgres.NewTemplateStore(pool)

	// Pre-existing row without a holder_kind (pre-012 install). Seeded via
	// raw SQL, not classStore.CreateJournalType with an explicit kind --
	// simulating data that predates this column, exactly like the
	// BalanceRole upgrade test above simulates pre-032 classifications.
	postgrestest.SeedJournalType(t, pool, "deposit_confirm", "Deposit Confirm")
	preexisting, err := classStore.GetJournalTypeByCode(ctx, "deposit_confirm")
	require.NoError(t, err)
	require.Equal(t, core.HolderTxKindNone, preexisting.HolderKind)

	require.NoError(t, presets.InstallDefaultTemplatePresets(ctx, classStore, classStore, tmplStore))

	upgraded, err := classStore.GetJournalTypeByCode(ctx, "deposit_confirm")
	require.NoError(t, err)
	assert.Equal(t, core.HolderTxKindDeposit, upgraded.HolderKind)

	withdrawFee, err := classStore.GetJournalTypeByCode(ctx, "withdraw_fee")
	require.NoError(t, err)
	assert.Equal(t, core.HolderTxKindFee, withdrawFee.HolderKind)

	// Re-install is a no-op (kinds already match).
	require.NoError(t, presets.InstallDefaultTemplatePresets(ctx, classStore, classStore, tmplStore))
}

// A pre-existing journal type whose holder_kind is already non-empty and
// DIFFERENT from what the preset wants must be rejected, not silently
// re-tagged -- the journal-type counterpart of
// TestInstallPresets_BalanceRoleConflictAtCreation above.
func TestInstallPresets_HolderKindConflictAtCreation(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	tmplStore := postgres.NewTemplateStore(pool)

	preexisting, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{
		Code: "deposit_confirm", Name: "Deposit Confirm",
		HolderKind: core.HolderTxKindOther, // preset wants 'deposit'
	})
	require.NoError(t, err)
	require.Equal(t, core.HolderTxKindOther, preexisting.HolderKind)

	err = presets.InstallDefaultTemplatePresets(ctx, classStore, classStore, tmplStore)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)
}
