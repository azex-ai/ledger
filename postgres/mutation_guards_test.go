package postgres_test

// Pins for I-25 (migration 045): the five mutation-guard gaps in
// docs/plans/2026-08-21-tamper-evident-ledger-design.md §6 (A1-A5). Every
// test in this file attempts the exact SQL a DB-credentialed attacker would
// run and asserts the guard trigger rejects it; each was verified to fail
// (i.e. the raw UPDATE would have succeeded) before migration 045 landed.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// A1: classifications.normal_side is a plain CHECK-constrained column with
// no legitimate mutation path anywhere in the codebase. Flipping it
// retroactively reinterprets every historical rollup for that classification.
func TestClassificationsGuard_NormalSideImmutable(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	uid := postgrestest.SeedClassification(t, pool, postgrestest.UniqueKey("guard-normal-side"), "Guard Normal Side", "debit", false)

	_, err := pool.Exec(ctx, "UPDATE classifications SET normal_side = 'credit' WHERE uid = $1", uid)
	require.Error(t, err, "UPDATE classifications.normal_side must be rejected by the mutation guard")
}

// A2: classifications.balance_role's only legitimate transition is the
// expand-style ” -> <role> upgrade ClassificationStore.SetBalanceRole
// performs. Any other change -- including switching between two non-empty
// roles or reverting to ” -- silently re-buckets the holder-facing
// available/pending/locked breakdown.
func TestClassificationsGuard_BalanceRoleOnlyUpgradesFromEmpty(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	uid := postgrestest.SeedClassificationWithRole(t, pool, postgrestest.UniqueKey("guard-balance-role"), "Guard Balance Role", "debit", false, "")

	// The one legitimate transition: '' -> 'available'.
	classStore := postgres.NewClassificationStore(pool)
	err := classStore.SetBalanceRole(ctx, uid, core.BalanceRoleAvailable)
	require.NoError(t, err, "the documented '' -> role upgrade must still work")

	// Any further change is rejected -- including going through the very
	// entry point that performed the first, legitimate transition.
	err = classStore.SetBalanceRole(ctx, uid, core.BalanceRoleLocked)
	require.Error(t, err, "switching between two non-empty balance roles must be rejected")

	// And a raw SQL attempt to revert to '' must also be rejected.
	_, err = pool.Exec(ctx, "UPDATE classifications SET balance_role = '' WHERE uid = $1", uid)
	require.Error(t, err, "reverting balance_role to '' must be rejected by the mutation guard")
}

// A3a: reservations' dimension/identity columns have no legitimate mutation
// path (InsertReservation is the only writer of any of them).
func TestReservationsGuard_DimensionColumnsImmutable(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	res, _ := seedActiveReservation(t, pool, ctx)

	cases := []struct {
		name string
		sql  string
	}{
		{"account_holder", "UPDATE reservations SET account_holder = account_holder + 1 WHERE id = $1"},
		{"currency_id", "UPDATE reservations SET currency_id = currency_id + 1 WHERE id = $1"},
		{"reserved_amount", "UPDATE reservations SET reserved_amount = reserved_amount + 1 WHERE id = $1"},
		{"idempotency_key", "UPDATE reservations SET idempotency_key = idempotency_key || '-tamper' WHERE id = $1"},
		{"expires_at", "UPDATE reservations SET expires_at = expires_at + INTERVAL '1 day' WHERE id = $1"},
		{"created_at", "UPDATE reservations SET created_at = created_at - INTERVAL '1 day' WHERE id = $1"},
		{"uid", "UPDATE reservations SET uid = gen_random_uuid() WHERE id = $1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, tc.sql, res.id)
			require.Error(t, err, "UPDATE reservations.%s must be rejected by the mutation guard", tc.name)
		})
	}
}

// A3b: settled_amount only ever accumulates. A decrease can only be
// tampering -- SettlePartial's own precondition already guarantees this at
// the application layer; the guard makes it a DB-level fact.
func TestReservationsGuard_SettledAmountMustNotDecrease(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	res, reserverStore := seedActiveReservation(t, pool, ctx)

	err := reserverStore.SettlePartial(ctx, core.SettlePartialInput{
		ReservationUID: res.uid,
		Amount:         decimal.NewFromInt(10),
		IdempotencyKey: postgrestest.UniqueKey("settle-partial"),
	})
	require.NoError(t, err)

	_, err = pool.Exec(ctx, "UPDATE reservations SET settled_amount = settled_amount - 1 WHERE id = $1", res.id)
	require.Error(t, err, "a decrease in reservations.settled_amount must be rejected by the mutation guard")

	// A same-or-increasing change must still be allowed (the guard targets
	// the direction, not settled_amount changes in general).
	_, err = pool.Exec(ctx, "UPDATE reservations SET settled_amount = settled_amount + 1 WHERE id = $1", res.id)
	require.NoError(t, err, "an increase in reservations.settled_amount must remain allowed")
}

// A3c: reservations.journal_id is set-once (NULL -> non-NULL only), matching
// the FK-target-exception shape used elsewhere (018/035).
func TestReservationsGuard_JournalIDSetOnce(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	res, _ := seedActiveReservation(t, pool, ctx)
	journalID1 := seedBareJournal(t, pool, ctx)
	journalID2 := seedBareJournal(t, pool, ctx)

	// NULL -> non-NULL: allowed.
	_, err := pool.Exec(ctx, "UPDATE reservations SET journal_id = $2 WHERE id = $1", res.id, journalID1)
	require.NoError(t, err, "the first NULL -> non-NULL journal_id transition must be allowed")

	// non-NULL -> a different value: rejected.
	_, err = pool.Exec(ctx, "UPDATE reservations SET journal_id = $2 WHERE id = $1", res.id, journalID2)
	require.Error(t, err, "reservations.journal_id must be set-once")

	// non-NULL -> NULL: also rejected.
	_, err = pool.Exec(ctx, "UPDATE reservations SET journal_id = NULL WHERE id = $1", res.id)
	require.Error(t, err, "reservations.journal_id must not be unset once set")
}

// A3d: reservations.status follows the whitelist state machine in
// core/reserve.go (reservationTransitions). settled/settling have no
// outgoing edge back to active.
func TestReservationsGuard_StatusWhitelist(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	t.Run("settled_cannot_reopen_to_active", func(t *testing.T) {
		res, reserverStore := seedActiveReservation(t, pool, ctx)
		err := reserverStore.Settle(ctx, core.SettleInput{ReservationUID: res.uid, Amount: decimal.NewFromInt(50)})
		require.NoError(t, err)

		_, err = pool.Exec(ctx, "UPDATE reservations SET status = 'active' WHERE id = $1", res.id)
		require.Error(t, err, "settled -> active must be rejected by the mutation guard")
	})

	t.Run("settling_cannot_revert_to_active", func(t *testing.T) {
		res, reserverStore := seedActiveReservation(t, pool, ctx)
		err := reserverStore.SettlePartial(ctx, core.SettlePartialInput{
			ReservationUID: res.uid,
			Amount:         decimal.NewFromInt(10),
			IdempotencyKey: postgrestest.UniqueKey("settle-partial-revert"),
		})
		require.NoError(t, err)

		_, err = pool.Exec(ctx, "UPDATE reservations SET status = 'active' WHERE id = $1", res.id)
		require.Error(t, err, "settling -> active must be rejected by the mutation guard")
	})

	t.Run("noop_same_status_is_allowed", func(t *testing.T) {
		res, _ := seedActiveReservation(t, pool, ctx)
		_, err := pool.Exec(ctx, "UPDATE reservations SET status = 'active' WHERE id = $1", res.id)
		require.NoError(t, err, "a no-op status update (active -> active) must remain allowed")
	})
}

// A5: period_closes is documented as append-only but had no enforcing
// trigger. Reuses the same ledger_block_mutation() function journal_entries
// relies on.
func TestPeriodClosesGuard_NoUpdateNoDelete(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	closer := postgres.NewPeriodCloseStore(pool)
	pc, err := closer.ClosePeriod(ctx, core.ClosePeriodInput{
		CloseBefore: time.Now().Add(-24 * time.Hour),
		Note:        "guard test",
	})
	require.NoError(t, err)

	_, err = pool.Exec(ctx, "UPDATE period_closes SET note = 'tampered' WHERE uid = $1", pc.UID)
	require.Error(t, err, "UPDATE on period_closes must be rejected")

	_, err = pool.Exec(ctx, "DELETE FROM period_closes WHERE uid = $1", pc.UID)
	require.Error(t, err, "DELETE on period_closes must be rejected")
}

// A4: journals.event_id is now a nullable FK-target-exception column with
// true set-once semantics: NULL -> non-NULL is the only legal transition.
// Before migration 045 this column wasn't even in the anti-tamper guard's
// comparison list, so ANY UPDATE (including reassigning it to a different
// event) silently passed.
func TestJournalsGuard_EventIDSetOnce(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	classStore := postgres.NewClassificationStore(pool)
	bookingStore := postgres.NewBookingStore(pool)

	curID := postgrestest.SeedCurrency(t, pool, postgrestest.UniqueKey("USDT"), "Tether USD")
	clsWallet := postgrestest.SeedClassification(t, pool, postgrestest.UniqueKey("guard-event-wallet"), "Guard Event Wallet", "debit", false)
	clsCustodial := postgrestest.SeedClassification(t, pool, postgrestest.UniqueKey("guard-event-custodial"), "Guard Event Custodial", "credit", true)
	jt := postgrestest.SeedJournalType(t, pool, postgrestest.UniqueKey("guard-event-jt"), "Guard Event JT")

	lifecycle := &core.Lifecycle{
		Initial:  "pending",
		Terminal: []core.Status{"confirmed"},
		Transitions: map[core.Status][]core.Status{
			"pending": {"confirmed"},
		},
	}
	bookingCls, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code:       postgrestest.UniqueKey("guard-event-booking-cls"),
		Name:       "Guard Event Booking",
		NormalSide: core.NormalSideCredit,
		Lifecycle:  lifecycle,
	})
	require.NoError(t, err)

	makeEvent := func() string {
		booking, err := bookingStore.CreateBooking(ctx, core.CreateBookingInput{
			ClassificationCode: bookingCls.Code,
			AccountHolder:      9001,
			CurrencyUID:        curID,
			Amount:             decimal.NewFromInt(1),
			IdempotencyKey:     postgrestest.UniqueKey("guard-event-booking"),
			ChannelName:        "test",
		})
		require.NoError(t, err)
		evt, err := bookingStore.Transition(ctx, core.TransitionInput{
			BookingUID: booking.UID,
			ToStatus:   "confirmed",
			Source:     "test",
		})
		require.NoError(t, err)
		return evt.UID
	}

	eventUID1 := makeEvent()
	eventUID2 := makeEvent()

	j, err := ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jt,
		IdempotencyKey: postgrestest.UniqueKey("guard-event-journal"),
		Source:         "test",
		EventUID:       eventUID1,
		Entries: []core.EntryInput{
			{AccountHolder: 9001, CurrencyUID: curID, ClassificationUID: clsWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(10)},
			{AccountHolder: core.SystemAccountHolder(9001), CurrencyUID: curID, ClassificationUID: clsCustodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(10)},
		},
	})
	require.NoError(t, err)
	require.Equal(t, eventUID1, j.EventUID)

	eventID2 := postgrestest.InternalID(t, pool, "events", eventUID2)

	_, err = pool.Exec(ctx, "UPDATE journals SET event_id = $2 WHERE uid = $1", j.UID, eventID2)
	require.Error(t, err, "journals.event_id must be set-once and already set")

	_, err = pool.Exec(ctx, "UPDATE journals SET event_id = NULL WHERE uid = $1", j.UID)
	require.Error(t, err, "journals.event_id must not be unset once set")
}

type seededReservation struct {
	id  int64
	uid string
}

// seedActiveReservation creates a fresh currency + reservation via the real
// Reserver API and returns its internal id (for raw-SQL tamper attempts) and
// uid (for the Reserver API), plus the ReserverStore to drive further
// legitimate transitions in the calling test.
func seedActiveReservation(t *testing.T, pool *pgxpool.Pool, ctx context.Context) (seededReservation, *postgres.ReserverStore) {
	t.Helper()

	ledgerStore := postgres.NewLedgerStore(pool)
	reserverStore := postgres.NewReserverStore(pool, ledgerStore)
	curID := postgrestest.SeedCurrency(t, pool, postgrestest.UniqueKey("USDT"), "Tether USD")

	const holder int64 = 9002
	fundAvailableBalance(t, ctx, ledgerStore, pool, holder, curID, decimal.NewFromInt(1000))

	res, err := reserverStore.Reserve(ctx, core.ReserveInput{
		AccountHolder:  holder,
		CurrencyUID:    curID,
		Amount:         decimal.NewFromInt(100),
		IdempotencyKey: postgrestest.UniqueKey("guard-reservation"),
		ExpiresIn:      15 * time.Minute,
	})
	require.NoError(t, err)

	return seededReservation{
		id:  postgrestest.InternalID(t, pool, "reservations", res.UID),
		uid: res.UID,
	}, reserverStore
}

// fundAvailableBalance posts a deposit journal into a role='available'
// classification so Reserve's availability check (I-11) has something to
// draw against.
func fundAvailableBalance(t *testing.T, ctx context.Context, ledgerStore *postgres.LedgerStore, pool *pgxpool.Pool, holder int64, currencyUID string, amount decimal.Decimal) {
	t.Helper()

	jt := postgrestest.SeedJournalType(t, pool, postgrestest.UniqueKey("guard-fund-jt"), "Guard Fund JT")
	walletID := postgrestest.SeedClassificationWithRole(t, pool, postgrestest.UniqueKey("guard-fund-wallet"), "Guard Fund Wallet", "debit", false, "available")
	custodialID := postgrestest.SeedClassification(t, pool, postgrestest.UniqueKey("guard-fund-custodial"), "Guard Fund Custodial", "credit", true)

	_, err := ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jt,
		IdempotencyKey: postgrestest.UniqueKey("guard-fund-deposit"),
		Source:         "test",
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: currencyUID, ClassificationUID: walletID, EntryType: core.EntryTypeDebit, Amount: amount},
			{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: currencyUID, ClassificationUID: custodialID, EntryType: core.EntryTypeCredit, Amount: amount},
		},
	})
	require.NoError(t, err)
}

// seedBareJournal posts a minimal balanced journal and returns its internal
// id, purely as a valid FK target for reservations.journal_id tamper tests.
func seedBareJournal(t *testing.T, pool *pgxpool.Pool, ctx context.Context) int64 {
	t.Helper()

	ledgerStore := postgres.NewLedgerStore(pool)
	curID := postgrestest.SeedCurrency(t, pool, postgrestest.UniqueKey("USDT"), "Tether USD")
	clsWallet := postgrestest.SeedClassification(t, pool, postgrestest.UniqueKey("guard-journal-wallet"), "Guard Journal Wallet", "debit", false)
	clsCustodial := postgrestest.SeedClassification(t, pool, postgrestest.UniqueKey("guard-journal-custodial"), "Guard Journal Custodial", "credit", true)
	jt := postgrestest.SeedJournalType(t, pool, postgrestest.UniqueKey("guard-journal-jt"), "Guard Journal JT")

	j, err := ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jt,
		IdempotencyKey: postgrestest.UniqueKey("guard-bare-journal"),
		Source:         "test",
		Entries: []core.EntryInput{
			{AccountHolder: 9003, CurrencyUID: curID, ClassificationUID: clsWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(1)},
			{AccountHolder: core.SystemAccountHolder(9003), CurrencyUID: curID, ClassificationUID: clsCustodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(1)},
		},
	})
	require.NoError(t, err)
	return postgrestest.InternalID(t, pool, "journals", j.UID)
}
