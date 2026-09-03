package ledger_test

// Pin for M-3 (docs/audits/2026-09-03-independent-review/money-out.md): the
// availability BASE of the gated withdrawal path -- the one I-49 names as
// deciding how much money may leave -- draws on `balance_role='available'`
// classifications only.
//
// I-11 has said that since migration 032, and six pins were listed under it.
// None of them covered this path. The two whose names come closest
// (postgres.TestReserve_AvailableBasisExcludesPendingLockedAndRoleless,
// postgres.TestReserve_PendingOnlyBalanceNotReservable) run the UNGATED
// Reserve, which sums roles in sumBalancesByRoleWithQueries; I-49 replaced
// the gated path's base with two entirely new sums
// (requireVerifiedAvailableBalance outside the transaction,
// sumAvailableFromEntriesWithQueries under the lock) and each of those
// re-derives the role filter itself. The independent reviewer deleted BOTH
// filters, watched a holder whose only funds were an unconfirmed
// (role=pending) deposit withdraw them through the gate, and watched all six
// pins stay green.
//
// So this file exists to be the pin that goes red for that. It asserts three
// things about one fixture, in the order that makes a failure legible:
//
//  1. the gate resolves at all (the fixture is signed and verifiable -- a
//     pin that passed because everything is rejected would prove nothing);
//  2. the available-role base, and nothing else, is reservable;
//  3. a holder with pending-only funds can reserve nothing.
//
// Everything goes through ledger.New. postgres.NewReserverStore and
// postgres.NewVerifiedBalanceStore are never named: the gate a consumer
// actually reaches is the one behind the facade, and the filters being
// pinned live two constructors below it.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
)

// roleBasisFixture is one holder carrying money in four shapes: an
// available-role balance (reservable), a pending-role balance (an
// unconfirmed deposit), a locked-role balance (journal-locked funds) and a
// memo one (an expense classification -- `memo` is what a non-liability
// bucket must declare since I-25's explicit-role rule; the reviewer's
// "role-less" case). Only the first is part of the availability base.
type roleBasisFixture struct {
	holder      int64
	currencyUID string
	walletUID   string
}

func seedRoleBasisFixture(t *testing.T, ctx context.Context, svc *ledger.Service, holder int64) roleBasisFixture {
	t.Helper()
	suffix := time.Now().UnixNano()

	cur, err := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{
		Code: fmt.Sprintf("RBU_%d", suffix), Name: "Role Basis Unit", Exponent: 18,
	})
	require.NoError(t, err)

	mk := func(code, name string, side core.NormalSide, role core.BalanceRole, system bool) string {
		cls, err := svc.Classifications().CreateClassification(ctx, core.ClassificationInput{
			Code: fmt.Sprintf("%s_%d", code, suffix), Name: name,
			NormalSide: side, BalanceRole: role, IsSystem: system,
		})
		require.NoError(t, err)
		return cls.UID
	}
	wallet := mk("rb_main", "Role Basis Main", core.NormalSideDebit, core.BalanceRoleAvailable, false)
	pending := mk("rb_pending", "Role Basis Pending", core.NormalSideCredit, core.BalanceRolePending, false)
	locked := mk("rb_locked", "Role Basis Locked", core.NormalSideDebit, core.BalanceRoleLocked, false)
	memo := mk("rb_expense", "Role Basis Expense", core.NormalSideDebit, core.BalanceRoleMemo, false)
	custodial := mk("rb_custodial", "Role Basis Custodial", core.NormalSideCredit, "", true)
	suspense := mk("rb_suspense", "Role Basis Suspense", core.NormalSideDebit, "", true)

	jt, err := svc.JournalTypes().CreateJournalType(ctx, core.JournalTypeInput{
		Code: fmt.Sprintf("rb_fund_%d", suffix), Name: "Role Basis Fund",
	})
	require.NoError(t, err)

	post := func(key string, entries []core.EntryInput) {
		t.Helper()
		_, err := svc.JournalWriter().PostJournal(ctx, core.JournalInput{
			JournalTypeUID: jt.UID,
			IdempotencyKey: postgrestest.UniqueKey(key),
			Source:         "gated-reserve-role-basis-pin",
			ActorID:        holder,
			Entries:        entries,
		})
		require.NoError(t, err)
	}
	system := core.SystemAccountHolder(holder)

	// Confirmed deposit: the only reservable money in this fixture.
	post("rb-available", []core.EntryInput{
		{AccountHolder: holder, CurrencyUID: cur.UID, ClassificationUID: wallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(10)},
		{AccountHolder: system, CurrencyUID: cur.UID, ClassificationUID: custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(10)},
	})
	// Unconfirmed deposit: 1000 sitting on role=pending. This is the money
	// the reviewer withdrew with the filters removed.
	post("rb-pending", []core.EntryInput{
		{AccountHolder: system, CurrencyUID: cur.UID, ClassificationUID: suspense, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(1000)},
		{AccountHolder: holder, CurrencyUID: cur.UID, ClassificationUID: pending, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(1000)},
	})
	// Journal-locked funds and a role-less expense balance, both funded from
	// the custodial side so the available wallet keeps its 10.
	post("rb-locked", []core.EntryInput{
		{AccountHolder: holder, CurrencyUID: cur.UID, ClassificationUID: locked, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(500)},
		{AccountHolder: system, CurrencyUID: cur.UID, ClassificationUID: custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(500)},
	})
	post("rb-roleless", []core.EntryInput{
		{AccountHolder: holder, CurrencyUID: cur.UID, ClassificationUID: memo, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(300)},
		{AccountHolder: system, CurrencyUID: cur.UID, ClassificationUID: custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(300)},
	})

	return roleBasisFixture{holder: holder, currencyUID: cur.UID, walletUID: wallet}
}

// TestService_GatedReserve_AvailableRoleIsTheOnlyBasis is I-11's pin on the
// gated path. Deleting BOTH role filters in postgres.ReserverStore
// (requireVerifiedAvailableBalance's and sumAvailableFromEntriesWithQueries')
// turns the 1810 of pending + locked + memo money in this fixture into part
// of the base, and case 2 below starts passing where it must fail --
// measured, both cases in this file go red on exactly that assertion.
//
// Deleting only one of them does not, and that is the correct outcome rather
// than a hole in the pin: I-49 combines the two figures with min(), so the
// surviving filter caps the inflated one and no money moves. What this pin
// asserts is the property that matters -- money leaving on a non-available
// basis -- not the presence of two particular lines.
func TestService_GatedReserve_AvailableRoleIsTheOnlyBasis(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	attestor, verifier := newTestAttestor(t, "gated-reserve-role-basis-key")
	svc, err := ledger.New(pool, ledger.WithAttestor(attestor, verifier))
	require.NoError(t, err)

	fx := seedRoleBasisFixture(t, ctx, svc, 9401)

	gatedReserve := func(amount int64, key string) error {
		_, err := svc.Reserver().Reserve(ctx, core.ReserveInput{
			AccountHolder:          fx.holder,
			CurrencyUID:            fx.currencyUID,
			Amount:                 decimal.NewFromInt(amount),
			IdempotencyKey:         postgrestest.UniqueKey(key),
			ExpiresIn:              10 * time.Minute,
			RequireVerifiedBalance: true,
		})
		return err
	}

	// 1. Sanity: the gate resolves this holder's available dimension rather
	//    than refusing it. Without this, every assertion below would be
	//    satisfied by a gate that rejects everything for an unrelated reason
	//    (no verifier wired, an unsigned journal, a poisoned dimension).
	balance, err := svc.VerifiedBalanceReader().VerifiedBalance(ctx, fx.holder, fx.currencyUID, fx.walletUID)
	require.NoError(t, err, "sanity: the fixture must be verifiable, or the pin below proves nothing")
	require.True(t, balance.Equal(decimal.NewFromInt(10)), "expected 10 on the available dimension, got %s", balance)

	// 2. The pin. 1810 exists on this holder in this currency; 10 of it is
	//    reservable. A gate that has lost either role filter authorizes 900
	//    here -- an unconfirmed deposit paying out as if it had settled.
	err = gatedReserve(900, "rb-over")
	require.Error(t, err,
		"gated Reserve must not draw on pending/locked/memo balances: I-11's availability base is role=available only")
	assert.ErrorIs(t, err, core.ErrInsufficientBalance)

	// And not merely "less than everything": 11 is one unit past the
	// available base and must fail for the same reason.
	err = gatedReserve(11, "rb-over-by-one")
	require.Error(t, err, "the base must be exactly the available-role sum, not some larger figure")
	assert.ErrorIs(t, err, core.ErrInsufficientBalance)

	// 3. Exactly the available base is reservable -- the other half of "the
	//    base is 10", and what keeps case 2 from passing on a gate that
	//    refuses every amount.
	require.NoError(t, gatedReserve(10, "rb-exact"),
		"the available-role balance itself must remain reservable through the gate")
}

// TestService_GatedReserve_PendingOnlyHolderCanReserveNothing is the
// reviewer's own repro, kept as its own case because it is the shape that
// matters in production: a deposit that has been seen but not confirmed must
// not be withdrawable, and a holder with nothing else has no available
// dimension for the gate to iterate at all -- the code path where "the loop
// found no rows" and "the loop skipped every row" produce the same zero.
func TestService_GatedReserve_PendingOnlyHolderCanReserveNothing(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	attestor, verifier := newTestAttestor(t, "gated-reserve-pending-only-key")
	svc, err := ledger.New(pool, ledger.WithAttestor(attestor, verifier))
	require.NoError(t, err)

	suffix := time.Now().UnixNano()
	cur, err := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{
		Code: fmt.Sprintf("RBP_%d", suffix), Name: "Role Basis Pending Unit", Exponent: 18,
	})
	require.NoError(t, err)
	pending, err := svc.Classifications().CreateClassification(ctx, core.ClassificationInput{
		Code: fmt.Sprintf("rbp_pending_%d", suffix), Name: "Pending Only",
		NormalSide: core.NormalSideCredit, BalanceRole: core.BalanceRolePending,
	})
	require.NoError(t, err)
	suspense, err := svc.Classifications().CreateClassification(ctx, core.ClassificationInput{
		Code: fmt.Sprintf("rbp_suspense_%d", suffix), Name: "Pending Only Suspense",
		NormalSide: core.NormalSideDebit, IsSystem: true,
	})
	require.NoError(t, err)
	jt, err := svc.JournalTypes().CreateJournalType(ctx, core.JournalTypeInput{
		Code: fmt.Sprintf("rbp_jt_%d", suffix), Name: "Pending Only Fund",
	})
	require.NoError(t, err)

	const holder = int64(9402)
	_, err = svc.JournalWriter().PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jt.UID,
		IdempotencyKey: postgrestest.UniqueKey("rbp-pending"),
		Source:         "gated-reserve-role-basis-pin",
		ActorID:        holder,
		Entries: []core.EntryInput{
			{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: cur.UID, ClassificationUID: suspense.UID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(1000)},
			{AccountHolder: holder, CurrencyUID: cur.UID, ClassificationUID: pending.UID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(1000)},
		},
	})
	require.NoError(t, err)

	// The signed, verifiable 1000 is real money in the ledger -- it is simply
	// not the holder's to spend yet. Asserted so a reader can see the pin is
	// not passing because the deposit failed to land.
	verified, err := svc.VerifiedBalanceReader().VerifiedBalance(ctx, holder, cur.UID, pending.UID)
	require.NoError(t, err)
	require.True(t, verified.Equal(decimal.NewFromInt(1000)), "expected the pending deposit to be verifiable, got %s", verified)

	_, err = svc.Reserver().Reserve(ctx, core.ReserveInput{
		AccountHolder:          holder,
		CurrencyUID:            cur.UID,
		Amount:                 decimal.NewFromInt(900),
		IdempotencyKey:         postgrestest.UniqueKey("rbp-reserve"),
		ExpiresIn:              10 * time.Minute,
		RequireVerifiedBalance: true,
	})
	require.Error(t, err, "an unconfirmed deposit must not be withdrawable through the gated path")
	assert.ErrorIs(t, err, core.ErrInsufficientBalance)
}
