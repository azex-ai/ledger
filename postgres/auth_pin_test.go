package postgres_test

// P5 pin tests: per-journal authorization signing
// (docs/plans/2026-08-21-tamper-evident-ledger-design.md §7, I-26,
// docs/plans/2026-08-21-integrity-hardening-contracts.md §9 item 3). These
// match the M5 attack scenario the design doc singles out as its most
// valuable finding: a raw-SQL-privileged attacker can insert a perfectly
// balanced, perfectly valid-looking journal that every OTHER invariant
// (I-1 through I-25) accepts -- per-journal signing is the one mechanism
// that still tells it apart from a genuine posting.

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/authdev"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// authFixture is P5's own self-contained fixture (deliberately not sharing
// setupInvariantsFixture in invariants_test.go -- contracts §3 assigns P5
// no shared query files, and there is no reason to couple to another
// task's fixture helper).
type authFixture struct {
	CurrencyUID     string
	MainWalletUID   string
	CustodialUID    string
	JournalTypeUID  string
	JournalTypeCode string

	currencyID    int64
	mainWalletID  int64
	custodialID   int64
	journalTypeID int64
}

func setupAuthFixture(t testing.TB, pool *pgxpool.Pool, ctx context.Context) authFixture {
	t.Helper()

	classStore := postgres.NewClassificationStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)
	suffix := time.Now().UnixNano()

	cur, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{
		Code: fmt.Sprintf("AUTHC_%d", suffix), Name: "Auth Test Currency", Exponent: 18,
	})
	require.NoError(t, err)
	mw, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: fmt.Sprintf("auth_main_%d", suffix), Name: "Auth Main Wallet", NormalSide: core.NormalSideDebit,
	})
	require.NoError(t, err)
	cust, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: fmt.Sprintf("auth_custodial_%d", suffix), Name: "Auth Custodial", NormalSide: core.NormalSideCredit, IsSystem: true,
	})
	require.NoError(t, err)
	code := fmt.Sprintf("auth_jt_%d", suffix)
	jt, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{Code: code, Name: "Auth Test Journal Type"})
	require.NoError(t, err)

	f := authFixture{
		CurrencyUID: cur.UID, MainWalletUID: mw.UID, CustodialUID: cust.UID,
		JournalTypeUID: jt.UID, JournalTypeCode: code,
	}
	require.NoError(t, pool.QueryRow(ctx, "SELECT id FROM currencies WHERE uid=$1", cur.UID).Scan(&f.currencyID))
	require.NoError(t, pool.QueryRow(ctx, "SELECT id FROM classifications WHERE uid=$1", mw.UID).Scan(&f.mainWalletID))
	require.NoError(t, pool.QueryRow(ctx, "SELECT id FROM classifications WHERE uid=$1", cust.UID).Scan(&f.custodialID))
	require.NoError(t, pool.QueryRow(ctx, "SELECT id FROM journal_types WHERE uid=$1", jt.UID).Scan(&f.journalTypeID))
	return f
}

func (f authFixture) journalInput(userID int64, idemKey string, amount decimal.Decimal) core.JournalInput {
	return core.JournalInput{
		JournalTypeUID: f.JournalTypeUID,
		IdempotencyKey: idemKey,
		Source:         "auth-pin-test",
		ActorID:        userID,
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyUID: f.CurrencyUID, ClassificationUID: f.MainWalletUID, EntryType: core.EntryTypeDebit, Amount: amount},
			{AccountHolder: core.SystemAccountHolder(userID), CurrencyUID: f.CurrencyUID, ClassificationUID: f.CustodialUID, EntryType: core.EntryTypeCredit, Amount: amount},
		},
	}
}

func fetchAuthColumns(t testing.TB, pool *pgxpool.Pool, ctx context.Context, journalUID string) (digest, signature []byte, keyID string, effectiveAt time.Time) {
	t.Helper()
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT auth_digest, auth_signature, auth_key_id, effective_at FROM journals WHERE uid=$1", journalUID,
	).Scan(&digest, &signature, &keyID, &effectiveAt))
	return
}

// countingAttestor wraps a real Attestor and counts Sign calls, so tests
// can assert idempotent replay does NOT trigger a second KMS round trip
// (design doc §7.3: "same key + same payload -> digest same -> reuse,
// don't resign").
type countingAttestor struct {
	inner     core.Attestor
	signCalls atomic.Int64
}

func (a *countingAttestor) Sign(ctx context.Context, digest []byte) ([]byte, string, error) {
	a.signCalls.Add(1)
	return a.inner.Sign(ctx, digest)
}

// failingAttestor always errors, to exercise AttestorFailureMode.
type failingAttestor struct{}

func (failingAttestor) Sign(ctx context.Context, digest []byte) ([]byte, string, error) {
	return nil, "", fmt.Errorf("failingAttestor: simulated KMS outage")
}

// TestPostJournal_SignsWithConfiguredAttestor is the positive-path pin for
// I-26: a journal posted through PostJournal with an Attestor configured
// carries a signature that VerifyJournalAuth accepts.
func TestPostJournal_SignsWithConfiguredAttestor(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAuthFixture(t, pool, ctx)

	attestor, verifier, err := authdev.NewInsecureLocalAttestor("dev-ed25519-pin-1")
	require.NoError(t, err)

	store, err := postgres.NewLedgerStore(pool).WithAuth(attestor, core.AuthPolicy{
		FailureMode: core.AttestorFailureModeFailClosed,
		Coverage:    map[string]core.SignatureRequirement{f.JournalTypeCode: core.SignatureRequirementRequired},
	})
	require.NoError(t, err)

	input := f.journalInput(8001, postgrestest.UniqueKey("auth-signed"), decimal.NewFromInt(100))
	j, err := store.PostJournal(ctx, input)
	require.NoError(t, err)

	digest, signature, keyID, effectiveAt := fetchAuthColumns(t, pool, ctx, j.UID)
	require.NotEmpty(t, digest, "auth_digest must be populated when an Attestor is configured")
	require.NotEmpty(t, signature, "auth_signature must be populated when an Attestor is configured")
	require.Equal(t, "dev-ed25519-pin-1", keyID)

	err = core.VerifyJournalAuth(ctx, verifier, input, effectiveAt, digest, signature, keyID)
	require.NoError(t, err, "a genuinely signed journal must pass VerifyJournalAuth")
}

// TestPostJournal_UnsignedWithoutAttestor pins design doc §12's P5 row:
// Attestor == nil (never calling WithAuth) must leave PostJournal's
// behavior unchanged from before P5 existed -- auth columns stay empty,
// no error.
func TestPostJournal_UnsignedWithoutAttestor(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAuthFixture(t, pool, ctx)

	store := postgres.NewLedgerStore(pool) // no WithAuth call
	input := f.journalInput(8002, postgrestest.UniqueKey("auth-unsigned"), decimal.NewFromInt(50))
	j, err := store.PostJournal(ctx, input)
	require.NoError(t, err)

	digest, signature, keyID, _ := fetchAuthColumns(t, pool, ctx, j.UID)
	require.Empty(t, digest)
	require.Empty(t, signature)
	require.Empty(t, keyID)
}

// TestPostJournal_IdempotentReplayDoesNotResign pins design doc §7.3: a
// retried PostJournal call with the same idempotency key and payload must
// not trigger a second Attestor.Sign call -- the existing journal (and its
// existing signature) is returned instead.
func TestPostJournal_IdempotentReplayDoesNotResign(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAuthFixture(t, pool, ctx)

	realAttestor, _, err := authdev.NewInsecureLocalAttestor("dev-ed25519-pin-2")
	require.NoError(t, err)
	counting := &countingAttestor{inner: realAttestor}

	store, err := postgres.NewLedgerStore(pool).WithAuth(counting, core.AuthPolicy{
		FailureMode: core.AttestorFailureModeFailClosed,
		Coverage:    map[string]core.SignatureRequirement{f.JournalTypeCode: core.SignatureRequirementRequired},
	})
	require.NoError(t, err)

	input := f.journalInput(8003, postgrestest.UniqueKey("auth-replay"), decimal.NewFromInt(25))

	first, err := store.PostJournal(ctx, input)
	require.NoError(t, err)
	require.EqualValues(t, 1, counting.signCalls.Load(), "first post must sign exactly once")

	second, err := store.PostJournal(ctx, input)
	require.NoError(t, err)
	require.Equal(t, first.UID, second.UID, "replay must return the original journal")
	require.EqualValues(t, 1, counting.signCalls.Load(), "replay must not trigger a second Sign call")
}

// TestPostJournal_MissingCoverageDecisionIsRejected pins AuthPolicy's
// "no types not listed are exempt" fence (design doc §14 item 2): a
// journal type with no explicit Coverage entry is refused, not silently
// signed or silently skipped.
func TestPostJournal_MissingCoverageDecisionIsRejected(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAuthFixture(t, pool, ctx)

	attestor, _, err := authdev.NewInsecureLocalAttestor("dev-ed25519-pin-3")
	require.NoError(t, err)

	store, err := postgres.NewLedgerStore(pool).WithAuth(attestor, core.AuthPolicy{
		FailureMode: core.AttestorFailureModeFailClosed,
		Coverage:    map[string]core.SignatureRequirement{}, // f.JournalTypeCode deliberately absent
	})
	require.NoError(t, err)

	input := f.journalInput(8004, postgrestest.UniqueKey("auth-no-coverage"), decimal.NewFromInt(10))
	_, err = store.PostJournal(ctx, input)
	require.Error(t, err)
	require.ErrorIs(t, err, core.ErrInvalidInput)
}

// TestPostJournal_FailOpenPostsUnsignedOnAttestorError pins
// AttestorFailureModeFailOpen: when the Attestor errors, the journal still
// posts, unsigned.
func TestPostJournal_FailOpenPostsUnsignedOnAttestorError(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAuthFixture(t, pool, ctx)

	store, err := postgres.NewLedgerStore(pool).WithAuth(failingAttestor{}, core.AuthPolicy{
		FailureMode: core.AttestorFailureModeFailOpen,
		Coverage:    map[string]core.SignatureRequirement{f.JournalTypeCode: core.SignatureRequirementRequired},
	})
	require.NoError(t, err)

	input := f.journalInput(8005, postgrestest.UniqueKey("auth-fail-open"), decimal.NewFromInt(15))
	j, err := store.PostJournal(ctx, input)
	require.NoError(t, err, "fail-open must let the write through despite the Attestor erroring")

	digest, signature, keyID, _ := fetchAuthColumns(t, pool, ctx, j.UID)
	require.Empty(t, digest)
	require.Empty(t, signature)
	require.Empty(t, keyID)
}

// TestPostJournal_FailClosedRejectsOnAttestorError pins
// AttestorFailureModeFailClosed: when the Attestor errors, the write is
// rejected entirely (wrapping core.ErrAttestorUnavailable) -- nothing is
// persisted.
func TestPostJournal_FailClosedRejectsOnAttestorError(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAuthFixture(t, pool, ctx)

	store, err := postgres.NewLedgerStore(pool).WithAuth(failingAttestor{}, core.AuthPolicy{
		FailureMode: core.AttestorFailureModeFailClosed,
		Coverage:    map[string]core.SignatureRequirement{f.JournalTypeCode: core.SignatureRequirementRequired},
	})
	require.NoError(t, err)

	idemKey := postgrestest.UniqueKey("auth-fail-closed")
	input := f.journalInput(8006, idemKey, decimal.NewFromInt(20))
	_, err = store.PostJournal(ctx, input)
	require.Error(t, err)
	require.ErrorIs(t, err, core.ErrAttestorUnavailable)

	var count int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM journals WHERE idempotency_key=$1", idemKey).Scan(&count))
	require.Zero(t, count, "fail-closed must not persist anything when signing fails")
}

// TestForgedDirectSQLJournalIsUnauthorized is the M5 pin (design doc §2
// "评审确认的缺陷清单" / §11 "篡改测试": "插入无签名 journal -> P5 拒绝"). It
// bypasses PostJournal entirely -- exactly the attacker model this whole
// wave defends against (a database write credential, not an app API call)
// -- and inserts a journal + two perfectly balanced entries directly via
// SQL. Every OTHER invariant in this codebase (I-1 through I-25: FK
// integrity, per-currency balance, append-only triggers, account holder
// sign convention...) is satisfied by construction. Only the empty
// auth_digest/auth_signature/auth_key_id -- and the VerifyJournalAuth check
// that reads them -- tell this journal apart from a genuine one.
func TestForgedDirectSQLJournalIsUnauthorized(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAuthFixture(t, pool, ctx)

	const forgedUserID int64 = 8007
	forgedAmount := "1000000.000000000000000000" // an attacker minting a large balanced pair

	var journalID int64
	var journalUID string
	var effectiveAt time.Time
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit, metadata, actor_id, source, event_id, effective_at, uid, auth_digest, auth_signature, auth_key_id)
		VALUES ($1, $2, $3::numeric, $3::numeric, '{}'::jsonb, 0, 'forged-direct-sql', 0, now(), gen_random_uuid(), ''::bytea, ''::bytea, '')
		RETURNING id, uid, effective_at
	`, f.journalTypeID, postgrestest.UniqueKey("forged-direct-sql"), forgedAmount).Scan(&journalID, &journalUID, &effectiveAt))

	_, err := pool.Exec(ctx, `
		INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, effective_at, created_at)
		VALUES ($1, $2, $3, $4, 'debit', $5::numeric, $6, now())
	`, journalID, forgedUserID, f.currencyID, f.mainWalletID, forgedAmount, effectiveAt)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, effective_at, created_at)
		VALUES ($1, $2, $3, $4, 'credit', $5::numeric, $6, now())
	`, journalID, core.SystemAccountHolder(forgedUserID), f.currencyID, f.custodialID, forgedAmount, effectiveAt)
	require.NoError(t, err)

	// Sanity: the forged journal really is balanced per the existing
	// per-journal check (the mechanism M1/P3 relies on) -- this forged
	// journal does NOT trip any pre-existing balance guard.
	var unbalancedCount int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT currency_id FROM journal_entries WHERE journal_id = $1
			GROUP BY currency_id
			HAVING SUM(CASE WHEN entry_type = 'debit' THEN amount ELSE -amount END) <> 0
		) t
	`, journalID).Scan(&unbalancedCount))
	require.Zero(t, unbalancedCount, "the forged journal must be a realistic forgery: balanced, not a strawman")

	digest, signature, keyID, storedEffectiveAt := fetchAuthColumns(t, pool, ctx, journalUID)
	require.Empty(t, digest)
	require.Empty(t, signature)
	require.Empty(t, keyID)

	_, verifier, err := authdev.NewInsecureLocalAttestor("dev-ed25519-forged-check")
	require.NoError(t, err)
	// The exact JournalInput reconstruction is irrelevant here: VerifyJournalAuth
	// rejects on the empty stored digest before it would even reach the
	// verifier -- see core.VerifyJournalAuth's doc comment.
	placeholderInput := f.journalInput(forgedUserID, "placeholder", decimal.NewFromInt(1))
	err = core.VerifyJournalAuth(ctx, verifier, placeholderInput, storedEffectiveAt, digest, signature, keyID)
	require.Error(t, err)
	require.ErrorIs(t, err, core.ErrUnauthorizedJournal, "a forged, unsigned journal must be flagged unauthorized even though every other invariant passes")
}
