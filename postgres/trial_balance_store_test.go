package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// Trial balance: total debit must equal total credit (§2 "试算平衡表" check),
// and each row's Net must respect the classification's normal side.
func TestTrialBalanceStore_Balanced(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledgerStore := postgres.NewLedgerStore(pool)
	tbStore := postgres.NewTrialBalanceStore(pool)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	jtID := postgrestest.SeedJournalType(t, pool, "transfer", "Transfer")
	clsWallet := postgrestest.SeedClassification(t, pool, "wallet", "Wallet", "debit", false)
	clsCustodial := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)

	_, err := ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("trial-balance-1"),
		Entries: []core.EntryInput{
			{AccountHolder: 1, CurrencyUID: curID, ClassificationUID: clsWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(200)},
			{AccountHolder: -1, CurrencyUID: curID, ClassificationUID: clsCustodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(200)},
		},
	})
	require.NoError(t, err)

	report, err := tbStore.TrialBalance(ctx, curID, time.Now().Add(time.Minute))
	require.NoError(t, err)

	assert.True(t, report.Balanced)
	assert.True(t, report.TotalDebit.Equal(report.TotalCredit))
	assert.True(t, report.TotalDebit.Equal(decimal.NewFromInt(200)))

	var walletRow, custodialRow *core.TrialBalanceRow
	for i := range report.Rows {
		switch report.Rows[i].ClassificationUID {
		case clsWallet:
			walletRow = &report.Rows[i]
		case clsCustodial:
			custodialRow = &report.Rows[i]
		}
	}
	require.NotNil(t, walletRow)
	require.NotNil(t, custodialRow)
	assert.True(t, walletRow.Net.Equal(decimal.NewFromInt(200)), "debit-normal wallet net = debit - credit")
	assert.True(t, custodialRow.Net.Equal(decimal.NewFromInt(200)), "credit-normal custodial net = credit - debit")
}

// Trial balance as-of cutoff uses effective_at: a backdated entry must be
// included when as_of is after its effective_at, and a future-dated cutoff
// relative to a not-yet-effective retroactive posting must exclude it if
// as_of predates effective_at.
func TestTrialBalanceStore_AsOf_UsesEffectiveAt(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledgerStore := postgres.NewLedgerStore(pool)
	tbStore := postgres.NewTrialBalanceStore(pool)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	jtID := postgrestest.SeedJournalType(t, pool, "transfer", "Transfer")
	clsWallet := postgrestest.SeedClassification(t, pool, "wallet", "Wallet", "debit", false)
	clsCustodial := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)

	backdated := time.Now().AddDate(0, 0, -10)
	_, err := ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("trial-balance-asof"),
		EffectiveAt:    backdated,
		Entries: []core.EntryInput{
			{AccountHolder: 1, CurrencyUID: curID, ClassificationUID: clsWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(40)},
			{AccountHolder: -1, CurrencyUID: curID, ClassificationUID: clsCustodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(40)},
		},
	})
	require.NoError(t, err)

	// as_of before the entry's effective date: must be excluded.
	before, err := tbStore.TrialBalance(ctx, curID, backdated.AddDate(0, 0, -1))
	require.NoError(t, err)
	assert.Empty(t, before.Rows)
	assert.True(t, before.Balanced, "zero rows is trivially balanced")

	// as_of after the entry's effective date: must be included, even though
	// created_at (write time, "now") is also after — this specifically proves
	// the query filters on effective_at and not created_at.
	after, err := tbStore.TrialBalance(ctx, curID, backdated.AddDate(0, 0, 1))
	require.NoError(t, err)
	require.Len(t, after.Rows, 2)
	assert.True(t, after.TotalDebit.Equal(decimal.NewFromInt(40)))
}
