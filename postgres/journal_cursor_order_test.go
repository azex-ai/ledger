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

// TestListJournals_CursorOrderIsNewestFirst is H-m3's pin.
//
// docs/openapi.yaml described GET /journals as "descending id" while
// ListJournalsCursor was `WHERE id > cursor ORDER BY id ASC`, and the holder
// surface's own journal pagination (holder.sql page_journals) really was
// DESC. So the same API paginated its ledger reads in two directions and the
// documented one was the direction nothing implemented -- a consumer building
// a "most recent activity" list got the oldest page, with the cursor walking
// the wrong way.
//
// Reverting either half of the fix (the ORDER BY, or the `id <` predicate)
// makes this red: the first assertion catches the direction, the second
// catches a cursor that walks the wrong way.
func TestListJournals_CursorOrderIsNewestFirst(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	classStore := postgres.NewClassificationStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)
	queries := postgres.NewQueryStore(pool)

	cur, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{Code: "USDT-ORD", Name: "Order USDT", Exponent: 18})
	require.NoError(t, err)
	wallet, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "wallet_ord", Name: "Wallet Order", NormalSide: core.NormalSideDebit,
		BalanceRole: core.BalanceRoleAvailable,
	})
	require.NoError(t, err)
	sys, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "sys_ord", Name: "System Order", NormalSide: core.NormalSideCredit, IsSystem: true,
	})
	require.NoError(t, err)
	jt, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{Code: "jt_ord", Name: "Order JT"})
	require.NoError(t, err)

	holder := int64(7101)
	amount := decimal.NewFromInt(10)
	post := func(key string) *core.Journal {
		j, err := ledgerStore.PostJournal(ctx, core.JournalInput{
			JournalTypeUID: jt.UID,
			IdempotencyKey: postgrestest.UniqueKey(key),
			Entries: []core.EntryInput{
				{AccountHolder: holder, CurrencyUID: cur.UID, ClassificationUID: wallet.UID, EntryType: core.EntryTypeDebit, Amount: amount},
				{AccountHolder: -holder, CurrencyUID: cur.UID, ClassificationUID: sys.UID, EntryType: core.EntryTypeCredit, Amount: amount},
			},
			Source: "order_test",
		})
		require.NoError(t, err)
		return j
	}

	first := post("order-1")
	second := post("order-2")
	third := post("order-3")

	// Page one of size 1 must be the newest journal.
	page, next, err := queries.ListJournals(ctx, "", 1)
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, third.UID, page[0].UID, "the first page of a newest-first list is the most recent journal")
	require.NotEmpty(t, next, "a full page must hand back a cursor")

	// Page two must be strictly older, never a repeat and never a jump back
	// to the beginning.
	page2, next2, err := queries.ListJournals(ctx, next, 1)
	require.NoError(t, err)
	require.Len(t, page2, 1)
	assert.Equal(t, second.UID, page2[0].UID, "the cursor must walk backwards in time")
	require.NotEmpty(t, next2)

	page3, _, err := queries.ListJournals(ctx, next2, 1)
	require.NoError(t, err)
	require.Len(t, page3, 1)
	assert.Equal(t, first.UID, page3[0].UID)

	// And the same direction on the entry list a consumer pages alongside it.
	entries, _, err := queries.ListEntriesByAccount(ctx, holder, cur.UID, "", 2)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, third.UID, entries[0].JournalUID,
		"GET /entries must page in the same direction as GET /journals")
}

// TestListHolderHolds_IsBounded is H-m9's pin for the unbounded list.
//
// ListHolderHolds had no LIMIT and no cursor: it returned every outstanding
// hold the holder had, so one holder with a runaway number of active
// reservations produced an unbounded response body from an unbounded scan.
// api-contract.md §6 gives list endpoints a cursor shape; an endpoint with no
// bound at all is outside it.
//
// Reverting either the SQL LIMIT or the store's cursor handling makes this
// red: the first assertion catches a page larger than the limit, the second
// catches a cursor that does not advance.
func TestListHolderHolds_IsBounded(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	classStore := postgres.NewClassificationStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)
	reserver := postgres.NewReserverStore(pool, ledgerStore, postgres.NewVerifiedBalanceStore(pool, nil))

	cur, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{Code: "USDT-HOLD", Name: "Hold USDT", Exponent: 18})
	require.NoError(t, err)
	wallet, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "wallet_hold", Name: "Wallet Hold", NormalSide: core.NormalSideDebit,
		BalanceRole: core.BalanceRoleAvailable,
	})
	require.NoError(t, err)
	_, err = classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "locked_hold", Name: "Locked Hold", NormalSide: core.NormalSideDebit,
		BalanceRole: core.BalanceRoleLocked,
	})
	require.NoError(t, err)
	sys, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "sys_hold", Name: "System Hold", NormalSide: core.NormalSideCredit, IsSystem: true,
	})
	require.NoError(t, err)
	jt, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{Code: "jt_hold", Name: "Hold JT"})
	require.NoError(t, err)

	holder := int64(7202)
	// Fund the holder so the reservations below can be taken.
	_, err = ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jt.UID,
		IdempotencyKey: postgrestest.UniqueKey("hold-fund"),
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: cur.UID, ClassificationUID: wallet.UID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -holder, CurrencyUID: cur.UID, ClassificationUID: sys.UID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
		Source: "hold_test",
	})
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		_, err := reserver.Reserve(ctx, core.ReserveInput{
			AccountHolder:  holder,
			CurrencyUID:    cur.UID,
			Amount:         decimal.NewFromInt(1),
			IdempotencyKey: postgrestest.UniqueKey("hold-res"),
			ExpiresIn:      time.Hour,
		})
		require.NoError(t, err)
	}

	// A limit of 1 must return exactly one hold and a cursor.
	page, next, err := ledgerStore.ListHolderHolds(ctx, holder, "", 1)
	require.NoError(t, err)
	require.Len(t, page, 1, "the store must not return more rows than the requested limit")
	require.NotEmpty(t, next, "a full page must hand back a cursor")

	// The cursor must advance to a different hold, newest first.
	page2, _, err := ledgerStore.ListHolderHolds(ctx, holder, next, 1)
	require.NoError(t, err)
	require.Len(t, page2, 1)
	require.NotEqual(t, page[0].UID, page2[0].UID, "the cursor must advance instead of repeating the first page")
	require.True(t, page2[0].CreatedAt.Compare(page[0].CreatedAt) <= 0, "holds page newest first")

	// A garbage cursor is an explicit rejection, never a silent restart at
	// page one (working-agreements §3).
	_, _, err = ledgerStore.ListHolderHolds(ctx, holder, "not-a-cursor", 1)
	require.ErrorIs(t, err, core.ErrInvalidInput)
}
