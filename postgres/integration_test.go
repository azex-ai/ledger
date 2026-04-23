package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres"
)

func TestIntegration_FullLedgerFlow(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	// Create stores
	ledgerStore := postgres.NewLedgerStore(pool)
	reserverStore := postgres.NewReserverStore(pool, ledgerStore)
	classStore := postgres.NewClassificationStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)
	tmplStore := postgres.NewTemplateStore(pool)

	// Step 1: Create currency
	usdt, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{Code: "USDT", Name: "Tether USD"})
	require.NoError(t, err)

	// Step 2: Create classifications
	mainWallet, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "main_wallet", Name: "Main Wallet", NormalSide: core.NormalSideDebit,
	})
	require.NoError(t, err)

	locked, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "locked", Name: "Locked", NormalSide: core.NormalSideDebit,
	})
	require.NoError(t, err)

	custodial, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "custodial", Name: "Custodial", NormalSide: core.NormalSideCredit, IsSystem: true,
	})
	require.NoError(t, err)

	fees, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "fees", Name: "Fee Revenue", NormalSide: core.NormalSideCredit, IsSystem: true,
	})
	require.NoError(t, err)

	// Step 3: Create journal types and template
	jtDeposit, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{Code: "deposit_confirm", Name: "Deposit Confirm"})
	require.NoError(t, err)

	jtTransfer, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{Code: "internal_transfer", Name: "Internal Transfer"})
	require.NoError(t, err)

	_, err = tmplStore.CreateTemplate(ctx, core.TemplateInput{
		Code:          "deposit_confirm",
		Name:          "Deposit Confirm",
		JournalTypeID: jtDeposit.ID,
		Lines: []core.TemplateLineInput{
			{ClassificationID: mainWallet.ID, EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 1},
			{ClassificationID: custodial.ID, EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 2},
		},
	})
	require.NoError(t, err)

	userID := int64(42)

	// Step 4: Execute deposit via template
	j, err := ledgerStore.ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
		HolderID:       userID,
		CurrencyID:     usdt.ID,
		IdempotencyKey: uniqueKey("integ-dep-journal"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(1000)},
		Source:         "deposit",
	})
	require.NoError(t, err)
	assert.True(t, j.TotalDebit.Equal(decimal.NewFromInt(1000)))

	// Step 5: Verify balances
	walletBal, err := ledgerStore.GetBalance(ctx, userID, usdt.ID, mainWallet.ID)
	require.NoError(t, err)
	assert.True(t, walletBal.Equal(decimal.NewFromInt(1000)), "wallet: expected 1000, got %s", walletBal)

	custodialBal, err := ledgerStore.GetBalance(ctx, -userID, usdt.ID, custodial.ID)
	require.NoError(t, err)
	assert.True(t, custodialBal.Equal(decimal.NewFromInt(1000)), "custodial: expected 1000, got %s", custodialBal)

	// Step 6: Reserve → Settle flow (lock 200 from wallet)
	// First, create a lock journal: wallet credit 200, locked debit 200
	_, err = ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jtTransfer.ID,
		IdempotencyKey: uniqueKey("integ-lock"),
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyID: usdt.ID, ClassificationID: mainWallet.ID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(200)},
			{AccountHolder: userID, CurrencyID: usdt.ID, ClassificationID: locked.ID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(200)},
		},
		Source: "reserve",
	})
	require.NoError(t, err)

	reservation, err := reserverStore.Reserve(ctx, core.ReserveInput{
		AccountHolder:  userID,
		CurrencyID:     usdt.ID,
		Amount:         decimal.NewFromInt(200),
		IdempotencyKey: uniqueKey("integ-reserve"),
		ExpiresIn:      10 * time.Minute,
	})
	require.NoError(t, err)
	assert.Equal(t, core.ReservationStatusActive, reservation.Status)

	err = reserverStore.Settle(ctx, reservation.ID, decimal.NewFromInt(200))
	require.NoError(t, err)

	// Verify after lock: wallet 800, locked 200
	walletBal, err = ledgerStore.GetBalance(ctx, userID, usdt.ID, mainWallet.ID)
	require.NoError(t, err)
	assert.True(t, walletBal.Equal(decimal.NewFromInt(800)), "wallet after lock: expected 800, got %s", walletBal)

	lockedBal, err := ledgerStore.GetBalance(ctx, userID, usdt.ID, locked.ID)
	require.NoError(t, err)
	assert.True(t, lockedBal.Equal(decimal.NewFromInt(200)), "locked: expected 200, got %s", lockedBal)

	// Step 7: Verify rollup queue has pending items
	var pendingCount int64
	err = pool.QueryRow(ctx, "SELECT COUNT(*) FROM rollup_queue WHERE processed_at IS NULL").Scan(&pendingCount)
	require.NoError(t, err)
	assert.True(t, pendingCount > 0, "rollup queue should have pending items")

	// Step 8: Reconciliation check — total debits == total credits
	var totalDebits, totalCredits string
	err = pool.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN entry_type = 'debit' THEN amount ELSE 0 END), 0)::text,
			COALESCE(SUM(CASE WHEN entry_type = 'credit' THEN amount ELSE 0 END), 0)::text
		FROM journal_entries
	`).Scan(&totalDebits, &totalCredits)
	require.NoError(t, err)
	assert.Equal(t, totalDebits, totalCredits, "accounting equation: debits (%s) must equal credits (%s)", totalDebits, totalCredits)

	// Step 9: Verify fee classification exists but has no entries
	feeBal, err := ledgerStore.GetBalance(ctx, -userID, usdt.ID, fees.ID)
	require.NoError(t, err)
	assert.True(t, feeBal.IsZero(), "fees should be zero")
}
