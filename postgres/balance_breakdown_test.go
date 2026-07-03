package postgres_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// breakdownFixture seeds one holder with balances across every balance role:
//
//	main_wallet (available): +100 deposit, −30 lock, −5 fee  = 65
//	locked      (locked)   : +30 lock                        = 30
//	pending     (pending)  : +40 unconfirmed deposit          = 40
//	fee_expense (role '')  : +5 — must NOT appear anywhere
type breakdownFixture struct {
	pool     *pgxpool.Pool
	ledger   *postgres.LedgerStore
	reserver *postgres.ReserverStore
	holder   int64
	curUID   string
}

func seedBreakdownFixture(t *testing.T) (breakdownFixture, context.Context) {
	t.Helper()
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	reserver := postgres.NewReserverStore(pool, ledger)
	ctx := context.Background()

	curUID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	jt := postgrestest.SeedJournalType(t, pool, "bd_test", "Breakdown Test")
	wallet := postgrestest.SeedClassificationWithRole(t, pool, "main_wallet", "Main Wallet", "debit", false, "available")
	lockedCls := postgrestest.SeedClassificationWithRole(t, pool, "locked", "Locked", "debit", false, "locked")
	pendingCls := postgrestest.SeedClassificationWithRole(t, pool, "pending", "Pending", "credit", false, "pending")
	feeExpense := postgrestest.SeedClassification(t, pool, "fee_expense", "Fee Expense", "debit", false)
	custodial := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)
	suspense := postgrestest.SeedClassification(t, pool, "suspense", "Suspense", "debit", true)

	const holder = int64(77)
	post := func(key string, entries []core.EntryInput) {
		t.Helper()
		_, err := ledger.PostJournal(ctx, core.JournalInput{
			JournalTypeUID: jt,
			IdempotencyKey: postgrestest.UniqueKey(key),
			Entries:        entries,
			Source:         "test",
		})
		require.NoError(t, err)
	}

	// Confirmed deposit: main_wallet +100.
	post("bd-deposit", []core.EntryInput{
		{AccountHolder: holder, CurrencyUID: curUID, ClassificationUID: wallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
		{AccountHolder: -holder, CurrencyUID: curUID, ClassificationUID: custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
	})
	// Unconfirmed deposit: pending +40.
	post("bd-pending", []core.EntryInput{
		{AccountHolder: -holder, CurrencyUID: curUID, ClassificationUID: suspense, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(40)},
		{AccountHolder: holder, CurrencyUID: curUID, ClassificationUID: pendingCls, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(40)},
	})
	// Lock 30 for a withdrawal: main_wallet −30, locked +30.
	post("bd-lock", []core.EntryInput{
		{AccountHolder: holder, CurrencyUID: curUID, ClassificationUID: wallet, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(30)},
		{AccountHolder: holder, CurrencyUID: curUID, ClassificationUID: lockedCls, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(30)},
	})
	// Fee paid: fee_expense +5 (role-less), main_wallet −5.
	post("bd-fee", []core.EntryInput{
		{AccountHolder: holder, CurrencyUID: curUID, ClassificationUID: feeExpense, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(5)},
		{AccountHolder: holder, CurrencyUID: curUID, ClassificationUID: wallet, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(5)},
	})

	return breakdownFixture{pool: pool, ledger: ledger, reserver: reserver, holder: holder, curUID: curUID}, ctx
}

// Pins the BalanceBreakdown aggregation contract:
// available = Σ role=available − held, locked = Σ role=locked + held,
// pending = Σ role=pending, total = available + locked + pending.
// Role-less classifications (fee_expense) appear nowhere.
func TestGetBalanceBreakdown_RolesPlusHolds(t *testing.T) {
	fx, ctx := seedBreakdownFixture(t)

	// Hold 20 via a reservation.
	_, err := fx.reserver.Reserve(ctx, core.ReserveInput{
		AccountHolder:  fx.holder,
		CurrencyUID:    fx.curUID,
		Amount:         decimal.NewFromInt(20),
		IdempotencyKey: postgrestest.UniqueKey("bd-hold"),
	})
	require.NoError(t, err)

	b, err := fx.ledger.GetBalanceBreakdown(ctx, fx.holder, fx.curUID)
	require.NoError(t, err)

	assert.True(t, b.Available.Equal(decimal.NewFromInt(45)), "available: got %s", b.Available)
	assert.True(t, b.Pending.Equal(decimal.NewFromInt(40)), "pending: got %s", b.Pending)
	assert.True(t, b.Locked.Equal(decimal.NewFromInt(50)), "locked: got %s", b.Locked)
	assert.True(t, b.Total.Equal(decimal.NewFromInt(135)), "total: got %s", b.Total)
	// The identity is structural, not coincidental.
	assert.True(t, b.Total.Equal(b.Available.Add(b.Locked).Add(b.Pending)))
}

// Pins the revised I-11 basis: Reserve draws on role=available balances ONLY.
// Pending deposits, journal-locked funds, and role-less classifications are
// not reservable.
func TestReserve_AvailableBasisExcludesPendingLockedAndRoleless(t *testing.T) {
	fx, ctx := seedBreakdownFixture(t)

	// available base is 65 (100 − 30 locked − 5 fee). Total across all
	// classifications would be 65+30+40+5 = 140 — the old (buggy) basis.
	_, err := fx.reserver.Reserve(ctx, core.ReserveInput{
		AccountHolder:  fx.holder,
		CurrencyUID:    fx.curUID,
		Amount:         decimal.NewFromInt(66),
		IdempotencyKey: postgrestest.UniqueKey("bd-over"),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInsufficientBalance)

	// Exactly the available base is reservable.
	_, err = fx.reserver.Reserve(ctx, core.ReserveInput{
		AccountHolder:  fx.holder,
		CurrencyUID:    fx.curUID,
		Amount:         decimal.NewFromInt(65),
		IdempotencyKey: postgrestest.UniqueKey("bd-exact"),
	})
	require.NoError(t, err)
}

// A holder whose only funds are unconfirmed (pending) deposits cannot reserve
// anything — the exact double-spend M2 flagged: reserve against a deposit
// that later gets cancelled.
func TestReserve_PendingOnlyBalanceNotReservable(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledger := postgres.NewLedgerStore(pool)
	reserver := postgres.NewReserverStore(pool, ledger)
	ctx := context.Background()

	curUID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	jt := postgrestest.SeedJournalType(t, pool, "bd_pending_only", "Pending Only")
	pendingCls := postgrestest.SeedClassificationWithRole(t, pool, "pending", "Pending", "credit", false, "pending")
	suspense := postgrestest.SeedClassification(t, pool, "suspense", "Suspense", "debit", true)

	_, err := ledger.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jt,
		IdempotencyKey: postgrestest.UniqueKey("bd-pending-only"),
		Entries: []core.EntryInput{
			{AccountHolder: -55, CurrencyUID: curUID, ClassificationUID: suspense, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: 55, CurrencyUID: curUID, ClassificationUID: pendingCls, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
		Source: "test",
	})
	require.NoError(t, err)

	_, err = reserver.Reserve(ctx, core.ReserveInput{
		AccountHolder:  55,
		CurrencyUID:    curUID,
		Amount:         decimal.NewFromInt(1),
		IdempotencyKey: postgrestest.UniqueKey("bd-pending-reserve"),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInsufficientBalance)

	b, err := ledger.GetBalanceBreakdown(ctx, 55, curUID)
	require.NoError(t, err)
	assert.True(t, b.Available.IsZero())
	assert.True(t, b.Pending.Equal(decimal.NewFromInt(100)))
	assert.True(t, b.Total.Equal(decimal.NewFromInt(100)))
}
