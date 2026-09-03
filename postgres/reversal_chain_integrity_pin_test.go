package postgres_test

// Pin for M-2 (docs/audits/2026-09-03-independent-review/money-out.md): a
// journal linked with `reversal_of = J` that does not reverse J must not be
// able to make "reverse everything remaining" reverse less than everything
// and report success.
//
// The measured failure: append one balanced, net-zero journal carrying
// `reversal_of = J` (CR wallet 50 / DR wallet 50 / DR custodial 50 / CR
// custodial 50). It moves no money -- the holder's balance is untouched --
// but every consumer of the chain reads it as "50 of J has already been
// reversed". A platform then clawing the deposit back in full got
// `ReverseJournalFraction(J, 1, 1) -> err=nil` posting a reversal of 50, a
// holder left holding 50, and all sixteen reconciliation checks then in the
// suite green -- the seventeenth, "reversal_chain_integrity", was added for
// exactly this and has its own pin in service/.
//
// I-51 already forbids exactly this shape on the way in
// (validateReversalOfInput rules 2 and 3), and that gate is real -- the last
// case below drives the same forgery through PostJournal and watches it be
// refused. But `journals.reversal_of` is an ordinary column and `ledger_app`
// holds INSERT on `journals`, so under this repo's standing threat model the
// input gate says nothing about what is actually in the table. The rules now
// also run where the chain is CONSUMED (cumulativeReversedByDimension), and
// the reversal fails closed naming the offending journal instead of
// under-reversing silently.
//
// Every forged row below is inserted over a real socket as `ledger_app`, not
// as the test superuser: the claim is about what the application's own
// credential can do to a reversal, and a superuser INSERT would prove
// nothing about the grants.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// reversalChainFixture is one honest 100 deposit and the ids needed to append
// raw rows beside it.
type reversalChainFixture struct {
	pool         *pgxpool.Pool
	store        *postgres.LedgerStore
	attacker     *pgxpool.Pool
	holder       int64
	journalUID   string
	currencyUID  string
	walletUID    string
	custodialUID string

	journalTypeID int64
	currencyID    int64
	walletID      int64
	custodialID   int64
}

func seedReversalChainFixture(t *testing.T, ctx context.Context) reversalChainFixture {
	t.Helper()
	pool := postgrestest.SetupDB(t)
	store := postgres.NewLedgerStore(pool)

	const holder = int64(8801)
	curUID := postgrestest.SeedCurrency(t, pool, "RCU", "Reversal Chain Unit")
	jtUID := postgrestest.SeedJournalType(t, pool, "rc_deposit", "Reversal Chain Deposit")
	walletUID := postgrestest.SeedClassificationWithRole(t, pool, "rc_wallet", "RC Wallet", "debit", false, "available")
	custodialUID := postgrestest.SeedClassification(t, pool, "rc_custodial", "RC Custodial", "credit", true)

	journal, err := store.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtUID,
		IdempotencyKey: postgrestest.UniqueKey("rc-deposit"),
		Source:         "reversal-chain-pin",
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: curUID, ClassificationUID: walletUID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: curUID, ClassificationUID: custodialUID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	})
	require.NoError(t, err)

	fx := reversalChainFixture{
		pool:         pool,
		store:        store,
		attacker:     ledgerAppPool(t, ctx, pool, "reversal-chain-app-credential"),
		holder:       holder,
		journalUID:   journal.UID,
		currencyUID:  curUID,
		walletUID:    walletUID,
		custodialUID: custodialUID,
	}
	require.NoError(t, pool.QueryRow(ctx, "SELECT id FROM journal_types WHERE uid=$1", jtUID).Scan(&fx.journalTypeID))
	require.NoError(t, pool.QueryRow(ctx, "SELECT id FROM currencies WHERE uid=$1", curUID).Scan(&fx.currencyID))
	require.NoError(t, pool.QueryRow(ctx, "SELECT id FROM classifications WHERE uid=$1", walletUID).Scan(&fx.walletID))
	require.NoError(t, pool.QueryRow(ctx, "SELECT id FROM classifications WHERE uid=$1", custodialUID).Scan(&fx.custodialID))
	return fx
}

// forgeLinkedJournal appends, as ledger_app, a balanced journal carrying
// reversal_of = the fixture's deposit. legs are (holder, classification id,
// entry_type, amount) and must balance per currency or the deferred
// per-journal balance trigger (I-1) rejects the transaction -- which is the
// point: this forgery is invisible to every DB-side guard there is.
type forgedLeg struct {
	holder    int64
	classID   int64
	entryType string
	amount    string
}

func (fx reversalChainFixture) forgeLinkedJournal(t *testing.T, ctx context.Context, idemKey string, legs []forgedLeg) string {
	t.Helper()

	tx, err := fx.attacker.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	total := decimal.Zero
	for _, l := range legs {
		if l.entryType == "debit" {
			total = total.Add(decimal.RequireFromString(l.amount))
		}
	}

	var journalID int64
	var journalUID string
	var effectiveAt time.Time
	require.NoError(t, tx.QueryRow(ctx, `
		INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit, metadata, actor_id, source, effective_at, uid,
		                      auth_digest, auth_signature, auth_key_id, reversal_of)
		VALUES ($1, $2, $3::numeric, $3::numeric, '{}'::jsonb, 0, 'forged-reversal-link', now(), gen_random_uuid(),
		        ''::bytea, ''::bytea, '', (SELECT id FROM journals WHERE uid = $4))
		RETURNING id, uid, effective_at
	`, fx.journalTypeID, idemKey, total.String(), fx.journalUID).Scan(&journalID, &journalUID, &effectiveAt))

	for _, l := range legs {
		_, err = tx.Exec(ctx, `
			INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, effective_at, created_at)
			VALUES ($1, $2, $3, $4, $5, $6::numeric, $7, now())
		`, journalID, l.holder, fx.currencyID, l.classID, l.entryType, l.amount, effectiveAt)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit(ctx))
	return journalUID
}

// netZeroLegs is the reviewer's forgery: four legs that cancel out on every
// dimension, so no balance moves, while registering 50 of "already reversed"
// on the deposit's wallet dimension.
func (fx reversalChainFixture) netZeroLegs() []forgedLeg {
	system := core.SystemAccountHolder(fx.holder)
	return []forgedLeg{
		{holder: fx.holder, classID: fx.walletID, entryType: "credit", amount: "50"},
		{holder: fx.holder, classID: fx.walletID, entryType: "debit", amount: "50"},
		{holder: system, classID: fx.custodialID, entryType: "debit", amount: "50"},
		{holder: system, classID: fx.custodialID, entryType: "credit", amount: "50"},
	}
}

func (fx reversalChainFixture) balance(t *testing.T, ctx context.Context) decimal.Decimal {
	t.Helper()
	b, err := fx.store.GetBalance(ctx, fx.holder, fx.currencyUID, fx.walletUID)
	require.NoError(t, err)
	return b
}

// TestReverseJournalFraction_RefusesACorruptReversalChain is the pin. Before
// the read-side check existed, the full reversal below returned nil and
// reversed 50 of 100.
func TestReverseJournalFraction_RefusesACorruptReversalChain(t *testing.T) {
	ctx := context.Background()
	fx := seedReversalChainFixture(t, ctx)

	require.True(t, fx.balance(t, ctx).Equal(decimal.NewFromInt(100)),
		"sanity: the honest deposit must be on the books before anything is forged")

	forgedUID := fx.forgeLinkedJournal(t, ctx, postgrestest.UniqueKey("rc-forged"), fx.netZeroLegs())

	// The forgery itself moves nothing -- which is why it went unnoticed.
	require.True(t, fx.balance(t, ctx).Equal(decimal.NewFromInt(100)),
		"sanity: the forged link must not move money, or this pin is measuring something else")

	_, err := fx.store.ReverseJournalFraction(ctx, fx.journalUID, 1, 1, "clawback",
		postgrestest.UniqueKey("rc-full-reversal"))
	require.Error(t, err,
		"reversing everything on a journal whose reversal chain is forged must fail closed, not silently reverse half of it")
	assert.ErrorIs(t, err, core.ErrConflict)
	assert.Contains(t, err.Error(), forgedUID,
		"the operator has to be told WHICH journal is the bad link; a total nobody can act on is not actionable")
	assert.Contains(t, err.Error(), "reverses nothing the original posted")

	// The refusal is the whole point: the money is still where it was, and no
	// half-reversal was written to make it look otherwise.
	assert.True(t, fx.balance(t, ctx).Equal(decimal.NewFromInt(100)),
		"a refused reversal must not have posted anything")
}

// TestReverseJournal_RefusesACorruptReversalChain covers the other entry
// point a clawback reaches for. ReverseJournal never gets as far as the
// chain check: it refuses outright once ANY reversal of the journal exists
// (ledger_store.go, "use ReverseJournalFraction for further partial
// reversals"), so the forged link makes the named API fail closed for a
// different, pre-existing reason.
//
// Pinned as what it is rather than as what one would prefer: fail-closed is
// the outcome that matters, and an operator who follows that error's own
// advice lands on ReverseJournalFraction, which then names the bad link. What
// must never happen -- and is asserted here -- is this call succeeding with a
// short reversal.
func TestReverseJournal_RefusesACorruptReversalChain(t *testing.T) {
	ctx := context.Background()
	fx := seedReversalChainFixture(t, ctx)
	forgedUID := fx.forgeLinkedJournal(t, ctx, postgrestest.UniqueKey("rc-forged-full"), fx.netZeroLegs())
	require.NotEmpty(t, forgedUID)

	_, err := fx.store.ReverseJournal(ctx, fx.journalUID, "clawback")
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrConflict)
	assert.Contains(t, err.Error(), "ReverseJournalFraction",
		"the refusal has to route the operator to the call that can say what is wrong")
	assert.True(t, fx.balance(t, ctx).Equal(decimal.NewFromInt(100)))
}

// TestReverseJournalFraction_RefusesAnOverReversedChain covers the second
// rule at the same consumption point: a chain whose amounts exceed the
// original's.
//
// Honest about what this adds. A well-shaped but oversized chain was ALREADY
// refused before the read-side check existed -- reversalEntriesFor's own
// overshoot check catches it on the way out ("cumulative reversed 150 + this
// reversal's 50 would exceed original amount 100"), so no money moved either
// way. What changes is the diagnosis: the refusal now says the CHAIN is
// corrupt, at the point the sum is produced, instead of describing the
// caller's request as the thing that would overshoot. Rule 3 here is defence
// in depth over a diagnostic, not a hole being closed -- recorded as such so
// nobody later reads this pin as evidence of a money-path fix.
func TestReverseJournalFraction_RefusesAnOverReversedChain(t *testing.T) {
	ctx := context.Background()
	fx := seedReversalChainFixture(t, ctx)
	system := core.SystemAccountHolder(fx.holder)

	// A well-SHAPED reversal (every leg inverts a real one) for more than the
	// original was worth: rule 2 is satisfied, rule 3 is not.
	forgedUID := fx.forgeLinkedJournal(t, ctx, postgrestest.UniqueKey("rc-over"), []forgedLeg{
		{holder: fx.holder, classID: fx.walletID, entryType: "credit", amount: "150"},
		{holder: system, classID: fx.custodialID, entryType: "debit", amount: "150"},
	})
	require.NotEmpty(t, forgedUID)

	_, err := fx.store.ReverseJournalFraction(ctx, fx.journalUID, 1, 2, "partial clawback",
		postgrestest.UniqueKey("rc-partial"))
	require.Error(t, err, "a chain claiming more reversed than the original was worth must not be summed as fact")
	assert.ErrorIs(t, err, core.ErrConflict)
	assert.Contains(t, err.Error(), "more than the original")
}

// TestPostJournal_StillRefusesTheForgeryOnTheWayIn is the control: I-51's
// input gate is not being replaced by the read-side check, and the same
// forgery attempted through the library is still refused before it lands.
// Without this, a future change could delete the input gate and only these
// read-side pins would notice -- one release too late, with the row already
// in the table.
func TestPostJournal_StillRefusesTheForgeryOnTheWayIn(t *testing.T) {
	ctx := context.Background()
	fx := seedReversalChainFixture(t, ctx)

	var jtUID string
	require.NoError(t, fx.attacker.QueryRow(ctx, "SELECT uid FROM journal_types WHERE id=$1", fx.journalTypeID).Scan(&jtUID))

	system := core.SystemAccountHolder(fx.holder)
	_, err := fx.store.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtUID,
		IdempotencyKey: postgrestest.UniqueKey("rc-input-gate"),
		Source:         "reversal-chain-pin",
		ReversalOfUID:  fx.journalUID,
		Entries: []core.EntryInput{
			{AccountHolder: fx.holder, CurrencyUID: fx.currencyUID, ClassificationUID: fx.walletUID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(50)},
			{AccountHolder: fx.holder, CurrencyUID: fx.currencyUID, ClassificationUID: fx.walletUID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(50)},
			{AccountHolder: system, CurrencyUID: fx.currencyUID, ClassificationUID: fx.custodialUID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(50)},
			{AccountHolder: system, CurrencyUID: fx.currencyUID, ClassificationUID: fx.custodialUID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(50)},
		},
	})
	require.Error(t, err, "I-51's input gate must still refuse the net-zero link posted through the library")
	assert.ErrorIs(t, err, core.ErrInvalidInput)
	assert.Contains(t, err.Error(), "does not reverse any entry of the referenced journal")
}

// TestReverseJournalFraction_HonestPartialChainStillReverses is the
// false-positive guard: the read-side check must not turn a legitimate
// multi-step reversal into an error. Two honest halves, posted through the
// library, and the second one has to see the first as history.
func TestReverseJournalFraction_HonestPartialChainStillReverses(t *testing.T) {
	ctx := context.Background()
	fx := seedReversalChainFixture(t, ctx)

	_, err := fx.store.ReverseJournalFraction(ctx, fx.journalUID, 1, 2, "half",
		postgrestest.UniqueKey("rc-honest-half"))
	require.NoError(t, err)
	require.True(t, fx.balance(t, ctx).Equal(decimal.NewFromInt(50)), "half of 100 must be reversed, got %s", fx.balance(t, ctx))

	_, err = fx.store.ReverseJournalFraction(ctx, fx.journalUID, 1, 1, "the rest",
		postgrestest.UniqueKey("rc-honest-rest"))
	require.NoError(t, err, "a well-formed chain must remain fully reversible; the read-side check is not allowed to cost that")
	assert.True(t, fx.balance(t, ctx).IsZero(), "expected 0 after reversing the remainder, got %s", fx.balance(t, ctx))

	// And a third attempt is refused as already-fully-reversed, not as a
	// corrupt chain -- the two failures must stay distinguishable.
	_, err = fx.store.ReverseJournalFraction(ctx, fx.journalUID, 1, 1, "again",
		postgrestest.UniqueKey("rc-honest-again"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already fully reversed",
		fmt.Sprintf("expected the already-reversed refusal, got: %v", err))
}

// TestCorruptReversalLinks_FindsTheForgedLink is the SQL half of the
// reversal_chain_integrity reconcile check (I-51): the fleet scan has to see
// the same forgery the read-side gate refuses, without anyone attempting a
// reversal first.
//
// This is what makes the check a detection layer rather than a restatement.
// Both existing enforcement points fire only when somebody posts or reverses
// something; between the forgery landing and the next clawback, nothing
// looks. Here nothing is posted after the forgery at all -- the scan is run
// straight against the table.
func TestCorruptReversalLinks_FindsTheForgedLink(t *testing.T) {
	ctx := context.Background()
	fx := seedReversalChainFixture(t, ctx)
	adapter := postgres.NewReconcileAdapter(fx.pool)

	clean, err := adapter.CorruptReversalLinks(ctx, 200)
	require.NoError(t, err)
	require.Empty(t, clean, "sanity: an honest ledger must produce no findings, or every assertion below is meaningless")

	forgedUID := fx.forgeLinkedJournal(t, ctx, postgrestest.UniqueKey("rc-scan-forged"), fx.netZeroLegs())

	rows, err := adapter.CorruptReversalLinks(ctx, 200)
	require.NoError(t, err)
	// Two, not one: the forgery puts a leg on BOTH sides of both dimensions,
	// so the wallet's extra DEBIT leg and the custodial's extra CREDIT leg
	// each flip onto a dimension the deposit never posted. Asserted as two
	// rather than "at least one" so a later change that starts reporting
	// only the first offending leg per journal shows up here.
	require.Len(t, rows, 2, "both extra legs land on dimensions the original never posted: %+v", rows)

	for _, r := range rows {
		assert.Equal(t, "unmatched_dimension", r.Violation)
		assert.Equal(t, forgedUID, r.ReversalUID, "the finding has to name the forged journal, not just the original")
		assert.Equal(t, fx.journalUID, r.OriginalUID)
		assert.True(t, r.ReversedAmount.Equal(decimal.NewFromInt(50)), "got %s", r.ReversedAmount)
	}

	// The holder-side row, spelled out: the deposit posted a DEBIT on the
	// wallet, so a credit dimension there is one the original never touched.
	// (The other row is its system-side mirror on the custodial account.)
	holderSide := rows[1]
	if rows[0].AccountHolder == fx.holder {
		holderSide = rows[0]
	}
	assert.Equal(t, fx.holder, holderSide.AccountHolder)
	assert.Equal(t, "credit", holderSide.EntryType,
		"the reported dimension is the ORIGINAL's grain: the forged DEBIT leg flips to a credit dimension the deposit never posted")
}

// TestCorruptReversalLinks_FindsAnOverReversedChain covers the other
// violation, and pins the deliberate choice not to name a journal for it.
func TestCorruptReversalLinks_FindsAnOverReversedChain(t *testing.T) {
	ctx := context.Background()
	fx := seedReversalChainFixture(t, ctx)
	adapter := postgres.NewReconcileAdapter(fx.pool)
	system := core.SystemAccountHolder(fx.holder)

	// Well-shaped (every leg inverts a real one) but for more than the
	// original was worth.
	fx.forgeLinkedJournal(t, ctx, postgrestest.UniqueKey("rc-scan-over"), []forgedLeg{
		{holder: fx.holder, classID: fx.walletID, entryType: "credit", amount: "150"},
		{holder: system, classID: fx.custodialID, entryType: "debit", amount: "150"},
	})

	rows, err := adapter.CorruptReversalLinks(ctx, 200)
	require.NoError(t, err)
	require.Len(t, rows, 2, "both of the original's dimensions are over-reversed: %+v", rows)

	for _, r := range rows {
		assert.Equal(t, "over_reversed", r.Violation)
		assert.Equal(t, fx.journalUID, r.OriginalUID)
		assert.Empty(t, r.ReversalUID,
			"an overshoot is a property of the total; naming whichever journal happened to be last would point at the wrong row")
		assert.True(t, r.ReversedAmount.Equal(decimal.NewFromInt(150)), "got %s", r.ReversedAmount)
		assert.True(t, r.OriginalAmount.Equal(decimal.NewFromInt(100)), "got %s", r.OriginalAmount)
	}
}

// TestCorruptReversalLinks_HonestPartialReversalsAreNotFindings is the
// false-positive guard. A check that fires on legitimate partial reversals
// would be turned off within a week, and then it protects nothing.
func TestCorruptReversalLinks_HonestPartialReversalsAreNotFindings(t *testing.T) {
	ctx := context.Background()
	fx := seedReversalChainFixture(t, ctx)
	adapter := postgres.NewReconcileAdapter(fx.pool)

	_, err := fx.store.ReverseJournalFraction(ctx, fx.journalUID, 1, 3, "first third",
		postgrestest.UniqueKey("rc-scan-third"))
	require.NoError(t, err)
	_, err = fx.store.ReverseJournalFraction(ctx, fx.journalUID, 1, 1, "the rest",
		postgrestest.UniqueKey("rc-scan-rest"))
	require.NoError(t, err)
	require.True(t, fx.balance(t, ctx).IsZero(), "sanity: the journal must be fully reversed, got %s", fx.balance(t, ctx))

	rows, err := adapter.CorruptReversalLinks(ctx, 200)
	require.NoError(t, err)
	assert.Empty(t, rows, "a fully and honestly reversed journal is not a corrupt chain: %+v", rows)
}
