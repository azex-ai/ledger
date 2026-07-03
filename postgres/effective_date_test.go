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

// I-14: journal_entries.effective_at == the parent journal's effective_at,
// and effective_at defaults to "now" (write time) when the caller doesn't
// supply one.
func TestLedgerStore_PostJournal_EffectiveAt_DefaultsToNow(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	store := postgres.NewLedgerStore(pool)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	jtID := postgrestest.SeedJournalType(t, pool, "transfer", "Transfer")
	clsA := postgrestest.SeedClassification(t, pool, "wallet", "Wallet", "debit", false)
	clsB := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)

	before := time.Now()
	input := core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("eff-default"),
		Entries: []core.EntryInput{
			{AccountHolder: 1, CurrencyUID: curID, ClassificationUID: clsA, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(50)},
			{AccountHolder: -1, CurrencyUID: curID, ClassificationUID: clsB, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(50)},
		},
	}

	journal, err := store.PostJournal(ctx, input)
	require.NoError(t, err)
	after := time.Now()

	assert.False(t, journal.EffectiveAt.Before(before.Add(-time.Second)))
	assert.False(t, journal.EffectiveAt.After(after.Add(time.Second)))

	entries, err := store.GetBalances(ctx, 1, curID)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.True(t, entries[0].Balance.Equal(decimal.NewFromInt(50)))
}

// I-14: a backdated effective_at is accepted (retroactive posting) and is
// persisted verbatim on both the journal and its entries.
func TestLedgerStore_PostJournal_EffectiveAt_Backdated(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	store := postgres.NewLedgerStore(pool)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	jtID := postgrestest.SeedJournalType(t, pool, "transfer", "Transfer")
	clsA := postgrestest.SeedClassification(t, pool, "wallet", "Wallet", "debit", false)
	clsB := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)

	backdated := time.Now().AddDate(0, 0, -10).Truncate(time.Microsecond)
	input := core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("eff-backdated"),
		EffectiveAt:    backdated,
		Entries: []core.EntryInput{
			{AccountHolder: 1, CurrencyUID: curID, ClassificationUID: clsA, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(50)},
			{AccountHolder: -1, CurrencyUID: curID, ClassificationUID: clsB, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(50)},
		},
	}

	journal, err := store.PostJournal(ctx, input)
	require.NoError(t, err)
	assert.True(t, journal.EffectiveAt.Equal(backdated), "want %s, got %s", backdated, journal.EffectiveAt)
	// created_at (write time) must still be "now", independent of effective_at.
	assert.True(t, journal.CreatedAt.After(backdated))

	// Real-time balance is unaffected by effective_at — it must reflect the
	// posting immediately regardless of how far back the business date is.
	bal, err := store.GetBalance(ctx, 1, curID, clsA)
	require.NoError(t, err)
	assert.True(t, bal.Equal(decimal.NewFromInt(50)))

	// I-14: every entry's effective_at must equal the parent journal's.
	queries := postgres.NewQueryStore(pool)
	_, entries, err := queries.GetJournal(ctx, journal.UID)
	require.NoError(t, err)
	for _, e := range entries {
		assert.True(t, e.EffectiveAt.Equal(backdated), "entry effective_at must equal journal effective_at")
	}
}

// I-14 / §1: effective_at more than the clock-skew tolerance in the future is
// rejected with ErrInvalidInput. Scheduled posting is a different feature.
func TestLedgerStore_PostJournal_EffectiveAt_RejectsFarFuture(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	store := postgres.NewLedgerStore(pool)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	jtID := postgrestest.SeedJournalType(t, pool, "transfer", "Transfer")
	clsA := postgrestest.SeedClassification(t, pool, "wallet", "Wallet", "debit", false)
	clsB := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)

	input := core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("eff-future"),
		EffectiveAt:    time.Now().Add(time.Hour),
		Entries: []core.EntryInput{
			{AccountHolder: 1, CurrencyUID: curID, ClassificationUID: clsA, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(50)},
			{AccountHolder: -1, CurrencyUID: curID, ClassificationUID: clsB, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(50)},
		},
	}

	_, err := store.PostJournal(ctx, input)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)
}

// I-14: reversal journals default effective_at to "now" — they do NOT
// inherit the original journal's effective_at. This is the standard
// close-then-correct pattern (fix lands in the current open period).
func TestLedgerStore_ReverseJournal_EffectiveAt_DoesNotInheritOriginal(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	store := postgres.NewLedgerStore(pool)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	jtID := postgrestest.SeedJournalType(t, pool, "transfer", "Transfer")
	clsA := postgrestest.SeedClassification(t, pool, "wallet", "Wallet", "debit", false)
	clsB := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)

	backdated := time.Now().AddDate(0, -1, 0)
	original, err := store.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("eff-reverse-original"),
		EffectiveAt:    backdated,
		Entries: []core.EntryInput{
			{AccountHolder: 1, CurrencyUID: curID, ClassificationUID: clsA, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(50)},
			{AccountHolder: -1, CurrencyUID: curID, ClassificationUID: clsB, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(50)},
		},
	})
	require.NoError(t, err)

	before := time.Now()
	reversal, err := store.ReverseJournal(ctx, original.UID, "correction")
	require.NoError(t, err)
	after := time.Now()

	assert.False(t, reversal.EffectiveAt.Before(before.Add(-time.Second)), "reversal effective_at should be ~now, not inherited from original")
	assert.False(t, reversal.EffectiveAt.After(after.Add(time.Second)))
	assert.True(t, reversal.EffectiveAt.After(original.EffectiveAt))
}

// I-14 / §1: as-of balance queries (ListBalancesAt, used by daily snapshots
// and balance trends) key off effective_at, not created_at — a backdated
// entry posted "today" must be visible in a snapshot cutoff between its
// effective_at and its created_at.
func TestRollupAdapter_ListBalancesAt_UsesEffectiveAt(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	store := postgres.NewLedgerStore(pool)
	adapter := postgres.NewRollupAdapter(pool)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	jtID := postgrestest.SeedJournalType(t, pool, "transfer", "Transfer")
	clsA := postgrestest.SeedClassification(t, pool, "wallet", "Wallet", "debit", false)
	clsB := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)

	// Posted "now" (created_at ~= now) but attributed to 10 days ago.
	backdated := time.Now().AddDate(0, 0, -10)
	_, err := store.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("eff-asof"),
		EffectiveAt:    backdated,
		Entries: []core.EntryInput{
			{AccountHolder: 42, CurrencyUID: curID, ClassificationUID: clsA, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(75)},
			{AccountHolder: -42, CurrencyUID: curID, ClassificationUID: clsB, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(75)},
		},
	})
	require.NoError(t, err)

	// Cutoff is between effective_at (10 days ago) and created_at (now): a
	// created_at-keyed query would MISS this entry; an effective_at-keyed
	// query must include it.
	cutoff := time.Now().AddDate(0, 0, -5)
	balances, err := adapter.ListBalancesAt(ctx, cutoff)
	require.NoError(t, err)

	var found bool
	for _, b := range balances {
		if b.AccountHolder == 42 && b.CurrencyUID == curID && b.ClassificationUID == clsA {
			found = true
			assert.True(t, b.Balance.Equal(decimal.NewFromInt(75)))
		}
	}
	assert.True(t, found, "backdated entry must be visible in as-of balance keyed on effective_at")
}
