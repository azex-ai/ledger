package postgres_test

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres"
)

func TestLedgerStore_PostJournal_Balanced(t *testing.T) {
	pool := setupTestDB(t)
	store := postgres.NewLedgerStore(pool)
	ctx := context.Background()

	curID := seedCurrency(t, pool, "USDT", "Tether USD")
	jtID := seedJournalType(t, pool, "transfer", "Internal Transfer")
	clsWallet := seedClassification(t, pool, "main_wallet", "Main Wallet", "debit", false)
	clsCustodial := seedClassification(t, pool, "custodial", "Custodial", "credit", true)

	input := core.JournalInput{
		JournalTypeID:  jtID,
		IdempotencyKey: uniqueKey("post-balanced"),
		Entries: []core.EntryInput{
			{AccountHolder: 1, CurrencyID: curID, ClassificationID: clsWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyID: curID, ClassificationID: clsCustodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
		Source: "test",
	}

	journal, err := store.PostJournal(ctx, input)
	require.NoError(t, err)
	assert.True(t, journal.TotalDebit.Equal(decimal.NewFromInt(100)))
	assert.True(t, journal.TotalCredit.Equal(decimal.NewFromInt(100)))
	assert.Equal(t, input.IdempotencyKey, journal.IdempotencyKey)
	assert.True(t, journal.ID > 0)
}

func TestLedgerStore_PostJournal_Idempotent(t *testing.T) {
	pool := setupTestDB(t)
	store := postgres.NewLedgerStore(pool)
	ctx := context.Background()

	curID := seedCurrency(t, pool, "USDT", "Tether USD")
	jtID := seedJournalType(t, pool, "transfer", "Transfer")
	clsA := seedClassification(t, pool, "wallet", "Wallet", "debit", false)
	clsB := seedClassification(t, pool, "custodial", "Custodial", "credit", true)

	key := uniqueKey("idem")
	input := core.JournalInput{
		JournalTypeID:  jtID,
		IdempotencyKey: key,
		Entries: []core.EntryInput{
			{AccountHolder: 1, CurrencyID: curID, ClassificationID: clsA, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(50)},
			{AccountHolder: -1, CurrencyID: curID, ClassificationID: clsB, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(50)},
		},
	}

	j1, err := store.PostJournal(ctx, input)
	require.NoError(t, err)

	j2, err := store.PostJournal(ctx, input)
	require.NoError(t, err)
	assert.Equal(t, j1.ID, j2.ID)
}

func TestLedgerStore_PostJournal_Unbalanced(t *testing.T) {
	pool := setupTestDB(t)
	store := postgres.NewLedgerStore(pool)
	ctx := context.Background()

	curID := seedCurrency(t, pool, "USDT", "Tether USD")
	jtID := seedJournalType(t, pool, "transfer", "Transfer")
	cls := seedClassification(t, pool, "wallet", "Wallet", "debit", false)

	input := core.JournalInput{
		JournalTypeID:  jtID,
		IdempotencyKey: uniqueKey("unbalanced"),
		Entries: []core.EntryInput{
			{AccountHolder: 1, CurrencyID: curID, ClassificationID: cls, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyID: curID, ClassificationID: cls, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(50)},
		},
	}

	_, err := store.PostJournal(ctx, input)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unbalanced")
}

func TestLedgerStore_GetBalance(t *testing.T) {
	pool := setupTestDB(t)
	store := postgres.NewLedgerStore(pool)
	ctx := context.Background()

	curID := seedCurrency(t, pool, "USDT", "Tether USD")
	jtID := seedJournalType(t, pool, "deposit", "Deposit")
	clsWallet := seedClassification(t, pool, "main_wallet", "Main Wallet", "debit", false)
	clsCustodial := seedClassification(t, pool, "custodial", "Custodial", "credit", true)

	// Post a deposit journal: debit wallet, credit custodial
	_, err := store.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jtID,
		IdempotencyKey: uniqueKey("bal-deposit"),
		Entries: []core.EntryInput{
			{AccountHolder: 1, CurrencyID: curID, ClassificationID: clsWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyID: curID, ClassificationID: clsCustodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	})
	require.NoError(t, err)

	// User's wallet balance should be 100 (debit-normal, debits increase)
	bal, err := store.GetBalance(ctx, 1, curID, clsWallet)
	require.NoError(t, err)
	assert.True(t, bal.Equal(decimal.NewFromInt(100)), "expected 100, got %s", bal)

	// System custodial balance should be 100 (credit-normal, credits increase)
	bal, err = store.GetBalance(ctx, -1, curID, clsCustodial)
	require.NoError(t, err)
	assert.True(t, bal.Equal(decimal.NewFromInt(100)), "expected 100, got %s", bal)
}

func TestLedgerStore_GetBalance_MultipleJournals(t *testing.T) {
	pool := setupTestDB(t)
	store := postgres.NewLedgerStore(pool)
	ctx := context.Background()

	curID := seedCurrency(t, pool, "USDT", "Tether USD")
	jtID := seedJournalType(t, pool, "transfer", "Transfer")
	clsWallet := seedClassification(t, pool, "main_wallet", "Main Wallet", "debit", false)
	clsCustodial := seedClassification(t, pool, "custodial", "Custodial", "credit", true)

	// Post two journals
	for i := range 3 {
		_, err := store.PostJournal(ctx, core.JournalInput{
			JournalTypeID:  jtID,
			IdempotencyKey: uniqueKey("multi"),
			Entries: []core.EntryInput{
				{AccountHolder: 1, CurrencyID: curID, ClassificationID: clsWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
				{AccountHolder: -1, CurrencyID: curID, ClassificationID: clsCustodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
			},
		})
		require.NoError(t, err, "journal %d", i)
	}

	bal, err := store.GetBalance(ctx, 1, curID, clsWallet)
	require.NoError(t, err)
	assert.True(t, bal.Equal(decimal.NewFromInt(300)), "expected 300, got %s", bal)
}

func TestLedgerStore_GetBalances(t *testing.T) {
	pool := setupTestDB(t)
	store := postgres.NewLedgerStore(pool)
	ctx := context.Background()

	curID := seedCurrency(t, pool, "USDT", "Tether USD")
	jtID := seedJournalType(t, pool, "transfer", "Transfer")
	clsWallet := seedClassification(t, pool, "main_wallet", "Main Wallet", "debit", false)
	clsLocked := seedClassification(t, pool, "locked", "Locked", "debit", false)
	clsCustodial := seedClassification(t, pool, "custodial", "Custodial", "credit", true)

	// Deposit 200 to wallet
	_, err := store.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jtID,
		IdempotencyKey: uniqueKey("bals-1"),
		Entries: []core.EntryInput{
			{AccountHolder: 1, CurrencyID: curID, ClassificationID: clsWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(200)},
			{AccountHolder: -1, CurrencyID: curID, ClassificationID: clsCustodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(200)},
		},
	})
	require.NoError(t, err)

	// Lock 50 from wallet
	_, err = store.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jtID,
		IdempotencyKey: uniqueKey("bals-2"),
		Entries: []core.EntryInput{
			{AccountHolder: 1, CurrencyID: curID, ClassificationID: clsWallet, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(50)},
			{AccountHolder: 1, CurrencyID: curID, ClassificationID: clsLocked, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(50)},
		},
	})
	require.NoError(t, err)

	bals, err := store.GetBalances(ctx, 1, curID)
	require.NoError(t, err)
	require.Len(t, bals, 2)

	balMap := make(map[int64]decimal.Decimal)
	for _, b := range bals {
		balMap[b.ClassificationID] = b.Balance
	}
	assert.True(t, balMap[clsWallet].Equal(decimal.NewFromInt(150)), "wallet: expected 150, got %s", balMap[clsWallet])
	assert.True(t, balMap[clsLocked].Equal(decimal.NewFromInt(50)), "locked: expected 50, got %s", balMap[clsLocked])
}

func TestLedgerStore_ReverseJournal(t *testing.T) {
	pool := setupTestDB(t)
	store := postgres.NewLedgerStore(pool)
	ctx := context.Background()

	curID := seedCurrency(t, pool, "USDT", "Tether USD")
	jtID := seedJournalType(t, pool, "transfer", "Transfer")
	clsWallet := seedClassification(t, pool, "main_wallet", "Main Wallet", "debit", false)
	clsCustodial := seedClassification(t, pool, "custodial", "Custodial", "credit", true)

	j, err := store.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jtID,
		IdempotencyKey: uniqueKey("rev-orig"),
		Entries: []core.EntryInput{
			{AccountHolder: 1, CurrencyID: curID, ClassificationID: clsWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyID: curID, ClassificationID: clsCustodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	})
	require.NoError(t, err)

	rev, err := store.ReverseJournal(ctx, j.ID, "test-reversal")
	require.NoError(t, err)
	assert.NotZero(t, rev.ReversalOf)
	assert.Equal(t, j.ID, rev.ReversalOf)

	// After reversal, balance should be zero
	bal, err := store.GetBalance(ctx, 1, curID, clsWallet)
	require.NoError(t, err)
	assert.True(t, bal.IsZero(), "expected 0 after reversal, got %s", bal)
}

func TestLedgerStore_ReverseJournal_AlreadyReversed(t *testing.T) {
	pool := setupTestDB(t)
	store := postgres.NewLedgerStore(pool)
	ctx := context.Background()

	curID := seedCurrency(t, pool, "USDT", "Tether USD")
	jtID := seedJournalType(t, pool, "transfer", "Transfer")
	clsWallet := seedClassification(t, pool, "main_wallet", "Main Wallet", "debit", false)
	clsCustodial := seedClassification(t, pool, "custodial", "Custodial", "credit", true)

	j, err := store.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jtID,
		IdempotencyKey: uniqueKey("rev-once"),
		Entries: []core.EntryInput{
			{AccountHolder: 1, CurrencyID: curID, ClassificationID: clsWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyID: curID, ClassificationID: clsCustodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	})
	require.NoError(t, err)

	_, err = store.ReverseJournal(ctx, j.ID, "first")
	require.NoError(t, err)

	_, err = store.ReverseJournal(ctx, j.ID, "second")
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrConflict)
}

func TestLedgerStore_ExecuteTemplate(t *testing.T) {
	pool := setupTestDB(t)
	ledgerStore := postgres.NewLedgerStore(pool)
	tmplStore := postgres.NewTemplateStore(pool)
	ctx := context.Background()

	curID := seedCurrency(t, pool, "USDT", "Tether USD")
	jtID := seedJournalType(t, pool, "deposit_confirm", "Deposit Confirm")
	clsWallet := seedClassification(t, pool, "main_wallet", "Main Wallet", "debit", false)
	clsCustodial := seedClassification(t, pool, "custodial", "Custodial", "credit", true)

	// Create template
	_, err := tmplStore.CreateTemplate(ctx, core.TemplateInput{
		Code:          "deposit_confirm",
		Name:          "Deposit Confirm",
		JournalTypeID: jtID,
		Lines: []core.TemplateLineInput{
			{ClassificationID: clsWallet, EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 1},
			{ClassificationID: clsCustodial, EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 2},
		},
	})
	require.NoError(t, err)

	// Execute template
	j, err := ledgerStore.ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
		HolderID:       42,
		CurrencyID:     curID,
		IdempotencyKey: uniqueKey("tmpl-exec"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(250)},
		Source:         "test",
	})
	require.NoError(t, err)
	assert.True(t, j.TotalDebit.Equal(decimal.NewFromInt(250)))

	// Verify balance
	bal, err := ledgerStore.GetBalance(ctx, 42, curID, clsWallet)
	require.NoError(t, err)
	assert.True(t, bal.Equal(decimal.NewFromInt(250)))
}
