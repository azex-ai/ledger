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
	"github.com/azex-ai/ledger/presets"
)

func TestAudit_ListJournalsByAccount_OrderedByID(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	classStore := postgres.NewClassificationStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)
	auditStore := postgres.NewAuditStore(pool)

	usdt, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{Code: "USDT-AUDIT", Name: "Audit USDT", Exponent: 18})
	require.NoError(t, err)

	wallet, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "wallet_audit", Name: "Wallet Audit", NormalSide: core.NormalSideDebit,
	})
	require.NoError(t, err)

	sys, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "sys_audit", Name: "System Audit", NormalSide: core.NormalSideCredit, IsSystem: true,
	})
	require.NoError(t, err)

	jt, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{Code: "jt_audit", Name: "Audit JT"})
	require.NoError(t, err)

	userID := int64(7001)
	amt := decimal.NewFromInt(100)

	// Post two journals.
	j1, err := ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jt.UID,
		IdempotencyKey: postgrestest.UniqueKey("audit-j1"),
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyUID: usdt.UID, ClassificationUID: wallet.UID, EntryType: core.EntryTypeDebit, Amount: amt},
			{AccountHolder: -userID, CurrencyUID: usdt.UID, ClassificationUID: sys.UID, EntryType: core.EntryTypeCredit, Amount: amt},
		},
		Source: "audit_test",
	})
	require.NoError(t, err)

	j2, err := ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jt.UID,
		IdempotencyKey: postgrestest.UniqueKey("audit-j2"),
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyUID: usdt.UID, ClassificationUID: wallet.UID, EntryType: core.EntryTypeDebit, Amount: amt},
			{AccountHolder: -userID, CurrencyUID: usdt.UID, ClassificationUID: sys.UID, EntryType: core.EntryTypeCredit, Amount: amt},
		},
		Source: "audit_test",
	})
	require.NoError(t, err)

	// List journals by account.
	journals, _, err := auditStore.ListJournalsByAccount(ctx, core.AuditFilter{
		AccountHolder:     userID,
		CurrencyUID:       usdt.UID,
		ClassificationUID: wallet.UID,
		Limit:             10,
	})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(journals), 2, "expected at least 2 journals")

	// Verify ordering: CreatedAt should be non-decreasing (keyset order).
	for i := 1; i < len(journals); i++ {
		assert.False(t, journals[i-1].CreatedAt.After(journals[i].CreatedAt), "journals should be ordered oldest first")
	}

	// Both posted journals should be in the result.
	uids := make(map[string]bool)
	for _, j := range journals {
		uids[j.UID] = true
	}
	assert.True(t, uids[j1.UID], "j1 should appear in list")
	assert.True(t, uids[j2.UID], "j2 should appear in list")
}

func TestAudit_ListEntriesByJournal(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	classStore := postgres.NewClassificationStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)
	auditStore := postgres.NewAuditStore(pool)

	usdt, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{Code: "USDT-ENT", Name: "Entry USDT", Exponent: 18})
	require.NoError(t, err)

	wallet, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "wallet_ent", Name: "Wallet Ent", NormalSide: core.NormalSideDebit,
	})
	require.NoError(t, err)

	sys, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "sys_ent", Name: "System Ent", NormalSide: core.NormalSideCredit, IsSystem: true,
	})
	require.NoError(t, err)

	jt, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{Code: "jt_ent", Name: "Entry JT"})
	require.NoError(t, err)

	userID := int64(7002)
	amt := decimal.NewFromInt(200)

	j, err := ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jt.UID,
		IdempotencyKey: postgrestest.UniqueKey("entries-j"),
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyUID: usdt.UID, ClassificationUID: wallet.UID, EntryType: core.EntryTypeDebit, Amount: amt},
			{AccountHolder: -userID, CurrencyUID: usdt.UID, ClassificationUID: sys.UID, EntryType: core.EntryTypeCredit, Amount: amt},
		},
		Source: "entry_test",
	})
	require.NoError(t, err)

	entries, err := auditStore.ListEntriesByJournal(ctx, j.UID)
	require.NoError(t, err)
	assert.Len(t, entries, 2, "expected 2 entries (debit + credit)")

	// Both legs must reference the parent journal.
	if len(entries) == 2 {
		assert.Equal(t, j.UID, entries[0].JournalUID)
		assert.Equal(t, j.UID, entries[1].JournalUID)
	}
}

func TestAudit_ListJournalsByTimeRange(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	classStore := postgres.NewClassificationStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)
	auditStore := postgres.NewAuditStore(pool)

	usdt, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{Code: "USDT-TR", Name: "TimeRange USDT", Exponent: 18})
	require.NoError(t, err)

	wallet, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "wallet_tr", Name: "Wallet TR", NormalSide: core.NormalSideDebit,
	})
	require.NoError(t, err)

	sys, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "sys_tr", Name: "System TR", NormalSide: core.NormalSideCredit, IsSystem: true,
	})
	require.NoError(t, err)

	jt, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{Code: "jt_tr", Name: "TR JT"})
	require.NoError(t, err)

	userID := int64(7003)
	amt := decimal.NewFromInt(50)

	before := time.Now().UTC()

	j, err := ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jt.UID,
		IdempotencyKey: postgrestest.UniqueKey("tr-j"),
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyUID: usdt.UID, ClassificationUID: wallet.UID, EntryType: core.EntryTypeDebit, Amount: amt},
			{AccountHolder: -userID, CurrencyUID: usdt.UID, ClassificationUID: sys.UID, EntryType: core.EntryTypeCredit, Amount: amt},
		},
		Source: "timerange_test",
	})
	require.NoError(t, err)

	after := time.Now().UTC().Add(time.Second)

	// Query within the time range that brackets the journal creation.
	journals, _, err := auditStore.ListJournalsByTimeRange(ctx, core.AuditFilter{
		Since: before.Add(-time.Second),
		Until: after,
		Limit: 50,
	})
	require.NoError(t, err)

	uids := make(map[string]bool)
	for _, jj := range journals {
		uids[jj.UID] = true
	}
	assert.True(t, uids[j.UID], "journal should appear in time range query")

	// Query outside the range: should not find the journal.
	notFound, _, err := auditStore.ListJournalsByTimeRange(ctx, core.AuditFilter{
		Since: after.Add(time.Hour),
		Until: after.Add(2 * time.Hour),
		Limit: 50,
	})
	require.NoError(t, err)
	assert.NotContains(t, journalUIDs(notFound), j.UID, "journal should not appear outside its time range")
}

func TestAudit_TraceBooking(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	bookingStore := postgres.NewBookingStore(pool)
	classStore := postgres.NewClassificationStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)
	auditStore := postgres.NewAuditStore(pool)

	usdt, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{Code: "USDT-TRACE", Name: "Trace USDT", Exponent: 18})
	require.NoError(t, err)

	// Install deposit lifecycle so we can create a booking.
	depClass, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code:       "deposit_trace",
		Name:       "Deposit Trace",
		NormalSide: core.NormalSideCredit,
		Lifecycle:  presets.DepositLifecycle,
	})
	require.NoError(t, err)

	userID := int64(8001)

	booking, err := bookingStore.CreateBooking(ctx, core.CreateBookingInput{
		ClassificationCode: depClass.Code,
		AccountHolder:      userID,
		CurrencyUID:        usdt.UID,
		Amount:             decimal.NewFromInt(300),
		IdempotencyKey:     postgrestest.UniqueKey("trace-booking"),
		ChannelName:        "manual",
	})
	require.NoError(t, err)

	// Transition the booking to generate an event (CreateBooking alone does not emit events).
	// Deposit lifecycle: pending -> confirming -> confirmed | failed | expired
	event, err := bookingStore.Transition(ctx, core.TransitionInput{
		BookingUID: booking.UID,
		ToStatus:   "confirming",
		Amount:     decimal.NewFromInt(300),
		ActorID:    userID,
	})
	require.NoError(t, err)
	require.NotNil(t, event)

	trace, err := auditStore.TraceBooking(ctx, booking.UID)
	require.NoError(t, err)
	require.NotNil(t, trace)

	assert.Equal(t, booking.UID, trace.Booking.UID)
	// After one transition, there should be exactly one event.
	assert.GreaterOrEqual(t, len(trace.Events), 1, "expected at least one event")
	// Events should include the transition we just made.
	assert.Equal(t, event.UID, trace.Events[0].UID)
}

func TestAudit_TraceBooking_NotFound(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	auditStore := postgres.NewAuditStore(pool)

	_, err := auditStore.TraceBooking(ctx, "00000000-0000-7000-8000-000999999999")
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestAudit_ListReversals(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	classStore := postgres.NewClassificationStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)
	auditStore := postgres.NewAuditStore(pool)

	usdt, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{Code: "USDT-REV", Name: "Reversal USDT", Exponent: 18})
	require.NoError(t, err)

	wallet, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "wallet_rev", Name: "Wallet Rev", NormalSide: core.NormalSideDebit,
	})
	require.NoError(t, err)

	sys, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "sys_rev", Name: "System Rev", NormalSide: core.NormalSideCredit, IsSystem: true,
	})
	require.NoError(t, err)

	jt, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{Code: "jt_rev", Name: "Rev JT"})
	require.NoError(t, err)

	userID := int64(9002)
	amt := decimal.NewFromInt(100)

	// Post original journal.
	original, err := ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jt.UID,
		IdempotencyKey: postgrestest.UniqueKey("rev-orig"),
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyUID: usdt.UID, ClassificationUID: wallet.UID, EntryType: core.EntryTypeDebit, Amount: amt},
			{AccountHolder: -userID, CurrencyUID: usdt.UID, ClassificationUID: sys.UID, EntryType: core.EntryTypeCredit, Amount: amt},
		},
		Source: "reversal_test",
	})
	require.NoError(t, err)

	// Reverse the original.
	reversal, err := ledgerStore.ReverseJournal(ctx, original.UID, "test reversal")
	require.NoError(t, err)

	// ListReversals from the original should return both.
	chain, err := auditStore.ListReversals(ctx, original.UID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(chain), 2, "expected at least 2 journals in chain")

	chainUIDs := journalUIDs(chain)
	assert.Contains(t, chainUIDs, original.UID, "chain should include original")
	assert.Contains(t, chainUIDs, reversal.UID, "chain should include reversal")
}

// journalUIDs extracts the uid slice from a []core.Journal for assertion convenience.
func journalUIDs(journals []core.Journal) []string {
	uids := make([]string, len(journals))
	for i, j := range journals {
		uids[i] = j.UID
	}
	return uids
}
