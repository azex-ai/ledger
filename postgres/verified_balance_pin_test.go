package postgres_test

// Pin tests for W2-T2: VerifiedBalanceReader + Reserve's opt-in
// authorization gate (docs/plans/2026-08-21-integrity-hardening-contracts.md
// §W2-1/§W2-2/§W2-3, docs/INVARIANTS.md I-32).
//
// The headline scenario mirrors P5's own M5 pin
// (TestForgedDirectSQLJournalIsUnauthorized in auth_pin_test.go): an
// attacker with a raw DB write credential inserts a perfectly balanced,
// perfectly per-journal-valid journal crediting a user's available-role
// classification -- every invariant I-1 through I-31 accepts it. Only
// VerifiedBalanceReader (and, when a caller opts in, Reserve) can tell it
// apart from a genuine deposit.
import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// vbFixture is this file's own self-contained fixture (deliberately not
// sharing authFixture: that one's classifications carry no BalanceRole,
// and Reserve's available-balance computation --
// (*ReserverStore).requireVerifiedAvailableBalance, and the pre-existing
// availableBase it complements -- only ever considers
// balance_role='available' classifications).
type vbFixture struct {
	CurrencyUID    string
	AvailableUID   string // balance_role=available, normal_side=debit
	CustodialUID   string // system side, no balance_role
	JournalTypeUID string

	currencyID    int64
	availableID   int64
	custodialID   int64
	journalTypeID int64
}

func setupVBFixture(t testing.TB, pool *pgxpool.Pool, ctx context.Context) vbFixture {
	t.Helper()

	classStore := postgres.NewClassificationStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)
	suffix := time.Now().UnixNano()

	cur, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{
		Code: fmt.Sprintf("VBC_%d", suffix), Name: "Verified Balance Test Currency", Exponent: 18,
	})
	require.NoError(t, err)
	avail, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: fmt.Sprintf("vb_available_%d", suffix), Name: "VB Available", NormalSide: core.NormalSideDebit,
		BalanceRole: core.BalanceRoleAvailable,
	})
	require.NoError(t, err)
	cust, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: fmt.Sprintf("vb_custodial_%d", suffix), Name: "VB Custodial", NormalSide: core.NormalSideCredit, IsSystem: true,
	})
	require.NoError(t, err)
	jt, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{
		Code: fmt.Sprintf("vb_jt_%d", suffix), Name: "VB Test Journal Type",
	})
	require.NoError(t, err)

	f := vbFixture{
		CurrencyUID: cur.UID, AvailableUID: avail.UID, CustodialUID: cust.UID,
		JournalTypeUID: jt.UID,
	}
	require.NoError(t, pool.QueryRow(ctx, "SELECT id FROM currencies WHERE uid=$1", cur.UID).Scan(&f.currencyID))
	require.NoError(t, pool.QueryRow(ctx, "SELECT id FROM classifications WHERE uid=$1", avail.UID).Scan(&f.availableID))
	require.NoError(t, pool.QueryRow(ctx, "SELECT id FROM classifications WHERE uid=$1", cust.UID).Scan(&f.custodialID))
	require.NoError(t, pool.QueryRow(ctx, "SELECT id FROM journal_types WHERE uid=$1", jt.UID).Scan(&f.journalTypeID))
	return f
}

func (f vbFixture) journalInput(userID int64, idemKey string, amount decimal.Decimal) core.JournalInput {
	return core.JournalInput{
		JournalTypeUID: f.JournalTypeUID,
		IdempotencyKey: idemKey,
		Source:         "verified-balance-pin-test",
		ActorID:        userID,
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyUID: f.CurrencyUID, ClassificationUID: f.AvailableUID, EntryType: core.EntryTypeDebit, Amount: amount},
			{AccountHolder: core.SystemAccountHolder(userID), CurrencyUID: f.CurrencyUID, ClassificationUID: f.CustodialUID, EntryType: core.EntryTypeCredit, Amount: amount},
		},
	}
}

// insertForgedBalancedJournal bypasses PostJournal/Authorize entirely --
// the M5 attacker model (a DB write credential, not an app API call) -- and
// inserts a journal + two perfectly balanced entries directly via SQL, with
// empty auth_digest/auth_signature/auth_key_id. Mirrors
// TestForgedDirectSQLJournalIsUnauthorized's technique exactly, including
// doing both legs in one transaction (migration 044's deferred per-journal
// balance trigger requires it) and leaving event_id NULL (migration 045
// made it a real nullable FK; a literal 0 would trip journals_event_id_fkey).
func insertForgedBalancedJournal(t testing.TB, pool *pgxpool.Pool, ctx context.Context, f vbFixture, holder int64, amount string, idemKey string) string {
	t.Helper()

	var journalID int64
	var journalUID string
	var effectiveAt time.Time

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	require.NoError(t, tx.QueryRow(ctx, `
		INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit, metadata, actor_id, source, effective_at, uid, auth_digest, auth_signature, auth_key_id)
		VALUES ($1, $2, $3::numeric, $3::numeric, '{}'::jsonb, 0, 'forged-direct-sql', now(), gen_random_uuid(), ''::bytea, ''::bytea, '')
		RETURNING id, uid, effective_at
	`, f.journalTypeID, idemKey, amount).Scan(&journalID, &journalUID, &effectiveAt))

	_, err = tx.Exec(ctx, `
		INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, effective_at, created_at)
		VALUES ($1, $2, $3, $4, 'debit', $5::numeric, $6, now())
	`, journalID, holder, f.currencyID, f.availableID, amount, effectiveAt)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `
		INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, effective_at, created_at)
		VALUES ($1, $2, $3, $4, 'credit', $5::numeric, $6, now())
	`, journalID, core.SystemAccountHolder(holder), f.currencyID, f.custodialID, amount, effectiveAt)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	return journalUID
}

// TestVerifiedBalance_ZeroContributingJournalsIsDefinedZero pins the
// vacuous-truth case (core.VerifiedBalanceReader's doc comment): a
// dimension no journal has ever touched has no unauthorized journal to
// find, so it is a DEFINED zero, not UNDEFINED -- even with no
// core.AuthVerifier configured at all.
func TestVerifiedBalance_ZeroContributingJournalsIsDefinedZero(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupVBFixture(t, pool, ctx)

	vb := postgres.NewVerifiedBalanceStore(pool, nil)
	balance, err := vb.VerifiedBalance(ctx, 9001, f.CurrencyUID, f.AvailableUID)
	require.NoError(t, err)
	require.True(t, balance.IsZero())
}

// TestVerifiedBalance_AllAuthorizedMatchesRecompute pins the positive path:
// when every contributing journal is genuinely signed, VerifiedBalance
// returns the exact same figure CheckpointIntegrityStore.RecomputeBalance
// does -- the authorization gate does not change the arithmetic, only
// whether the ledger will vouch for it.
func TestVerifiedBalance_AllAuthorizedMatchesRecompute(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupVBFixture(t, pool, ctx)
	const holder int64 = 9002

	attestor, verifier := newTestAttestor(t, "ed25519-vb-authorized")
	store := postgres.NewLedgerStore(pool).WithAuth(attestor)

	_, err := store.PostJournal(ctx, f.journalInput(holder, postgrestest.UniqueKey("vb-authorized-1"), decimal.NewFromInt(100)))
	require.NoError(t, err)
	_, err = store.PostJournal(ctx, f.journalInput(holder, postgrestest.UniqueKey("vb-authorized-2"), decimal.NewFromInt(50)))
	require.NoError(t, err)

	vb := postgres.NewVerifiedBalanceStore(pool, verifier)
	verified, err := vb.VerifiedBalance(ctx, holder, f.CurrencyUID, f.AvailableUID)
	require.NoError(t, err)

	recompute := postgres.NewCheckpointIntegrityStore(pool)
	expected, err := recompute.RecomputeBalance(ctx, holder, f.CurrencyUID, f.AvailableUID)
	require.NoError(t, err)

	require.True(t, verified.Equal(expected), "verified=%s recompute=%s", verified, expected)
	require.True(t, verified.Equal(decimal.NewFromInt(150)))
}

// TestVerifiedBalance_UnauthorizedContributingJournalIsUndefined is the
// direct pin for contracts §W2-1's ruling: a forged, unsigned journal
// contributing to the dimension makes the WHOLE balance UNDEFINED, never a
// smaller-but-defined number. The forged journal here is a pure credit (a
// fabricated deposit) -- exactly the shape that would make "exclude the
// unauthorized journal and sum the rest" report a LOWER number than this
// test's forged-inflated total, which would make the naive exclusion bug
// invisible with this fixture. The bug W2-1 actually warns about (excluding
// a NET-NEGATIVE unauthorized journal, e.g. a reversal, over-reporting) is
// exercised by TestVerifiedBalance_UnauthorizedReversalNeverInflatesBalance
// below; this test's job is simply "any unauthorized contributor at all ->
// UNDEFINED", the simplest possible case.
func TestVerifiedBalance_UnauthorizedContributingJournalIsUndefined(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupVBFixture(t, pool, ctx)
	const holder int64 = 9003

	attestor, verifier := newTestAttestor(t, "ed25519-vb-mixed")
	store := postgres.NewLedgerStore(pool).WithAuth(attestor)
	_, err := store.PostJournal(ctx, f.journalInput(holder, postgrestest.UniqueKey("vb-mixed-signed"), decimal.NewFromInt(100)))
	require.NoError(t, err)

	insertForgedBalancedJournal(t, pool, ctx, f, holder, "1000000.000000000000000000", postgrestest.UniqueKey("vb-mixed-forged"))

	vb := postgres.NewVerifiedBalanceStore(pool, verifier)
	balance, err := vb.VerifiedBalance(ctx, holder, f.CurrencyUID, f.AvailableUID)
	require.Error(t, err, "a forged contributing journal must make the balance UNDEFINED")
	require.ErrorIs(t, err, core.ErrUnauthorizedJournal)
	require.True(t, balance.IsZero(), "UNDEFINED must surface as a non-nil error, never a fabricated non-zero number")
}

// TestVerifiedBalance_UnauthorizedReversalNeverInflatesBalance is the exact
// scenario contracts §W2-1 names as the reason "exclude the unauthorized
// journal and sum the rest" is wrong and dangerous: a reversal journal's
// net contribution to a dimension is NEGATIVE, so excluding it from the sum
// (instead of refusing to answer) would report a balance HIGHER than the
// true one. A genuine signed deposit is reversed by a forged (unsigned)
// reversal -- if VerifiedBalance silently excluded the forged reversal, it
// would report the full deposit amount as available, when the true
// recomputed balance is zero.
func TestVerifiedBalance_UnauthorizedReversalNeverInflatesBalance(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupVBFixture(t, pool, ctx)
	const holder int64 = 9004

	attestor, verifier := newTestAttestor(t, "ed25519-vb-reversal")
	store := postgres.NewLedgerStore(pool).WithAuth(attestor)
	deposit, err := store.PostJournal(ctx, f.journalInput(holder, postgrestest.UniqueKey("vb-reversal-deposit"), decimal.NewFromInt(100)))
	require.NoError(t, err)

	// Forge the reversal directly via SQL (swap debit/credit vs the
	// original) rather than through ReverseJournal, so it lands unsigned --
	// modeling an attacker who reverses a real deposit without the
	// signing key, not a legitimate ReverseJournal call.
	var journalID int64
	var journalUID string
	var effectiveAt time.Time
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	require.NoError(t, tx.QueryRow(ctx, `
		INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit, metadata, actor_id, source, reversal_of, effective_at, uid, auth_digest, auth_signature, auth_key_id)
		VALUES ($1, $2, 100::numeric, 100::numeric, '{}'::jsonb, 0, 'forged-reversal', $3, now(), gen_random_uuid(), ''::bytea, ''::bytea, '')
		RETURNING id, uid, effective_at
	`, f.journalTypeID, postgrestest.UniqueKey("vb-reversal-forged"), journalIDOf(t, pool, ctx, deposit.UID)).Scan(&journalID, &journalUID, &effectiveAt))
	_, err = tx.Exec(ctx, `
		INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, effective_at, created_at)
		VALUES ($1, $2, $3, $4, 'credit', 100::numeric, $5, now())
	`, journalID, holder, f.currencyID, f.availableID, effectiveAt)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `
		INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, effective_at, created_at)
		VALUES ($1, $2, $3, $4, 'debit', 100::numeric, $5, now())
	`, journalID, core.SystemAccountHolder(holder), f.currencyID, f.custodialID, effectiveAt)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	vb := postgres.NewVerifiedBalanceStore(pool, verifier)
	balance, err := vb.VerifiedBalance(ctx, holder, f.CurrencyUID, f.AvailableUID)
	require.Error(t, err)
	require.ErrorIs(t, err, core.ErrUnauthorizedJournal)
	require.True(t, balance.IsZero(), "UNDEFINED must never surface as the deposit's full, un-reversed amount")

	// Sanity: the true recomputed balance (no auth gate) is indeed zero --
	// confirming this test models "excluding the forged reversal would
	// have overstated the balance", not a strawman.
	recompute := postgres.NewCheckpointIntegrityStore(pool)
	trueBalance, err := recompute.RecomputeBalance(ctx, holder, f.CurrencyUID, f.AvailableUID)
	require.NoError(t, err)
	require.True(t, trueBalance.IsZero())
}

func journalIDOf(t testing.TB, pool *pgxpool.Pool, ctx context.Context, uid string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, pool.QueryRow(ctx, "SELECT id FROM journals WHERE uid=$1", uid).Scan(&id))
	return id
}

// TestReserve_RequireVerifiedBalance_RejectsWhenUnauthorizedJournalExists
// is the board pin: Reserve must refuse to reserve funds when the
// account's available balance is backed (in part) by a forged, unsigned
// journal -- even though the ordinary checkpoint-based available-balance
// check alone would happily approve it (proving this gate adds a real,
// distinct refusal, not a redundant one). Comment out the
// RequireVerifiedBalance branch in (*ReserverStore).Reserve to see this go
// red.
func TestReserve_RequireVerifiedBalance_RejectsWhenUnauthorizedJournalExists(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupVBFixture(t, pool, ctx)
	const holder int64 = 9005

	// Forge a large balanced "deposit" directly via SQL -- no signature at
	// all. The ordinary checkpoint/rollup path has no way to distinguish
	// this from a genuine deposit; it happily reports the funds as
	// available.
	insertForgedBalancedJournal(t, pool, ctx, f, holder, "1000.000000000000000000", postgrestest.UniqueKey("reserve-vb-forged"))

	_, verifier := newTestAttestor(t, "ed25519-reserve-vb")
	ledger := postgres.NewLedgerStore(pool)
	vb := postgres.NewVerifiedBalanceStore(pool, verifier)
	reserver := postgres.NewReserverStore(pool, ledger, vb)

	// Sanity: without the opt-in, Reserve approves the reservation purely
	// off the checkpoint-based available balance -- confirming the forged
	// journal really would sail through unnoticed absent this gate.
	baseline, err := reserver.Reserve(ctx, core.ReserveInput{
		AccountHolder:  holder,
		CurrencyUID:    f.CurrencyUID,
		Amount:         decimal.NewFromInt(500),
		IdempotencyKey: postgrestest.UniqueKey("reserve-vb-baseline"),
	})
	require.NoError(t, err, "sanity: the forged deposit must actually be usable when the gate is off")
	require.NotNil(t, baseline)

	_, err = reserver.Reserve(ctx, core.ReserveInput{
		AccountHolder:          holder,
		CurrencyUID:            f.CurrencyUID,
		Amount:                 decimal.NewFromInt(500),
		IdempotencyKey:         postgrestest.UniqueKey("reserve-vb-gated"),
		RequireVerifiedBalance: true,
	})
	require.Error(t, err, "Reserve must refuse when RequireVerifiedBalance is set and a contributing journal is unauthorized")
	require.ErrorIs(t, err, core.ErrUnauthorizedJournal)
}

// TestReserve_RequireVerifiedBalance_AllowsWhenEverythingSigned is the
// companion positive-path pin: the opt-in gate must not reject a perfectly
// ordinary, fully signed account -- RequireVerifiedBalance is not a
// stricter amount check, only an authorization check.
func TestReserve_RequireVerifiedBalance_AllowsWhenEverythingSigned(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupVBFixture(t, pool, ctx)
	const holder int64 = 9006

	attestor, verifier := newTestAttestor(t, "ed25519-reserve-vb-ok")
	ledger := postgres.NewLedgerStore(pool).WithAuth(attestor)
	_, err := ledger.PostJournal(ctx, f.journalInput(holder, postgrestest.UniqueKey("reserve-vb-ok-deposit"), decimal.NewFromInt(1000)))
	require.NoError(t, err)

	vb := postgres.NewVerifiedBalanceStore(pool, verifier)
	reserver := postgres.NewReserverStore(pool, ledger, vb)

	res, err := reserver.Reserve(ctx, core.ReserveInput{
		AccountHolder:          holder,
		CurrencyUID:            f.CurrencyUID,
		Amount:                 decimal.NewFromInt(500),
		IdempotencyKey:         postgrestest.UniqueKey("reserve-vb-ok-reserve"),
		RequireVerifiedBalance: true,
	})
	require.NoError(t, err)
	require.NotNil(t, res)
}

// TestReserve_RequireVerifiedBalance_RejectsInflatedCheckpoint is the pin for
// the amount half of the gate (docs/INVARIANTS.md I-49,
// docs/audits/2026-09-02-deep-audit/tamper-evident.md C-1).
//
// Every journal here is genuinely signed, so the authorization half of the
// gate -- everything the two pins above cover -- passes cleanly. What the
// attacker touches is balance_checkpoints, the one balance-bearing table that
// must stay UPDATE-able for the rollup worker and therefore carries no
// append-only trigger: a single UPDATE with the ledger_app credential
// (docs/plans/2026-08-21-tamper-evident-ledger-design.md §1's first threat)
// inflates checkpoint + delta without touching a single journal or entry.
//
// The design's §0 decision table says the withdrawal path recomputes from
// entries and does not read the checkpoint. Before this fix Reserve did read
// it: requireVerifiedAvailableBalance threw away the entries-only figure
// VerifiedBalance had just computed and reserveWithQueries sized the
// reservation off checkpoint + delta. Reverting reserveWithQueries to
// roleSums[available] makes the middle assertion below go green-to-red.
func TestReserve_RequireVerifiedBalance_RejectsInflatedCheckpoint(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupVBFixture(t, pool, ctx)
	const holder int64 = 9007

	attestor, verifier := newTestAttestor(t, "ed25519-reserve-vb-checkpoint")
	ledger := postgres.NewLedgerStore(pool).WithAuth(attestor)
	_, err := ledger.PostJournal(ctx, f.journalInput(holder, postgrestest.UniqueKey("reserve-vb-cp-deposit"), decimal.NewFromInt(1000)))
	require.NoError(t, err)

	vb := postgres.NewVerifiedBalanceStore(pool, verifier)
	reserver := postgres.NewReserverStore(pool, ledger, vb)

	// Control 1: the gate still approves an amount the REAL balance covers.
	// Without this the test could pass by rejecting everything.
	honest, err := reserver.Reserve(ctx, core.ReserveInput{
		AccountHolder:          holder,
		CurrencyUID:            f.CurrencyUID,
		Amount:                 decimal.NewFromInt(100),
		IdempotencyKey:         postgrestest.UniqueKey("reserve-vb-cp-honest"),
		RequireVerifiedBalance: true,
	})
	require.NoError(t, err, "the gate must not reject an amount the entries-only balance genuinely covers")
	require.NotNil(t, honest)

	// The attack: inflate the checkpoint for the available-role dimension.
	// last_entry_id = 0 keeps every existing entry inside the delta window, so
	// the tampered figure is checkpoint + the real 1000, not a replacement.
	_, err = pool.Exec(ctx, `
		INSERT INTO balance_checkpoints (account_holder, currency_id, classification_id, balance, last_entry_id, last_entry_at)
		VALUES ($1, $2, $3, 1000000, 0, now())
		ON CONFLICT (account_holder, currency_id, classification_id)
		DO UPDATE SET balance = 1000000, last_entry_id = 0, last_entry_at = now()
	`, holder, f.currencyID, f.availableID)
	require.NoError(t, err)

	// The pin: signatures are all valid, so the authorization half of the gate
	// passes -- and the reservation must still be refused, because the amount
	// it would authorize exists only in the checkpoint.
	_, err = reserver.Reserve(ctx, core.ReserveInput{
		AccountHolder:          holder,
		CurrencyUID:            f.CurrencyUID,
		Amount:                 decimal.NewFromInt(500000),
		IdempotencyKey:         postgrestest.UniqueKey("reserve-vb-cp-gated"),
		RequireVerifiedBalance: true,
	})
	require.Error(t, err, "Reserve must size the reservation off the entries-only recompute, not off balance_checkpoints")
	require.ErrorIs(t, err, core.ErrInsufficientBalance)

	// Control 2: the tampering really is in effect and really would have paid
	// out -- the ungated path (the pre-fix behavior of the gated one) approves
	// the same 500000 the assertion above just refused.
	inflated, err := reserver.Reserve(ctx, core.ReserveInput{
		AccountHolder:  holder,
		CurrencyUID:    f.CurrencyUID,
		Amount:         decimal.NewFromInt(500000),
		IdempotencyKey: postgrestest.UniqueKey("reserve-vb-cp-baseline"),
	})
	require.NoError(t, err, "sanity: without the gate the inflated checkpoint must actually be spendable, or the pin above proves nothing")
	require.NotNil(t, inflated)
}

// countingVerifier records how many times a verification was attempted, so a
// test can assert a fail-closed guard fired BEFORE any (potentially remote)
// call went out -- not merely that the call happened to return an error.
type countingVerifier struct {
	inner  core.AuthVerifier
	verify atomic.Int64
}

func (v *countingVerifier) Verify(ctx context.Context, digest, signature []byte, keyID string) error {
	v.verify.Add(1)
	return v.inner.Verify(ctx, digest, signature, keyID)
}

// TestVerifiedBalance_TxBoundStoreFailsClosed pins the store-side half of
// concurrency.md's "VerifiedBalanceReader() on a RunInTx clone has no guard"
// (docs/INVARIANTS.md I-32).
//
// core.AuthVerifier is explicitly allowed to run off-host ("that independence
// is the whole point"), and financial.md forbids an external call inside an
// open transaction. Reserve's RequireVerifiedBalance gate already refuses on a
// tx-bound store for exactly that reason; the same external call is reachable
// through the store's own method, which had no such guard. The counter
// assertion is the point: the guard must fire before the verifier is
// consulted, not after.
func TestVerifiedBalance_TxBoundStoreFailsClosed(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupVBFixture(t, pool, ctx)
	const holder int64 = 9008

	attestor, realVerifier := newTestAttestor(t, "ed25519-vb-tx-guard")
	verifier := &countingVerifier{inner: realVerifier}
	ledger := postgres.NewLedgerStore(pool).WithAuth(attestor)
	_, err := ledger.PostJournal(ctx, f.journalInput(holder, postgrestest.UniqueKey("vb-tx-guard-deposit"), decimal.NewFromInt(1000)))
	require.NoError(t, err)

	vb := postgres.NewVerifiedBalanceStore(pool, verifier)

	// Control: on the pool the same call works and does reach the verifier,
	// so the guard below is about the transaction, not about the fixture.
	poolBalance, err := vb.VerifiedBalance(ctx, holder, f.CurrencyUID, f.AvailableUID)
	require.NoError(t, err)
	require.True(t, poolBalance.Equal(decimal.NewFromInt(1000)))
	require.Positive(t, verifier.verify.Load(), "control: the pool-mode path must actually consult the verifier")

	before := verifier.verify.Load()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = vb.WithDB(tx).VerifiedBalance(ctx, holder, f.CurrencyUID, f.AvailableUID)
	require.Error(t, err, "VerifiedBalance must fail closed on a transaction-bound store instead of calling a possibly-remote AuthVerifier inside an open transaction")
	require.ErrorIs(t, err, core.ErrInvalidInput)
	require.Equal(t, before, verifier.verify.Load(), "the guard must fire before the verifier is consulted, not after")
}

// spendInput is journalInput's mirror: it moves `amount` back OUT of the
// holder's available classification (CR available / DR custodial), so a
// concurrent spend can be posted through the ordinary signed path.
func (f vbFixture) spendInput(userID int64, idemKey string, amount decimal.Decimal) core.JournalInput {
	return core.JournalInput{
		JournalTypeUID: f.JournalTypeUID,
		IdempotencyKey: idemKey,
		Source:         "verified-balance-pin-test",
		ActorID:        userID,
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyUID: f.CurrencyUID, ClassificationUID: f.AvailableUID, EntryType: core.EntryTypeCredit, Amount: amount},
			{AccountHolder: core.SystemAccountHolder(userID), CurrencyUID: f.CurrencyUID, ClassificationUID: f.CustodialUID, EntryType: core.EntryTypeDebit, Amount: amount},
		},
	}
}

// waitForBlockedAdvisoryLock blocks until some backend in THIS test's database
// is waiting on an advisory lock, which is the observable form of "the Reserve
// under test has finished its pre-transaction gate and is now queued behind
// our lock". Polling pg_locks rather than sleeping is what makes the
// interleaving in TestReserve_RequireVerifiedBalance_RechecksUnderLock
// deterministic instead of merely likely.
//
// pg_locks is cluster-wide while postgrestest gives each test its own database
// on a shared server, so the database filter is load-bearing: without it a
// concurrent test package's advisory wait would satisfy this and let the
// spender commit early, quietly turning the pin into a weaker assertion.
//
// Returns an error rather than calling t.Fatal because it runs on a helper
// goroutine, where t.Fatal does not stop the test it appears to stop.
func waitForBlockedAdvisoryLock(ctx context.Context, pool *pgxpool.Pool) error {
	deadline := time.Now().Add(20 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory'
			  AND NOT granted
			  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
		`).Scan(&waiting); err != nil {
			return fmt.Errorf("poll pg_locks: %w", err)
		}
		if waiting > 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for the gated Reserve to block on the balance advisory lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestReserve_RequireVerifiedBalance_RechecksUnderLock pins the second half of
// I-49: the gate's authorized figure is computed before the transaction opens
// (an AuthVerifier may be remote), so it can be stale by the time the balance
// advisory lock is held, and a stale-high figure would re-open the over-sell
// race I-4's lock exists to close.
//
// The window is opened for real rather than simulated. A second goroutine
// holds the (holder, currency) advisory lock -- by posting a genuine, signed
// spend through PostJournal, which takes that lock itself -- and does not
// commit until the Reserve under test is observably queued behind it:
//
//  1. spender: BEGIN, PostJournal(-950) on the caller's tx. Lock held, row
//     uncommitted and therefore invisible to anyone else (READ COMMITTED).
//  2. reserver: Reserve(500, gate on). The gate runs outside any transaction
//     and sees only the committed 1000 deposit, so V = 1000 deterministically.
//     It then blocks on the advisory lock.
//  3. spender: sees the wait in pg_locks, COMMITs. Balance is now 50.
//  4. reserver: acquires the lock, recomputes E = 50 from entries, takes
//     min(1000, 50) = 50, and refuses 500.
//
// Delete the min() -- use *verifiedAvailableBase directly -- and step 4
// reserves 500 against a balance of 50.
func TestReserve_RequireVerifiedBalance_RechecksUnderLock(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupVBFixture(t, pool, ctx)
	const holder int64 = 9009

	attestor, verifier := newTestAttestor(t, "ed25519-reserve-vb-toctou")
	ledger := postgres.NewLedgerStore(pool).WithAuth(attestor)
	_, err := ledger.PostJournal(ctx, f.journalInput(holder, postgrestest.UniqueKey("reserve-vb-toctou-deposit"), decimal.NewFromInt(1000)))
	require.NoError(t, err)

	vb := postgres.NewVerifiedBalanceStore(pool, verifier)
	reserver := postgres.NewReserverStore(pool, ledger, vb)

	lockHeld := make(chan struct{})
	committed := make(chan error, 1)
	go func() {
		tx, err := pool.Begin(ctx)
		if err != nil {
			committed <- err
			close(lockHeld)
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()

		// PostJournal on the caller's transaction acquires the same
		// (holder, currency) advisory lock Reserve needs, and holds it until
		// this transaction ends.
		if _, err := ledger.WithDB(tx).WithAuth(attestor).PostJournal(ctx, f.spendInput(holder, postgrestest.UniqueKey("reserve-vb-toctou-spend"), decimal.NewFromInt(950))); err != nil {
			committed <- err
			close(lockHeld)
			return
		}
		close(lockHeld)

		if err := waitForBlockedAdvisoryLock(ctx, pool); err != nil {
			committed <- err
			return
		}
		committed <- tx.Commit(ctx)
	}()

	<-lockHeld

	_, err = reserver.Reserve(ctx, core.ReserveInput{
		AccountHolder:          holder,
		CurrencyUID:            f.CurrencyUID,
		Amount:                 decimal.NewFromInt(500),
		IdempotencyKey:         postgrestest.UniqueKey("reserve-vb-toctou-gated"),
		RequireVerifiedBalance: true,
	})
	require.NoError(t, <-committed, "test setup: the concurrent spend must commit")
	require.Error(t, err, "Reserve must re-derive the available base under the balance lock; the gate's pre-transaction figure can be stale-high")
	require.ErrorIs(t, err, core.ErrInsufficientBalance)

	// The spend really did land, so the refusal above is about the recomputed
	// 50 and not about the reservation having failed for some other reason.
	balance, err := postgres.NewCheckpointIntegrityStore(pool).RecomputeBalance(ctx, holder, f.CurrencyUID, f.AvailableUID)
	require.NoError(t, err)
	require.True(t, balance.Equal(decimal.NewFromInt(50)), "expected 50 after the concurrent spend, got %s", balance)
}
