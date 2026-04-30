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

// Confirms that GetPlatformBalances reflects a freshly-posted journal
// immediately, without waiting for the rollup worker to materialise a
// checkpoint. The old (cached) implementation would have reported zero for the
// brand-new account because there is no balance_checkpoints row yet.
func TestPlatformBalance_RealtimeReflectsUnrolledJournal(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	classStore := postgres.NewClassificationStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)
	pbStore := postgres.NewPlatformBalanceStore(pool)

	usdt, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{
		Code: "USDT-RT", Name: "Tether USD Realtime",
	})
	require.NoError(t, err)

	mainWallet, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "main_wallet_rt", Name: "Main Wallet RT", NormalSide: core.NormalSideDebit,
	})
	require.NoError(t, err)

	custodial, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "custodial", Name: "Custodial RT", NormalSide: core.NormalSideDebit, IsSystem: true,
	})
	require.NoError(t, err)

	jt, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{
		Code: "deposit_rt", Name: "Deposit Realtime",
	})
	require.NoError(t, err)

	userID := int64(7001)
	sysID := core.SystemAccountHolder(userID)

	// Sanity: with no journals the platform balance is empty.
	pb0, err := pbStore.GetPlatformBalances(ctx, usdt.ID)
	require.NoError(t, err)
	assert.Empty(t, pb0.UserSide)
	assert.Empty(t, pb0.SystemSide)

	// Post a journal. It writes to journal_entries and rollup_queue, but does
	// NOT touch balance_checkpoints — that's the rollup worker's job, which we
	// deliberately do not run here.
	_, err = ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jt.ID,
		IdempotencyKey: postgrestest.UniqueKey("rt-deposit"),
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyID: usdt.ID, ClassificationID: mainWallet.ID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(500)},
			{AccountHolder: sysID, CurrencyID: usdt.ID, ClassificationID: custodial.ID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(500)},
		},
		Source: "test",
	})
	require.NoError(t, err)

	// Verify no checkpoint rows exist yet (this is what makes this a true
	// realtime test — if checkpoints existed the old implementation would also
	// have reported the right number).
	var cpCount int
	require.NoError(t,
		pool.QueryRow(ctx,
			"SELECT count(*) FROM balance_checkpoints WHERE currency_id = $1 AND account_holder IN ($2, $3)",
			usdt.ID, userID, sysID,
		).Scan(&cpCount),
	)
	require.Zero(t, cpCount, "test invalid: checkpoints should not exist before rollup runs")

	// Realtime read: must reflect the journal even with no checkpoints.
	pb, err := pbStore.GetPlatformBalances(ctx, usdt.ID)
	require.NoError(t, err)

	assert.True(t,
		pb.UserSide["main_wallet_rt"].Equal(decimal.NewFromInt(500)),
		"user side main_wallet_rt: expected 500, got %s (cached implementation would return 0)",
		pb.UserSide["main_wallet_rt"],
	)
	// System custodial: debit-normal classification, credit entry → -500.
	assert.True(t,
		pb.SystemSide["custodial"].Equal(decimal.NewFromInt(-500)),
		"system side custodial: expected -500, got %s",
		pb.SystemSide["custodial"],
	)

	// Liability: 500 (the only user-side entry).
	liability, err := pbStore.GetTotalLiabilityByAsset(ctx, usdt.ID)
	require.NoError(t, err)
	assert.True(t, liability.Equal(decimal.NewFromInt(500)),
		"liability: expected 500, got %s", liability)
}

// Confirms that an additional journal between two reads is visible in the
// second read with no rollup tick between them. This exercises the
// continuously-updating real-time property.
func TestPlatformBalance_RealtimeReflectsSecondJournal(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	classStore := postgres.NewClassificationStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)
	pbStore := postgres.NewPlatformBalanceStore(pool)

	usdt, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{Code: "USDT-RT2", Name: "Tether USD RT2"})
	require.NoError(t, err)

	mainWallet, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "mw_rt2", Name: "Main Wallet RT2", NormalSide: core.NormalSideDebit,
	})
	require.NoError(t, err)

	custodial, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "custodial", Name: "Custodial RT2", NormalSide: core.NormalSideDebit, IsSystem: true,
	})
	require.NoError(t, err)

	jt, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{Code: "deposit_rt2", Name: "Deposit RT2"})
	require.NoError(t, err)

	userID := int64(7002)
	sysID := core.SystemAccountHolder(userID)

	post := func(amount int64, idem string) {
		_, err := ledgerStore.PostJournal(ctx, core.JournalInput{
			JournalTypeID:  jt.ID,
			IdempotencyKey: idem,
			Entries: []core.EntryInput{
				{AccountHolder: userID, CurrencyID: usdt.ID, ClassificationID: mainWallet.ID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(amount)},
				{AccountHolder: sysID, CurrencyID: usdt.ID, ClassificationID: custodial.ID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(amount)},
			},
			Source: "test",
		})
		require.NoError(t, err)
	}

	post(100, postgrestest.UniqueKey("rt2-1"))

	pb1, err := pbStore.GetPlatformBalances(ctx, usdt.ID)
	require.NoError(t, err)
	assert.True(t, pb1.UserSide["mw_rt2"].Equal(decimal.NewFromInt(100)))

	post(250, postgrestest.UniqueKey("rt2-2"))

	pb2, err := pbStore.GetPlatformBalances(ctx, usdt.ID)
	require.NoError(t, err)
	assert.True(t, pb2.UserSide["mw_rt2"].Equal(decimal.NewFromInt(350)),
		"second read should see both journals: expected 350, got %s", pb2.UserSide["mw_rt2"])
}

// Confirms SolvencyCheck is realtime: a journal that grants user balance
// without backing it with custodial — making liability exceed custodial — is
// detected immediately, no rollup tick required.
func TestPlatformBalance_RealtimeSolvencyCheck(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	classStore := postgres.NewClassificationStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)
	pbStore := postgres.NewPlatformBalanceStore(pool)

	usdt, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{Code: "USDT-SOLV", Name: "Tether USD Solv"})
	require.NoError(t, err)

	// Liability account: credit-normal (what we owe users).
	mainWallet, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "mw_solv", Name: "MW Solv", NormalSide: core.NormalSideCredit,
	})
	require.NoError(t, err)
	// Custodial: debit-normal asset (what we hold).
	custodial, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "custodial", Name: "Custodial Solv", NormalSide: core.NormalSideDebit, IsSystem: true,
	})
	require.NoError(t, err)
	// Promo expense: debit-normal, lets us grant user balance without backing.
	promo, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "promo_expense", Name: "Promo Expense", NormalSide: core.NormalSideDebit, IsSystem: true,
	})
	require.NoError(t, err)

	jt, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{Code: "deposit_solv", Name: "Deposit Solv"})
	require.NoError(t, err)

	user := int64(8001)
	sys := core.SystemAccountHolder(user)

	// Real deposit: DR custodial 1000, CR main_wallet 1000.
	// → custodial = +1000 (debit normal + debit), main_wallet = +1000 (credit normal + credit).
	_, err = ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jt.ID,
		IdempotencyKey: postgrestest.UniqueKey("solv-deposit"),
		Entries: []core.EntryInput{
			{AccountHolder: sys, CurrencyID: usdt.ID, ClassificationID: custodial.ID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(1000)},
			{AccountHolder: user, CurrencyID: usdt.ID, ClassificationID: mainWallet.ID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(1000)},
		},
		Source: "test",
	})
	require.NoError(t, err)

	report, err := pbStore.SolvencyCheck(ctx, usdt.ID)
	require.NoError(t, err)
	assert.True(t, report.Solvent, "after balanced deposit should be solvent: %+v", report)
	assert.True(t, report.Liability.Equal(decimal.NewFromInt(1000)), "liability=1000, got %s", report.Liability)
	assert.True(t, report.Custodial.Equal(decimal.NewFromInt(1000)), "custodial=1000, got %s", report.Custodial)
	assert.True(t, report.Margin.IsZero(), "margin=0 (exactly solvent), got %s", report.Margin)

	// Promo grant: DR promo_expense 500, CR main_wallet 500.
	// → main_wallet → +1500 (liability), custodial unchanged.
	_, err = ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jt.ID,
		IdempotencyKey: postgrestest.UniqueKey("solv-grant"),
		Entries: []core.EntryInput{
			{AccountHolder: sys, CurrencyID: usdt.ID, ClassificationID: promo.ID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(500)},
			{AccountHolder: user, CurrencyID: usdt.ID, ClassificationID: mainWallet.ID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(500)},
		},
		Source: "test",
	})
	require.NoError(t, err)

	report2, err := pbStore.SolvencyCheck(ctx, usdt.ID)
	require.NoError(t, err)
	assert.False(t, report2.Solvent, "after liability bump should be insolvent: %+v", report2)
	assert.True(t, report2.Liability.Equal(decimal.NewFromInt(1500)), "liability=1500, got %s", report2.Liability)
	assert.True(t, report2.Custodial.Equal(decimal.NewFromInt(1000)), "custodial=1000, got %s", report2.Custodial)
	assert.True(t, report2.Margin.Equal(decimal.NewFromInt(-500)), "margin=-500, got %s", report2.Margin)
}
