package postgres_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ledger "github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// wedgeFixture is the shape w3-review/money-path.md M-3 measured: a booking
// whose transition event has been claimed by an unrelated journal, so its own
// settling journal can never be recorded.
type wedgeFixture struct {
	bookingUID  string
	eventUID    string
	journalType string
	currency    string
	wallet      string
	custodial   string
	holder      int64
}

func setupEventWedge(t *testing.T, ctx context.Context, pool *pgxpool.Pool, svc *ledger.Service, suffix string) wedgeFixture {
	t.Helper()
	classStore := postgres.NewClassificationStore(pool)
	bookingStore := postgres.NewBookingStore(pool)

	f := wedgeFixture{
		currency:    postgrestest.SeedCurrencyWithExponent(t, pool, "WDG"+suffix, "Wedge Unit "+suffix, 2),
		journalType: postgrestest.SeedJournalType(t, pool, "transfer", "Transfer"),
		wallet:      postgrestest.SeedClassification(t, pool, "main_wallet", "Main Wallet", "debit", false),
		custodial:   postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true),
		holder:      4701,
	}

	cls, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code:       "wedge_booking_" + suffix,
		Name:       "Wedge Booking " + suffix,
		NormalSide: core.NormalSideCredit,
		IsSystem:   true,
		Lifecycle: &core.Lifecycle{
			Initial:     "pending",
			Terminal:    []core.Status{"confirmed"},
			Transitions: map[core.Status][]core.Status{"pending": {"confirmed"}},
		},
	})
	require.NoError(t, err)

	booking, err := bookingStore.CreateBooking(ctx, core.CreateBookingInput{
		ClassificationCode: cls.Code,
		AccountHolder:      f.holder,
		CurrencyUID:        f.currency,
		Amount:             decimal.RequireFromString("100"),
		IdempotencyKey:     postgrestest.UniqueKey("wdg-booking"),
		ChannelName:        "test",
	})
	require.NoError(t, err)
	f.bookingUID = booking.UID

	evt, err := bookingStore.Transition(ctx, core.TransitionInput{
		BookingUID:     booking.UID,
		ToStatus:       "confirmed",
		IdempotencyKey: postgrestest.UniqueKey("wdg-transition"),
	})
	require.NoError(t, err)
	f.eventUID = evt.UID

	// The squat: I-51 rule 4 requires only that the journal touch the
	// booking's (holder, currency), which a write-scope credential already
	// knows. 0.01 of an unrelated movement is enough to take the link.
	_, err = svc.JournalWriter().PostJournal(ctx, core.JournalInput{
		JournalTypeUID: f.journalType,
		IdempotencyKey: postgrestest.UniqueKey("wdg-squatter"),
		EventUID:       evt.UID,
		Entries: []core.EntryInput{
			{AccountHolder: f.holder, CurrencyUID: f.currency, ClassificationUID: f.wallet, EntryType: core.EntryTypeDebit, Amount: decimal.RequireFromString("0.01")},
			{AccountHolder: -f.holder, CurrencyUID: f.currency, ClassificationUID: f.custodial, EntryType: core.EntryTypeCredit, Amount: decimal.RequireFromString("0.01")},
		},
	})
	require.NoError(t, err, "the squat itself is legal today -- that is the finding")

	return f
}

func (f wedgeFixture) postSettlingJournal(ctx context.Context, svc *ledger.Service, key string) error {
	_, err := svc.JournalWriter().PostJournal(ctx, core.JournalInput{
		JournalTypeUID: f.journalType,
		IdempotencyKey: postgrestest.UniqueKey(key),
		EventUID:       f.eventUID,
		Entries: []core.EntryInput{
			{AccountHolder: f.holder, CurrencyUID: f.currency, ClassificationUID: f.wallet, EntryType: core.EntryTypeDebit, Amount: decimal.RequireFromString("100")},
			{AccountHolder: -f.holder, CurrencyUID: f.currency, ClassificationUID: f.custodial, EntryType: core.EntryTypeCredit, Amount: decimal.RequireFromString("100")},
		},
	})
	return err
}

// TestMigration027_UnlinkEventJournalReopensAWedgedBooking is M-3's pin
// (2026-09-02 adversarial re-review, w3-review/money-path.md M-3; contract
// §7.15 option (A)).
//
// A journal that claims another journal's event permanently prevents the
// booking's real accounting from ever being recorded: events.journal_id and
// bookings.journal_id are both set-once, journals are append-only so the
// squatter cannot be deleted, and the library shipped no unlink path at all.
// The whole deposit/settlement pipeline stops for that booking with no
// recovery -- fail-closed with no door is not fail-closed, it is stuck.
//
// Migration 027 adds the door, and puts it where the threat model says it
// belongs: owner-only, audited, and closed to the application credential
// (see TestMigration027_UnlinkEventJournalIsRefusedForLedgerApp).
func TestMigration027_UnlinkEventJournalReopensAWedgedBooking(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	svc, err := ledger.New(pool)
	require.NoError(t, err)

	f := setupEventWedge(t, ctx, pool, svc, "A")

	// The wedge: the booking's own settling journal is refused, forever.
	err = f.postSettlingJournal(ctx, svc, "wdg-real-before")
	require.Error(t, err)
	require.ErrorIs(t, err, core.ErrConflict)

	var bookingJournal, eventJournal *int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT b.journal_id, e.journal_id FROM bookings b JOIN events e ON e.booking_id = b.id WHERE b.uid = $1::uuid`,
		f.bookingUID).Scan(&bookingJournal, &eventJournal))
	require.NotNil(t, bookingJournal, "the squatter holds the booking's set-once journal_id")
	require.NotNil(t, eventJournal)
	squatter := *eventJournal

	// The door.
	_, err = pool.Exec(ctx, `SELECT ledger_unlink_event_journal($1::uuid)`, f.eventUID)
	require.NoError(t, err)

	require.NoError(t, pool.QueryRow(ctx,
		`SELECT b.journal_id, e.journal_id FROM bookings b JOIN events e ON e.booking_id = b.id WHERE b.uid = $1::uuid`,
		f.bookingUID).Scan(&bookingJournal, &eventJournal))
	assert.Nil(t, bookingJournal, "the booking's link must be reopened")
	assert.Nil(t, eventJournal, "the event's link must be reopened")

	// The pipeline resumes: the real settling journal records.
	require.NoError(t, f.postSettlingJournal(ctx, svc, "wdg-real-after"),
		"after the unlink the booking's own accounting must be postable")

	// The squatter journal itself is untouched -- journals are append-only,
	// and the row is real evidence of what happened.
	var stillThere int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM journals WHERE id = $1`, squatter).Scan(&stillThere))
	assert.Equal(t, 1, stillThere, "the unlink must not delete or rewrite the squatter journal")

	// And it is audited: the operation leaves a row naming the roles and the
	// before/after, on the table 020 made owner-only.
	var audited int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM config_table_changes
		WHERE table_name = 'ledger_unlink_event_journal'
	`).Scan(&audited))
	assert.Equal(t, 1, audited, "an owner-only repair that leaves no forensic row is worse than the wedge")
}

// TestMigration027_UnlinkEventJournalIsRefusedForLedgerApp is the half that
// makes the door safe: the credential this repo's threat model assumes is
// leaked must not be able to unlink anything. If it could, the set-once rule
// I-51 rests on would be advisory -- a squatter could unlink and re-squat at
// will, and a real settlement could be displaced after the fact.
func TestMigration027_UnlinkEventJournalIsRefusedForLedgerApp(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	svc, err := ledger.New(pool)
	require.NoError(t, err)

	f := setupEventWedge(t, ctx, pool, svc, "B")

	const appPassword = "mig027-app-password-not-a-real-secret" //nolint:gosec // test-only credential
	_, err = pool.Exec(ctx, fmt.Sprintf("ALTER ROLE ledger_app WITH PASSWORD '%s'", appPassword))
	require.NoError(t, err)
	app, err := pgxpool.New(ctx, withRole(t, roleURLFromPool(pool), "ledger_app", appPassword))
	require.NoError(t, err)
	t.Cleanup(app.Close)
	require.NoError(t, app.Ping(ctx))

	_, err = app.Exec(ctx, `SELECT ledger_unlink_event_journal($1::uuid)`, f.eventUID)
	assertPermissionDenied(t, err)

	// Nor by hand: the guards refuse the same UPDATE outside the function.
	_, err = app.Exec(ctx, `UPDATE events SET journal_id = NULL WHERE uid = $1::uuid`, f.eventUID)
	require.Error(t, err, "clearing the link directly must stay refused")
	_, err = app.Exec(ctx, `UPDATE bookings SET journal_id = NULL WHERE uid = $1::uuid`, f.bookingUID)
	require.Error(t, err)

	// Setting the transaction-local flag the guards look for does not help
	// either: the exception also requires membership of ledger_owner.
	_, err = app.Exec(ctx, `
		SELECT set_config('ledger.unlink_event_journal', 'on', false);
		UPDATE events SET journal_id = NULL WHERE uid = $1::uuid;
	`, f.eventUID)
	require.Error(t, err, "a GUC any role can set must not be the only thing standing between ledger_app and the link")

	var eventJournal *int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT journal_id FROM events WHERE uid = $1::uuid`, f.eventUID).Scan(&eventJournal))
	assert.NotNil(t, eventJournal, "the link must still be held after every refused attempt")
}

// TestMigration027_UnlinkEventJournalFailsLoud pins the two ways the function
// must not be quiet: an event that does not exist, and an event that holds no
// link. Returning success in either case would let a runbook step report
// "done" having done nothing (working-agreements.md §3).
func TestMigration027_UnlinkEventJournalFailsLoud(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	svc, err := ledger.New(pool)
	require.NoError(t, err)

	f := setupEventWedge(t, ctx, pool, svc, "C")

	_, err = pool.Exec(ctx, `SELECT ledger_unlink_event_journal('00000000-0000-0000-0000-000000000000'::uuid)`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no event")

	_, err = pool.Exec(ctx, `SELECT ledger_unlink_event_journal($1::uuid)`, f.eventUID)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `SELECT ledger_unlink_event_journal($1::uuid)`, f.eventUID)
	require.Error(t, err, "unlinking an already-unlinked event must not report success")
	assert.Contains(t, err.Error(), "carries no journal link")
}
