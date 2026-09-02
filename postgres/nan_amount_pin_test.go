package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// Pins for C-M7 (2026-09-02 audit, tamper-evident.md M-7): NUMERIC accepts
// 'NaN', and none of this schema's amount guards rejected it, because on
// numeric NaN = NaN is TRUE and NaN sorts above every finite value -- so
// both CHECK (x > 0) and a self-equality check pass. One such row panicked
// postgres.mustNumericToDecimal, which sits on the read path of
// service.VerifyLedger's journal sampling, the reconcile suite, and every
// journal read: a single INSERT turned the verification side off.
//
// Two independent defences, one pin each:
//   - migration 018's CHECK constraints (the durable fix -- the value never
//     gets in);
//   - the read paths in postgres/convert.go propagating an error instead of
//     panicking (so a row that predates the constraint, or arrives through
//     some path nobody predicted, degrades to a failed request rather than a
//     dead worker process).

// TestJournals_RejectsNaNTotals pins migration 018 on journals.
func TestJournals_RejectsNaNTotals(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	fx := seedNaNFixture(t, ctx, pool)
	journalTypeID := fx.journalTypeID

	insert := func(total string) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit, metadata, actor_id, source, effective_at, uid)
			VALUES ($1, $2, $3::numeric, $3::numeric, '{}'::jsonb, 0, 'nan-pin', now(), gen_random_uuid())
		`, journalTypeID, postgrestest.UniqueKey("nan-pin"), total)
		return err
	}

	require.NoError(t, insert("1"), "control: a finite total must be accepted")

	err := insert("NaN")
	require.Error(t, err, "a NaN total_debit/total_credit must be rejected by the database")
	require.Contains(t, strings.ToLower(err.Error()), "not_nan",
		"the rejection must come from migration 018's CHECK constraint, not from something incidental: %v", err)
}

// TestJournalEntries_RejectsNaNAmount pins migration 018 on the PARTITIONED
// table: the constraint is declared on the parent, so it has to reach every
// existing partition (and the ones the worker's partition job creates later).
func TestJournalEntries_RejectsNaNAmount(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	fx := seedNaNFixture(t, ctx, pool)

	var journalID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit, metadata, actor_id, source, effective_at, uid)
		VALUES ($1, $2, 1::numeric, 1::numeric, '{}'::jsonb, 0, 'nan-pin', now(), gen_random_uuid())
		RETURNING id
	`, fx.journalTypeID, postgrestest.UniqueKey("nan-entry-pin")).Scan(&journalID))

	_, err := pool.Exec(ctx, `
		INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, effective_at, created_at)
		VALUES ($1, 7701, $2, $3, 'debit', 'NaN'::numeric, now(), now())
	`, journalID, fx.currencyID, fx.classificationID)
	require.Error(t, err, "a NaN entry amount must be rejected")
	require.Contains(t, strings.ToLower(err.Error()), "not_nan", "%v", err)
}

// TestReservationsAndBookings_RejectNaNAmounts pins the sibling columns the
// same shape reaches (A's hand-off list: reservations, bookings,
// account_policies). Migration 018 covers every NUMERIC column in the
// schema; these are the three an application credential can write directly.
func TestReservationsAndBookings_RejectNaNAmounts(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	fx := seedNaNFixture(t, ctx, pool)

	_, err := pool.Exec(ctx, `
		INSERT INTO reservations (uid, account_holder, currency_id, reserved_amount, status, idempotency_key, expires_at)
		VALUES (gen_random_uuid(), 7702, $1, 'NaN'::numeric, 'active', $2, now() + interval '1 hour')
	`, fx.currencyID, postgrestest.UniqueKey("nan-reservation"))
	require.Error(t, err, "a NaN reserved_amount must be rejected")
	require.Contains(t, strings.ToLower(err.Error()), "not_nan", "%v", err)

	_, err = pool.Exec(ctx, `
		INSERT INTO bookings (uid, classification_id, account_holder, currency_id, amount, status, idempotency_key)
		VALUES (gen_random_uuid(), $1, 7703, $2, 'NaN'::numeric, 'pending', $3)
	`, fx.classificationID, fx.currencyID, postgrestest.UniqueKey("nan-booking"))
	require.Error(t, err, "a NaN booking amount must be rejected")
	require.Contains(t, strings.ToLower(err.Error()), "not_nan", "%v", err)

	_, err = pool.Exec(ctx, `
		INSERT INTO account_policies (uid, account_holder, currency_id, classification_id, status, min_balance, enforce_min_balance)
		VALUES (gen_random_uuid(), 7704, $1, $2, 'active', 'NaN'::numeric, true)
	`, fx.currencyID, fx.classificationID)
	require.Error(t, err, "a NaN min_balance must be rejected")
	require.Contains(t, strings.ToLower(err.Error()), "not_nan", "%v", err)
}

// TestJournalRead_NaNAmountIsAnErrorNotAPanic pins the READ-side half. The
// constraint is dropped for the length of this test (as an owner would to
// simulate a row that predates it) so a NaN row can exist at all; reading it
// must produce an error, never take the process down.
//
// The route is the ordinary consumer read path -- ledger.Service's query
// store, the same one service.VerifyLedger's journal sampling uses -- not a
// direct call to the converter.
func TestJournalRead_NaNAmountIsAnErrorNotAPanic(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	fx := seedNaNFixture(t, ctx, pool)

	// Dropped, not restored: postgrestest.SetupDB gives this test its own
	// database, which is discarded afterwards. Restoring the constraint is
	// impossible anyway while the NaN row exists -- journals is append-only,
	// so the row cannot be deleted, which is itself a useful reminder of why
	// the constraint has to be there BEFORE the row is.
	for _, name := range []string{"chk_journals_total_debit_not_nan", "chk_journals_total_credit_not_nan"} {
		_, err := pool.Exec(ctx, fmt.Sprintf("ALTER TABLE journals DROP CONSTRAINT %s", name))
		require.NoError(t, err)
	}

	// NaN in BOTH totals, which is what makes this row possible at all: the
	// pre-existing chk_journal_balance requires total_debit = total_credit,
	// and on numeric NaN = NaN is TRUE. The row satisfies every guard the
	// schema had before migration 018 -- balance check included.
	var uid string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit, metadata, actor_id, source, effective_at, uid)
		VALUES ($1, $2, 'NaN'::numeric, 'NaN'::numeric, '{}'::jsonb, 0, 'nan-read-pin', now(), gen_random_uuid())
		RETURNING uid
	`, fx.journalTypeID, postgrestest.UniqueKey("nan-read")).Scan(&uid))

	queries := postgres.NewQueryStore(pool)

	// Before the fix this call panicked inside mustNumericToDecimal, taking
	// down whatever goroutine was reading -- in production, the worker.
	_, _, err := queries.GetJournal(ctx, uid)
	require.Error(t, err, "reading a NaN amount must return an error")
	require.Contains(t, err.Error(), "NaN")

	// The same row through the sampling path service.VerifyLedger uses.
	_, err = queries.ListRecentJournals(ctx, 10)
	require.Error(t, err, "listing a NaN amount must return an error")
	require.Contains(t, err.Error(), "NaN")
}

// TestBookingIdempotency_UnparseableStoredMetadataConflicts pins I-N23: a
// stored metadata blob that does not parse used to degrade to nil, which is
// the same value a row with NO metadata produces -- so a replay carrying
// DIFFERENT metadata compared EQUAL to the stored row and resolved to
// "already done, here is the original result" instead of ErrConflict. The
// idempotency comparison must fail closed instead.
func TestBookingIdempotency_UnparseableStoredMetadataConflicts(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	fx := seedNaNFixture(t, ctx, pool)

	booker := postgres.NewBookingStore(pool)
	key := postgrestest.UniqueKey("meta-unparseable")
	input := core.CreateBookingInput{
		ClassificationCode: fx.classificationCode,
		AccountHolder:      7801,
		CurrencyUID:        fx.currencyUID,
		Amount:             decimal.NewFromInt(10),
		IdempotencyKey:     key,
		Metadata:           map[string]string{"note": "original"},
	}
	created, err := booker.CreateBooking(ctx, input)
	require.NoError(t, err)

	// Control: a genuine replay still resolves to the original row.
	replay, err := booker.CreateBooking(ctx, input)
	require.NoError(t, err, "a genuine replay must still be idempotent")
	require.Equal(t, created.UID, replay.UID)

	// Corrupt the stored blob. jsonb cannot hold a non-JSON value, so the
	// unparseable case is a valid JSON scalar where the code expects an
	// object -- json.Unmarshal into map[string]json.RawMessage fails on it
	// exactly as it would on garbage.
	_, err = pool.Exec(ctx, "UPDATE bookings SET metadata = '\"not-an-object\"'::jsonb WHERE uid = $1", created.UID)
	require.NoError(t, err)

	// The replay that used to slip through: SAME idempotency key, and NO
	// metadata. Pre-fix, the unreadable stored blob parsed to nil -- exactly
	// what a booking with no metadata produces -- so maps.Equal(nil, nil)
	// said "identical payload" and CreateBooking returned the ORIGINAL
	// booking as a successful idempotent replay. The caller was told its
	// (different) request had already been carried out.
	//
	// A payload with DIFFERENT metadata would not isolate the bug: nil vs
	// {"note":"changed"} compares unequal, so that case conflicted even
	// before the fix.
	noMetadata := input
	noMetadata.Metadata = nil
	_, err = booker.CreateBooking(ctx, noMetadata)
	require.ErrorIs(t, err, core.ErrConflict,
		"an unreadable stored metadata blob must fail closed as a conflict, never be treated as 'this row had no metadata' "+
			"and let a different payload resolve to the original result")
}

type nanFixture struct {
	journalTypeID      int64
	currencyID         int64
	classificationID   int64
	currencyUID        string
	classificationCode string
}

// seedNaNFixture creates the one currency / classification / journal type
// these pins need. Self-contained rather than reusing setupAuthFixture:
// nothing here depends on P5's signing shape.
func seedNaNFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) nanFixture {
	t.Helper()
	suffix := time.Now().UnixNano()

	classStore := postgres.NewClassificationStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)

	cur, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{
		Code: fmt.Sprintf("NANC_%d", suffix), Name: "NaN Pin Currency", Exponent: 18,
	})
	require.NoError(t, err)
	classCode := fmt.Sprintf("nan_main_%d", suffix)
	cls, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: classCode, Name: "NaN Pin Main", NormalSide: core.NormalSideDebit,
		BalanceRole: core.BalanceRoleAvailable,
		Lifecycle: &core.Lifecycle{
			Initial:     "pending",
			Terminal:    []core.Status{"confirmed"},
			Transitions: map[core.Status][]core.Status{"pending": {"confirmed"}},
		},
	})
	require.NoError(t, err)
	jt, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{
		Code: fmt.Sprintf("nan_jt_%d", suffix), Name: "NaN Pin Journal Type",
	})
	require.NoError(t, err)

	f := nanFixture{currencyUID: cur.UID, classificationCode: classCode}
	require.NoError(t, pool.QueryRow(ctx, "SELECT id FROM currencies WHERE uid=$1", cur.UID).Scan(&f.currencyID))
	require.NoError(t, pool.QueryRow(ctx, "SELECT id FROM classifications WHERE uid=$1", cls.UID).Scan(&f.classificationID))
	require.NoError(t, pool.QueryRow(ctx, "SELECT id FROM journal_types WHERE uid=$1", jt.UID).Scan(&f.journalTypeID))
	return f
}
