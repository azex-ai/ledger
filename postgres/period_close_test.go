package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// I-15: before any period is closed, ActiveCloseLine is the zero Time and
// postings at any effective_at succeed.
func TestPeriodCloseStore_ActiveCloseLine_NeverClosed(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	store := postgres.NewPeriodCloseStore(pool)
	ctx := context.Background()

	line, err := store.ActiveCloseLine(ctx)
	require.NoError(t, err)
	assert.True(t, line.IsZero())
}

// I-15: closing a period rejects postings whose effective_at predates the
// close line, but a journal at or after the line still succeeds.
func TestLedgerStore_PostJournal_PeriodClosed_Rejected(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledgerStore := postgres.NewLedgerStore(pool)
	periodStore := postgres.NewPeriodCloseStore(pool)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	jtID := postgrestest.SeedJournalType(t, pool, "transfer", "Transfer")
	clsA := postgrestest.SeedClassification(t, pool, "wallet", "Wallet", "debit", false)
	clsB := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)

	closeBefore := time.Now().Truncate(time.Microsecond).AddDate(0, 0, -5)
	_, err := periodStore.ClosePeriod(ctx, core.ClosePeriodInput{
		CloseBefore: closeBefore,
		Note:        "month-end close",
		ActorID:     1,
	})
	require.NoError(t, err)

	// Rejected: effective_at is before the close line.
	_, err = ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jtID,
		IdempotencyKey: postgrestest.UniqueKey("period-closed-rejected"),
		EffectiveAt:    closeBefore.AddDate(0, 0, -1),
		Entries: []core.EntryInput{
			{AccountHolder: 1, CurrencyID: curID, ClassificationID: clsA, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(10)},
			{AccountHolder: -1, CurrencyID: curID, ClassificationID: clsB, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(10)},
		},
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, core.ErrPeriodClosed), "got: %v", err)

	// Accepted: effective_at is in the open period (now).
	_, err = ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jtID,
		IdempotencyKey: postgrestest.UniqueKey("period-closed-accepted"),
		Entries: []core.EntryInput{
			{AccountHolder: 1, CurrencyID: curID, ClassificationID: clsA, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(10)},
			{AccountHolder: -1, CurrencyID: curID, ClassificationID: clsB, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(10)},
		},
	})
	require.NoError(t, err)
}

// I-15: reopening a period (appending a row with an earlier close_before)
// makes previously-rejected effective dates postable again — latest-row-wins,
// with full history retained (append-only).
func TestPeriodCloseStore_Reopen_LatestRowWins(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledgerStore := postgres.NewLedgerStore(pool)
	periodStore := postgres.NewPeriodCloseStore(pool)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	jtID := postgrestest.SeedJournalType(t, pool, "transfer", "Transfer")
	clsA := postgrestest.SeedClassification(t, pool, "wallet", "Wallet", "debit", false)
	clsB := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)

	firstClose := time.Now().Truncate(time.Microsecond).AddDate(0, 0, -5)
	_, err := periodStore.ClosePeriod(ctx, core.ClosePeriodInput{CloseBefore: firstClose, ActorID: 1})
	require.NoError(t, err)

	backdated := firstClose.AddDate(0, 0, -2)

	_, err = ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jtID,
		IdempotencyKey: postgrestest.UniqueKey("reopen-before"),
		EffectiveAt:    backdated,
		Entries: []core.EntryInput{
			{AccountHolder: 1, CurrencyID: curID, ClassificationID: clsA, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(10)},
			{AccountHolder: -1, CurrencyID: curID, ClassificationID: clsB, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(10)},
		},
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, core.ErrPeriodClosed))

	// Reopen: append a row with an earlier close_before.
	reopenedClose := backdated.AddDate(0, 0, -1)
	_, err = periodStore.ClosePeriod(ctx, core.ClosePeriodInput{CloseBefore: reopenedClose, Note: "reopen", ActorID: 1})
	require.NoError(t, err)

	activeLine, err := periodStore.ActiveCloseLine(ctx)
	require.NoError(t, err)
	assert.True(t, activeLine.Equal(reopenedClose))

	// Now the same effective_at succeeds.
	_, err = ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jtID,
		IdempotencyKey: postgrestest.UniqueKey("reopen-after"),
		EffectiveAt:    backdated,
		Entries: []core.EntryInput{
			{AccountHolder: 1, CurrencyID: curID, ClassificationID: clsA, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(10)},
			{AccountHolder: -1, CurrencyID: curID, ClassificationID: clsB, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(10)},
		},
	})
	require.NoError(t, err)

	// Full history retained (append-only): both rows are listed.
	history, err := periodStore.ListPeriodCloses(ctx, 10)
	require.NoError(t, err)
	require.Len(t, history, 2)
}

// I-15: correcting a closed period is done by reversing at the current
// (open) date — the reversal must succeed even though the original journal's
// effective_at is before the close line.
func TestLedgerStore_ReverseJournal_AfterPeriodClose_PostsAtCurrentPeriod(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledgerStore := postgres.NewLedgerStore(pool)
	periodStore := postgres.NewPeriodCloseStore(pool)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	jtID := postgrestest.SeedJournalType(t, pool, "transfer", "Transfer")
	clsA := postgrestest.SeedClassification(t, pool, "wallet", "Wallet", "debit", false)
	clsB := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)

	backdated := time.Now().Truncate(time.Microsecond).AddDate(0, 0, -10)
	original, err := ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jtID,
		IdempotencyKey: postgrestest.UniqueKey("close-then-reverse-original"),
		EffectiveAt:    backdated,
		Entries: []core.EntryInput{
			{AccountHolder: 1, CurrencyID: curID, ClassificationID: clsA, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(30)},
			{AccountHolder: -1, CurrencyID: curID, ClassificationID: clsB, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(30)},
		},
	})
	require.NoError(t, err)

	// Close the period covering the original posting.
	_, err = periodStore.ClosePeriod(ctx, core.ClosePeriodInput{
		CloseBefore: time.Now().Truncate(time.Microsecond).AddDate(0, 0, -1),
		ActorID:     1,
	})
	require.NoError(t, err)

	// The reversal's effective_at defaults to now (open period) so it must
	// succeed even though `original`'s effective_at is now behind the close line.
	reversal, err := ledgerStore.ReverseJournal(ctx, original.ID, "correction after close")
	require.NoError(t, err)
	assert.True(t, reversal.EffectiveAt.After(original.EffectiveAt))
}
