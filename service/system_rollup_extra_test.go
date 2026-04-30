package service_test

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

// --- Mock PlatformBalanceQuerier ---

type mockPlatformBalanceQuerier struct {
	pb        *core.PlatformBalance
	liability decimal.Decimal
	solvency  *core.SolvencyReport
}

func (m *mockPlatformBalanceQuerier) GetPlatformBalances(_ context.Context, _ int64) (*core.PlatformBalance, error) {
	return m.pb, nil
}

func (m *mockPlatformBalanceQuerier) GetTotalLiabilityByAsset(_ context.Context, _ int64) (decimal.Decimal, error) {
	return m.liability, nil
}

func (m *mockPlatformBalanceQuerier) SolvencyCheck(_ context.Context, _ int64) (*core.SolvencyReport, error) {
	return m.solvency, nil
}

// mockCheckpointAgg and mockRollupWriter exist only to build a SystemRollupService.

type mockCheckpointAgg struct{}

func (m *mockCheckpointAgg) AggregateCheckpointsByClassification(_ context.Context) ([]core.SystemRollup, error) {
	return nil, nil
}

type mockRollupWriter struct{}

func (m *mockRollupWriter) UpsertSystemRollup(_ context.Context, _ core.SystemRollup) error {
	return nil
}

func newTestSvc() *service.SystemRollupService {
	return service.NewSystemRollupService(&mockCheckpointAgg{}, &mockRollupWriter{}, core.NewEngine())
}

// --- Unit tests: service delegation without a querier ---

func TestSystemRollupService_GetPlatformBalances_NoQuerier(t *testing.T) {
	_, err := newTestSvc().GetPlatformBalances(context.Background(), 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "platform balance querier not configured")
}

func TestSystemRollupService_GetTotalLiabilityByAsset_NoQuerier(t *testing.T) {
	_, err := newTestSvc().GetTotalLiabilityByAsset(context.Background(), 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "platform balance querier not configured")
}

func TestSystemRollupService_SolvencyCheck_NoQuerier(t *testing.T) {
	_, err := newTestSvc().SolvencyCheck(context.Background(), 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "platform balance querier not configured")
}

// --- Unit tests: service delegation with a mock querier ---

func TestSystemRollupService_GetPlatformBalances_WithQuerier(t *testing.T) {
	expected := &core.PlatformBalance{
		CurrencyID: 1,
		UserSide:   map[string]decimal.Decimal{"main_wallet": decimal.NewFromInt(5000)},
		SystemSide: map[string]decimal.Decimal{"custodial": decimal.NewFromInt(5000)},
	}
	svc := newTestSvc().WithPlatformBalanceQuerier(&mockPlatformBalanceQuerier{pb: expected})
	got, err := svc.GetPlatformBalances(context.Background(), 1)
	require.NoError(t, err)
	assert.Equal(t, int64(1), got.CurrencyID)
	assert.True(t, got.UserSide["main_wallet"].Equal(decimal.NewFromInt(5000)))
	assert.True(t, got.SystemSide["custodial"].Equal(decimal.NewFromInt(5000)))
}

func TestSystemRollupService_GetTotalLiabilityByAsset_WithQuerier(t *testing.T) {
	svc := newTestSvc().WithPlatformBalanceQuerier(&mockPlatformBalanceQuerier{
		liability: decimal.NewFromInt(12345),
	})
	got, err := svc.GetTotalLiabilityByAsset(context.Background(), 1)
	require.NoError(t, err)
	assert.True(t, got.Equal(decimal.NewFromInt(12345)))
}

func TestSystemRollupService_SolvencyCheck_Solvent(t *testing.T) {
	report := &core.SolvencyReport{
		CurrencyID: 1,
		Liability:  decimal.NewFromInt(1000),
		Custodial:  decimal.NewFromInt(1200),
		Solvent:    true,
		Margin:     decimal.NewFromInt(200),
	}
	svc := newTestSvc().WithPlatformBalanceQuerier(&mockPlatformBalanceQuerier{solvency: report})
	got, err := svc.SolvencyCheck(context.Background(), 1)
	require.NoError(t, err)
	assert.True(t, got.Solvent)
	assert.True(t, got.Margin.Equal(decimal.NewFromInt(200)))
}

func TestSystemRollupService_SolvencyCheck_Insolvent(t *testing.T) {
	report := &core.SolvencyReport{
		CurrencyID: 1,
		Liability:  decimal.NewFromInt(2000),
		Custodial:  decimal.NewFromInt(1500),
		Solvent:    false,
		Margin:     decimal.NewFromInt(-500),
	}
	svc := newTestSvc().WithPlatformBalanceQuerier(&mockPlatformBalanceQuerier{solvency: report})
	got, err := svc.SolvencyCheck(context.Background(), 1)
	require.NoError(t, err)
	assert.False(t, got.Solvent)
	assert.True(t, got.Margin.IsNegative())
}

// --- Integration tests: postgres.PlatformBalanceStore with real DB ---
//
// These tests require Docker to be running. They are skipped automatically
// when the Docker daemon is not available (via postgrestest.SetupDB).

func TestPlatformBalanceStore_GetPlatformBalances(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	classStore := postgres.NewClassificationStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)
	pbStore := postgres.NewPlatformBalanceStore(pool)

	usdt, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{Code: "USDT-PB1", Name: "Tether USD PB1"})
	require.NoError(t, err)

	mainWallet, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "main_wallet_pb1", Name: "Main Wallet PB1", NormalSide: core.NormalSideDebit,
	})
	require.NoError(t, err)

	custodialClass, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "custodial_pb1", Name: "Custodial PB1", NormalSide: core.NormalSideDebit, IsSystem: true,
	})
	require.NoError(t, err)

	jtType, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{
		Code: "deposit_pb1", Name: "Deposit PB1",
	})
	require.NoError(t, err)

	userID := int64(1001)
	sysID := core.SystemAccountHolder(userID) // -1001

	// Post a journal: user DR 500 / system CR 500
	_, err = ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jtType.ID,
		IdempotencyKey: postgrestest.UniqueKey("pb-j1"),
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyID: usdt.ID, ClassificationID: mainWallet.ID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(500)},
			{AccountHolder: sysID, CurrencyID: usdt.ID, ClassificationID: custodialClass.ID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(500)},
		},
		Source: "test",
	})
	require.NoError(t, err)

	// Seed balance_checkpoints (rollup worker normally materialises these)
	_, err = pool.Exec(ctx,
		"INSERT INTO balance_checkpoints (account_holder, currency_id, classification_id, balance, last_entry_id, last_entry_at) VALUES ($1, $2, $3, $4, 1, now()) ON CONFLICT DO NOTHING",
		userID, usdt.ID, mainWallet.ID, "500",
	)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		"INSERT INTO balance_checkpoints (account_holder, currency_id, classification_id, balance, last_entry_id, last_entry_at) VALUES ($1, $2, $3, $4, 2, now()) ON CONFLICT DO NOTHING",
		sysID, usdt.ID, custodialClass.ID, "500",
	)
	require.NoError(t, err)

	pb, err := pbStore.GetPlatformBalances(ctx, usdt.ID)
	require.NoError(t, err)
	assert.Equal(t, usdt.ID, pb.CurrencyID)
	assert.True(t, pb.UserSide["main_wallet_pb1"].Equal(decimal.NewFromInt(500)),
		"user side main_wallet_pb1: got %s", pb.UserSide["main_wallet_pb1"])
	assert.True(t, pb.SystemSide["custodial_pb1"].Equal(decimal.NewFromInt(500)),
		"system side custodial_pb1: got %s", pb.SystemSide["custodial_pb1"])
}

func TestPlatformBalanceStore_GetTotalLiabilityByAsset(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	classStore := postgres.NewClassificationStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)
	pbStore := postgres.NewPlatformBalanceStore(pool)

	usdt, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{Code: "USDT-LB1", Name: "Tether USD LB1"})
	require.NoError(t, err)

	mainWallet, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "mw_lb1", Name: "Main Wallet LB1", NormalSide: core.NormalSideCredit,
	})
	require.NoError(t, err)

	locked, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "locked_lb1", Name: "Locked LB1", NormalSide: core.NormalSideCredit,
	})
	require.NoError(t, err)

	custodial, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "custodial_lb1", Name: "Custodial LB1", NormalSide: core.NormalSideDebit, IsSystem: true,
	})
	require.NoError(t, err)

	jt, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{Code: "deposit_lb1", Name: "Deposit LB1"})
	require.NoError(t, err)

	// Edge case: zero liability when no journals exist
	total, err := pbStore.GetTotalLiabilityByAsset(ctx, usdt.ID)
	require.NoError(t, err)
	assert.True(t, total.IsZero(), "expected zero with no data, got %s", total)

	// Real journals: user gets 300 in main_wallet + 100 in locked = 400 total liability.
	// System side: custodial = 400 (matches the deposits) — excluded from liability.
	user := int64(2001)
	sys := core.SystemAccountHolder(user)

	_, err = ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jt.ID,
		IdempotencyKey: postgrestest.UniqueKey("lb1-deposit-mw"),
		Entries: []core.EntryInput{
			{AccountHolder: sys, CurrencyID: usdt.ID, ClassificationID: custodial.ID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(300)},
			{AccountHolder: user, CurrencyID: usdt.ID, ClassificationID: mainWallet.ID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(300)},
		},
		Source: "test",
	})
	require.NoError(t, err)

	_, err = ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jt.ID,
		IdempotencyKey: postgrestest.UniqueKey("lb1-deposit-locked"),
		Entries: []core.EntryInput{
			{AccountHolder: sys, CurrencyID: usdt.ID, ClassificationID: custodial.ID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: user, CurrencyID: usdt.ID, ClassificationID: locked.ID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
		Source: "test",
	})
	require.NoError(t, err)

	total, err = pbStore.GetTotalLiabilityByAsset(ctx, usdt.ID)
	require.NoError(t, err)
	// Only user-side: 300 + 100 = 400. System custodial (400) is excluded.
	assert.True(t, total.Equal(decimal.NewFromInt(400)),
		"expected 400, got %s", total)
}

func TestPlatformBalanceStore_SolvencyCheck_SolventThenInsolvent(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	classStore := postgres.NewClassificationStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)
	pbStore := postgres.NewPlatformBalanceStore(pool)

	usdt, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{Code: "USDT-SC1", Name: "Tether USD SC1"})
	require.NoError(t, err)

	// Liability account (credit-normal — what we owe users).
	mainWallet, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "mw_sc1", Name: "Main Wallet SC1", NormalSide: core.NormalSideCredit,
	})
	require.NoError(t, err)
	// "custodial" code is required by SolvencyCheck (debit-normal asset).
	custodial, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "custodial", Name: "Custodial SC1", NormalSide: core.NormalSideDebit, IsSystem: true,
	})
	require.NoError(t, err)
	// A non-custodial system asset where we can move funds out to break solvency.
	hotWallet, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "hot_wallet_sc1", Name: "Hot Wallet SC1", NormalSide: core.NormalSideDebit, IsSystem: true,
	})
	require.NoError(t, err)

	jt, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{Code: "deposit_sc1", Name: "Deposit SC1"})
	require.NoError(t, err)
	xferType, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{Code: "internal_xfer_sc1", Name: "Internal Transfer SC1"})
	require.NoError(t, err)

	user := int64(3001)
	sys := core.SystemAccountHolder(user)

	// Deposit: DR custodial 1000, CR main_wallet 800 + DR custodial 200... no wait:
	// We want liability=800, custodial=1000. So that's two journals:
	// 1. Deposit 800 (custodial+800, mw+800)
	// 2. House top-up: custodial+200 from somewhere — using a "founders_capital" credit-normal source.
	founders, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "founders_capital_sc1", Name: "Founders Capital SC1", NormalSide: core.NormalSideCredit, IsSystem: true,
	})
	require.NoError(t, err)

	_, err = ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jt.ID,
		IdempotencyKey: postgrestest.UniqueKey("sc1-deposit"),
		Entries: []core.EntryInput{
			{AccountHolder: sys, CurrencyID: usdt.ID, ClassificationID: custodial.ID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(800)},
			{AccountHolder: user, CurrencyID: usdt.ID, ClassificationID: mainWallet.ID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(800)},
		},
		Source: "test",
	})
	require.NoError(t, err)

	_, err = ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jt.ID,
		IdempotencyKey: postgrestest.UniqueKey("sc1-house-topup"),
		Entries: []core.EntryInput{
			{AccountHolder: sys, CurrencyID: usdt.ID, ClassificationID: custodial.ID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(200)},
			{AccountHolder: sys, CurrencyID: usdt.ID, ClassificationID: founders.ID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(200)},
		},
		Source: "test",
	})
	require.NoError(t, err)

	report, err := pbStore.SolvencyCheck(ctx, usdt.ID)
	require.NoError(t, err)
	assert.True(t, report.Solvent, "expected solvent when custodial >= liability: %+v", report)
	assert.True(t, report.Liability.Equal(decimal.NewFromInt(800)), "liability: %s", report.Liability)
	assert.True(t, report.Custodial.Equal(decimal.NewFromInt(1000)), "custodial: %s", report.Custodial)
	assert.True(t, report.Margin.Equal(decimal.NewFromInt(200)), "margin: %s", report.Margin)

	// Move 500 of custodial out to hot_wallet (still on books, just not in
	// "custodial" classification). custodial drops to 500 → insolvent, margin -300.
	_, err = ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  xferType.ID,
		IdempotencyKey: postgrestest.UniqueKey("sc1-xfer-out"),
		Entries: []core.EntryInput{
			{AccountHolder: sys, CurrencyID: usdt.ID, ClassificationID: hotWallet.ID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(500)},
			{AccountHolder: sys, CurrencyID: usdt.ID, ClassificationID: custodial.ID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(500)},
		},
		Source: "test",
	})
	require.NoError(t, err)

	report, err = pbStore.SolvencyCheck(ctx, usdt.ID)
	require.NoError(t, err)
	assert.False(t, report.Solvent, "expected insolvent when custodial < liability: %+v", report)
	assert.True(t, report.Margin.IsNegative(), "margin should be negative: %s", report.Margin)
	assert.True(t, report.Margin.Equal(decimal.NewFromInt(-300)), "margin: %s", report.Margin)
}
