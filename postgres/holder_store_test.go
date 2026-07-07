package postgres_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// Pins the holder transaction-view projection rules
// (docs/plans/2026-07-08-holder-scoped-wallet-surface.md §3.3):
// net aggregation over role-bearing classifications, zero-net rows invisible
// but cursor-advancing, FX one row per currency, reversal marking, journal-
// granularity cursor, and the kind_label fallback chain.

type holderFixture struct {
	pool    *pgxpool.Pool
	ledger  *postgres.LedgerStore
	holder  int64
	usdUID  string
	eurUID  string
	jtUID   string // journal type WITH display_label "Deposit"
	jtPlain string // journal type WITHOUT display_label
	wallet  string // available role
	locked  string // locked role
	pending string // pending role
	feeExp  string // role-less holder-side tracker
	system  string // system custodial
}

func seedHolderFixture(t *testing.T) (holderFixture, context.Context) {
	t.Helper()
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	ctx := context.Background()

	f := holderFixture{
		pool:    pool,
		ledger:  ledger,
		holder:  501,
		usdUID:  postgrestest.SeedCurrency(t, pool, "USD", "US Dollar"),
		eurUID:  postgrestest.SeedCurrency(t, pool, "EUR", "Euro"),
		jtUID:   postgrestest.SeedJournalType(t, pool, "ht_deposit", "Holder Deposit"),
		jtPlain: postgrestest.SeedJournalType(t, pool, "ht_misc", "Misc Operation"),
		wallet:  postgrestest.SeedClassificationWithRole(t, pool, "main_wallet", "Main Wallet", "debit", false, "available"),
		locked:  postgrestest.SeedClassificationWithRole(t, pool, "locked", "Locked", "debit", false, "locked"),
		pending: postgrestest.SeedClassificationWithRole(t, pool, "pending", "Pending", "credit", false, "pending"),
		feeExp:  postgrestest.SeedClassification(t, pool, "fee_expense", "Fee Expense", "debit", false),
		system:  postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true),
	}

	// Seed the deposit journal type's display label directly (the store
	// setter is exercised in TestHolderKindLabelFallback).
	_, err := pool.Exec(ctx, "UPDATE journal_types SET display_label = 'Deposit' WHERE code = 'ht_deposit'")
	require.NoError(t, err)

	return f, ctx
}

func (f holderFixture) post(t *testing.T, ctx context.Context, jtUID, key string, entries []core.EntryInput) *core.Journal {
	t.Helper()
	j, err := f.ledger.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtUID,
		IdempotencyKey: postgrestest.UniqueKey(key),
		Entries:        entries,
		Source:         "test",
	})
	require.NoError(t, err)
	return j
}

// deposit posts +amount to the holder's wallet against system custodial.
func (f holderFixture) deposit(t *testing.T, ctx context.Context, key string, amount int64) *core.Journal {
	t.Helper()
	return f.post(t, ctx, f.jtUID, key, []core.EntryInput{
		{AccountHolder: f.holder, CurrencyUID: f.usdUID, ClassificationUID: f.wallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(amount)},
		{AccountHolder: -f.holder, CurrencyUID: f.usdUID, ClassificationUID: f.system, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(amount)},
	})
}

func TestHolderTransactionsProjection(t *testing.T) {
	f, ctx := seedHolderFixture(t)

	// (1) Simple inbound deposit: +100 USD.
	jDeposit := f.deposit(t, ctx, "ht-dep", 100)

	// (2) Fee charge: wallet -5, role-less fee_expense +5 — the role filter
	// must keep this visible as out/5 (not net it to zero).
	f.post(t, ctx, f.jtPlain, "ht-fee", []core.EntryInput{
		{AccountHolder: f.holder, CurrencyUID: f.usdUID, ClassificationUID: f.wallet, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(5)},
		{AccountHolder: f.holder, CurrencyUID: f.usdUID, ClassificationUID: f.feeExp, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(5)},
	})

	// (3) Internal lock: wallet -30, locked +30 — zero net, must NOT appear.
	f.post(t, ctx, f.jtPlain, "ht-lock", []core.EntryInput{
		{AccountHolder: f.holder, CurrencyUID: f.usdUID, ClassificationUID: f.wallet, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(30)},
		{AccountHolder: f.holder, CurrencyUID: f.usdUID, ClassificationUID: f.locked, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(30)},
	})

	// (4) FX: sell 20 USD, buy 18 EUR — one journal, two currencies, one row each.
	f.post(t, ctx, f.jtPlain, "ht-fx", []core.EntryInput{
		{AccountHolder: f.holder, CurrencyUID: f.usdUID, ClassificationUID: f.wallet, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(20)},
		{AccountHolder: -f.holder, CurrencyUID: f.usdUID, ClassificationUID: f.system, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(20)},
		{AccountHolder: f.holder, CurrencyUID: f.eurUID, ClassificationUID: f.wallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(18)},
		{AccountHolder: -f.holder, CurrencyUID: f.eurUID, ClassificationUID: f.system, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(18)},
	})

	// (5) Reversal of the deposit.
	rev, err := f.ledger.ReverseJournal(ctx, jDeposit.UID, "test reversal")
	require.NoError(t, err)

	items, next, err := f.ledger.ListHolderTransactions(ctx, f.holder, "", 50)
	require.NoError(t, err)
	assert.Empty(t, next, "single page")

	// Expected rows newest-first: reversal(out 100), FX EUR(in 18) + FX USD(out 20),
	// fee(out 5), deposit(in 100). Lock journal invisible.
	require.Len(t, items, 5)

	assert.Equal(t, rev.UID, items[0].UID)
	assert.Equal(t, core.HolderTransactionOut, items[0].Direction)
	assert.True(t, decimal.NewFromInt(100).Equal(items[0].Amount))
	assert.Equal(t, jDeposit.UID, items[0].ReversalOfUID, "reversal row carries the original journal uid")

	// FX rows share one journal, ordered by currency code (EUR < USD).
	assert.Equal(t, items[1].UID, items[2].UID)
	assert.Equal(t, "EUR", items[1].CurrencyCode)
	assert.Equal(t, core.HolderTransactionIn, items[1].Direction)
	assert.True(t, decimal.NewFromInt(18).Equal(items[1].Amount))
	assert.Equal(t, "USD", items[2].CurrencyCode)
	assert.Equal(t, core.HolderTransactionOut, items[2].Direction)
	assert.True(t, decimal.NewFromInt(20).Equal(items[2].Amount))

	assert.Equal(t, core.HolderTransactionOut, items[3].Direction)
	assert.True(t, decimal.NewFromInt(5).Equal(items[3].Amount), "fee stays visible despite role-less counter-entry")

	assert.Equal(t, jDeposit.UID, items[4].UID)
	assert.Equal(t, core.HolderTransactionIn, items[4].Direction)
	assert.Equal(t, "Deposit", items[4].KindLabel, "journal type display_label wins")
	assert.Equal(t, "ht_deposit", items[4].Kind)
	assert.Empty(t, items[4].ReversalOfUID)

	// User-facing surface guard: no internal vocabulary leaks through fields.
	for _, it := range items {
		assert.NotContains(t, it.KindLabel, "debit")
		assert.NotContains(t, it.KindLabel, "credit")
	}
}

func TestHolderTransactionsCursor(t *testing.T) {
	f, ctx := seedHolderFixture(t)

	// 5 deposits, then a zero-net lock journal, then 2 more deposits.
	// Newest-first paging with limit 2 must walk everything exactly once and
	// the zero-net journal must consume a page slot (cursor advance) without
	// emitting a row.
	for i := range 5 {
		f.deposit(t, ctx, fmt.Sprintf("ht-c-%d", i), int64(10+i))
	}
	f.post(t, ctx, f.jtPlain, "ht-c-lock", []core.EntryInput{
		{AccountHolder: f.holder, CurrencyUID: f.usdUID, ClassificationUID: f.wallet, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(7)},
		{AccountHolder: f.holder, CurrencyUID: f.usdUID, ClassificationUID: f.locked, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(7)},
	})
	f.deposit(t, ctx, "ht-c-5", 50)
	f.deposit(t, ctx, "ht-c-6", 60)

	var got []core.HolderTransaction
	cursor := ""
	pages := 0
	for {
		items, next, err := f.ledger.ListHolderTransactions(ctx, f.holder, cursor, 2)
		require.NoError(t, err)
		got = append(got, items...)
		pages++
		require.LessOrEqual(t, pages, 10, "cursor must terminate")
		if next == "" {
			break
		}
		cursor = next
	}

	// 7 visible transactions (the lock journal is consumed but invisible).
	require.Len(t, got, 7)
	seen := map[string]bool{}
	for _, it := range got {
		assert.False(t, seen[it.UID+it.CurrencyCode], "no duplicates across pages")
		seen[it.UID+it.CurrencyCode] = true
	}
	// Newest first overall.
	assert.True(t, decimal.NewFromInt(60).Equal(got[0].Amount))
	assert.True(t, decimal.NewFromInt(10).Equal(got[6].Amount))
}

func TestHolderKindLabelFallback(t *testing.T) {
	f, ctx := seedHolderFixture(t)
	cls := postgres.NewClassificationStore(f.pool)

	// No labels anywhere -> journal type Name.
	j1 := f.post(t, ctx, f.jtPlain, "ht-lbl-1", []core.EntryInput{
		{AccountHolder: f.holder, CurrencyUID: f.usdUID, ClassificationUID: f.wallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(1)},
		{AccountHolder: -f.holder, CurrencyUID: f.usdUID, ClassificationUID: f.system, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(1)},
	})

	// Before any classification label exists: journal type Name fallback.
	items, _, err := f.ledger.ListHolderTransactions(ctx, f.holder, "", 10)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, j1.UID, items[0].UID)
	assert.Equal(t, "Misc Operation", items[0].KindLabel, "falls back to journal type name")

	// Classification label set (single-classification group) -> it wins over
	// the journal type label. Labels are query-time display config, so the
	// override applies retroactively to already-posted journals by design.
	require.NoError(t, cls.SetDisplayLabelIfEmpty(ctx, f.wallet, "Wallet move"))
	j2 := f.post(t, ctx, f.jtUID, "ht-lbl-2", []core.EntryInput{
		{AccountHolder: f.holder, CurrencyUID: f.usdUID, ClassificationUID: f.wallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(2)},
		{AccountHolder: -f.holder, CurrencyUID: f.usdUID, ClassificationUID: f.system, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(2)},
	})

	// The IfEmpty guard must not clobber the existing label.
	require.NoError(t, cls.SetDisplayLabelIfEmpty(ctx, f.wallet, "SHOULD NOT WIN"))

	items, _, err = f.ledger.ListHolderTransactions(ctx, f.holder, "", 10)
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.Equal(t, j2.UID, items[0].UID)
	assert.Equal(t, "Wallet move", items[0].KindLabel, "classification label overrides journal type label")
	assert.Equal(t, j1.UID, items[1].UID)
	assert.Equal(t, "Wallet move", items[1].KindLabel, "labels are query-time config — retroactive by design")
}

func TestHolderBalancesAndHolds(t *testing.T) {
	f, ctx := seedHolderFixture(t)
	reserver := postgres.NewReserverStore(f.pool, f.ledger)

	f.deposit(t, ctx, "ht-b-1", 100)
	// EUR deposit so two currencies exist.
	f.post(t, ctx, f.jtUID, "ht-b-eur", []core.EntryInput{
		{AccountHolder: f.holder, CurrencyUID: f.eurUID, ClassificationUID: f.wallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(40)},
		{AccountHolder: -f.holder, CurrencyUID: f.eurUID, ClassificationUID: f.system, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(40)},
	})

	res, err := reserver.Reserve(ctx, core.ReserveInput{
		AccountHolder:  f.holder,
		CurrencyUID:    f.usdUID,
		Amount:         decimal.NewFromInt(25),
		IdempotencyKey: postgrestest.UniqueKey("ht-hold"),
		ExpiresIn:      time.Hour,
	})
	require.NoError(t, err)

	balances, err := f.ledger.ListHolderBalances(ctx, f.holder, "")
	require.NoError(t, err)
	require.Len(t, balances, 2, "one row per currency, EUR first by code")

	assert.Equal(t, "EUR", balances[0].CurrencyCode)
	assert.True(t, decimal.NewFromInt(40).Equal(balances[0].Total))

	usd := balances[1]
	assert.Equal(t, "USD", usd.CurrencyCode)
	assert.True(t, decimal.NewFromInt(75).Equal(usd.Available), "available = 100 - 25 held")
	assert.True(t, decimal.NewFromInt(25).Equal(usd.Locked), "hold counts as locked")
	assert.True(t, usd.Total.Equal(usd.Available.Add(usd.Pending).Add(usd.Locked)), "total invariant")

	holds, err := f.ledger.ListHolderHolds(ctx, f.holder)
	require.NoError(t, err)
	require.Len(t, holds, 1)
	assert.Equal(t, res.UID, holds[0].UID)
	assert.True(t, decimal.NewFromInt(25).Equal(holds[0].Amount))
	assert.Equal(t, "USD", holds[0].CurrencyCode)

	// Single-currency filter.
	only, err := f.ledger.ListHolderBalances(ctx, f.holder, f.eurUID)
	require.NoError(t, err)
	require.Len(t, only, 1)
	assert.Equal(t, "EUR", only[0].CurrencyCode)
}
