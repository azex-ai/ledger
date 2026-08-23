package postgres_test

// Pin tests for board #15 (W2-T1, docs/plans/2026-08-21-integrity-hardening-contracts.md
// "Wave 2 契约层"): extending per-journal authorization signing (design doc
// §7, P5, and its RunInTx-gap fix §7.5, board #12/#13) to ReverseJournal,
// ReverseJournalFraction, and ExecuteTemplateBatch. Before this fix all
// three ALWAYS posted core.AuthStatusUnsignedTxMode, even in pool mode where
// PostJournal itself was already able to sign -- the gap W2-1's ruling
// (docs/plans/2026-08-21-integrity-hardening-contracts.md, Wave 2 §W2-1)
// exists to close: a verified balance is UNDEFINED for any account touched
// by an unsigned contributing journal, and reversals/batches sit squarely on
// the money path.
//
// See core.JournalWriter.AuthorizeReversal's doc comment for the mechanism
// (pre-authorize outside any transaction, re-derive and digest-compare under
// the original journal's row lock, reject on mismatch rather than silently
// posting unsigned or re-signing inside a transaction).

import (
	"context"
	"sync"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// flipReversalEntries builds the entries a full reversal (no prior
// reversals) of orig would carry: same accounts/currency/classification/
// amount, debit and credit swapped. Mirrors reversalEntriesFor's num==den
// branch with an empty alreadyReversed map -- see that function's doc
// comment.
func flipReversalEntries(orig []core.EntryInput) []core.EntryInput {
	out := make([]core.EntryInput, len(orig))
	for i, e := range orig {
		flipped := core.EntryTypeCredit
		if e.EntryType == core.EntryTypeCredit {
			flipped = core.EntryTypeDebit
		}
		out[i] = core.EntryInput{
			AccountHolder:     e.AccountHolder,
			CurrencyUID:       e.CurrencyUID,
			ClassificationUID: e.ClassificationUID,
			EntryType:         flipped,
			Amount:            e.Amount,
		}
	}
	return out
}

// TestReverseJournal_SignsWithConfiguredAttestor pins the fix itself: in
// pool mode with an Attestor configured, ReverseJournal now signs (was
// unconditionally unsigned_tx_mode before board #15). Falsification: reverting
// ReverseJournal to call reverseJournalWithQueries without preAuth (the
// pre-#15 form) reproduces auth_status = unsigned_tx_mode here.
func TestReverseJournal_SignsWithConfiguredAttestor(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAuthFixture(t, pool, ctx)

	attestor, verifier := newTestAttestor(t, "ed25519-reversal-full-signed")
	store := postgres.NewLedgerStore(pool).WithAuth(attestor)

	input := f.journalInput(8201, postgrestest.UniqueKey("auth-reversal-full-orig"), decimal.NewFromInt(100))
	orig, err := store.PostJournal(ctx, input)
	require.NoError(t, err)

	rev, err := store.ReverseJournal(ctx, orig.UID, "pin test full reversal")
	require.NoError(t, err)

	assert.Equal(t, core.AuthStatusSigned, rev.AuthStatus)
	require.NotEmpty(t, rev.AuthDigest, "auth_digest must be populated once ReverseJournal signs")
	require.NotEmpty(t, rev.AuthSignature)
	assert.Equal(t, "ed25519-reversal-full-signed", rev.AuthKeyID)

	reversedInput := core.JournalInput{
		JournalTypeUID: input.JournalTypeUID,
		IdempotencyKey: rev.IdempotencyKey,
		Entries:        flipReversalEntries(input.Entries),
		Source:         "reversal",
		ReversalOfUID:  orig.UID,
	}
	err = core.VerifyJournalAuth(ctx, verifier, reversedInput, rev.EffectiveAt, rev.AuthDigest, rev.AuthSignature, rev.AuthKeyID)
	require.NoError(t, err, "a genuinely signed reversal must pass VerifyJournalAuth")
}

// TestReverseJournal_UnsignedNoAttestorInPoolMode pins the narrowed label:
// pool mode with NO Attestor configured must report unsigned_no_attestor
// (matching PostJournal's own pool-mode-no-attestor labeling), not
// unsigned_tx_mode -- "no attestor" and "no safe point because of tx mode"
// are different reasons and board #15 stops conflating them for reversals in
// pool mode (see AuthStatusUnsignedTxMode's updated doc comment).
func TestReverseJournal_UnsignedNoAttestorInPoolMode(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAuthFixture(t, pool, ctx)

	store := postgres.NewLedgerStore(pool) // no WithAuth
	input := f.journalInput(8202, postgrestest.UniqueKey("auth-reversal-noattestor-orig"), decimal.NewFromInt(100))
	orig, err := store.PostJournal(ctx, input)
	require.NoError(t, err)

	rev, err := store.ReverseJournal(ctx, orig.UID, "pin test no attestor")
	require.NoError(t, err)
	assert.Equal(t, core.AuthStatusUnsignedNoAttestor, rev.AuthStatus)
	assert.Empty(t, rev.AuthDigest)
}

// TestReverseJournal_TxMode_NeverSignsEvenWithAttestor is the negative
// contrast: a store bound via WithDB (participating in a caller-owned
// transaction, e.g. a RunInTx callback) still never signs a reversal, even
// with an Attestor configured -- there is genuinely no safe point to call
// out to the Attestor there (financial.md), and board #15 does not add a
// PostAuthorized-equivalent entrypoint for reversals.
func TestReverseJournal_TxMode_NeverSignsEvenWithAttestor(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAuthFixture(t, pool, ctx)

	attestor, _ := newTestAttestor(t, "ed25519-reversal-txmode")
	store := postgres.NewLedgerStore(pool).WithAuth(attestor)

	input := f.journalInput(8203, postgrestest.UniqueKey("auth-reversal-txmode-orig"), decimal.NewFromInt(100))
	orig, err := store.PostJournal(ctx, input)
	require.NoError(t, err)

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	txStore := store.WithDB(tx)

	rev, err := txStore.ReverseJournal(ctx, orig.UID, "pin test tx mode")
	require.NoError(t, err)
	assert.Equal(t, core.AuthStatusUnsignedTxMode, rev.AuthStatus)
	assert.Empty(t, rev.AuthDigest)
}

// TestReverseJournalFraction_SignsWithConfiguredAttestor covers BOTH
// branches reversalEntriesFor implements: a general num!=den proportional
// split, then a num==den ("reverse everything remaining") completion --
// exercising the exact two shapes AuthorizeReversal / the post-time digest
// comparison must agree on.
func TestReverseJournalFraction_SignsWithConfiguredAttestor(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	attestor, verifier := newTestAttestor(t, "ed25519-fraction-signed")
	store := postgres.NewLedgerStore(pool).WithAuth(attestor)

	curID := postgrestest.SeedCurrencyWithExponent(t, pool, "USDC", "USD Coin", 2)
	jtID := postgrestest.SeedJournalType(t, pool, "transfer", "Transfer")
	clsWallet := postgrestest.SeedClassification(t, pool, "main_wallet", "Main Wallet", "credit", false)
	clsCustodial := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "debit", true)

	amount := decimal.RequireFromString("100.01")
	orig, err := store.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("frac-signed-base"),
		Entries: []core.EntryInput{
			{AccountHolder: 71, CurrencyUID: curID, ClassificationUID: clsWallet, EntryType: core.EntryTypeCredit, Amount: amount},
			{AccountHolder: -71, CurrencyUID: curID, ClassificationUID: clsCustodial, EntryType: core.EntryTypeDebit, Amount: amount},
		},
	})
	require.NoError(t, err)

	// num != den: general proportional-split branch. 100.01 * 1/3 rounds
	// HalfUp at exponent 2 to 33.34 (same math as
	// TestReverseJournalFraction_ConservationAndRemainderCompletion).
	partial, err := store.ReverseJournalFraction(ctx, orig.UID, 1, 3, "partial refund", postgrestest.UniqueKey("frac-signed-partial"))
	require.NoError(t, err)
	assert.Equal(t, core.AuthStatusSigned, partial.AuthStatus)
	require.NotEmpty(t, partial.AuthSignature)

	partialInput := core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: partial.IdempotencyKey,
		Entries: []core.EntryInput{
			{AccountHolder: 71, CurrencyUID: curID, ClassificationUID: clsWallet, EntryType: core.EntryTypeDebit, Amount: decimal.RequireFromString("33.34")},
			{AccountHolder: -71, CurrencyUID: curID, ClassificationUID: clsCustodial, EntryType: core.EntryTypeCredit, Amount: decimal.RequireFromString("33.34")},
		},
		Source:        "reversal",
		ReversalOfUID: orig.UID,
	}
	err = core.VerifyJournalAuth(ctx, verifier, partialInput, partial.EffectiveAt, partial.AuthDigest, partial.AuthSignature, partial.AuthKeyID)
	require.NoError(t, err)

	// num == den (1/1): "reverse everything remaining" branch. Remaining =
	// 100.01 - 33.34 = 66.67.
	remaining, err := store.ReverseJournalFraction(ctx, orig.UID, 1, 1, "remaining refund", postgrestest.UniqueKey("frac-signed-remaining"))
	require.NoError(t, err)
	assert.Equal(t, core.AuthStatusSigned, remaining.AuthStatus)

	remainingInput := core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: remaining.IdempotencyKey,
		Entries: []core.EntryInput{
			{AccountHolder: 71, CurrencyUID: curID, ClassificationUID: clsWallet, EntryType: core.EntryTypeDebit, Amount: decimal.RequireFromString("66.67")},
			{AccountHolder: -71, CurrencyUID: curID, ClassificationUID: clsCustodial, EntryType: core.EntryTypeCredit, Amount: decimal.RequireFromString("66.67")},
		},
		Source:        "reversal",
		ReversalOfUID: orig.UID,
	}
	err = core.VerifyJournalAuth(ctx, verifier, remainingInput, remaining.EffectiveAt, remaining.AuthDigest, remaining.AuthSignature, remaining.AuthKeyID)
	require.NoError(t, err)
}

// TestReverseJournalFraction_UnsignedNoAttestorInPoolMode is
// ReverseJournal_UnsignedNoAttestorInPoolMode's fraction-form counterpart.
func TestReverseJournalFraction_UnsignedNoAttestorInPoolMode(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	store := postgres.NewLedgerStore(pool) // no attestor

	curID := postgrestest.SeedCurrencyWithExponent(t, pool, "USDC", "USD Coin", 2)
	jtID := postgrestest.SeedJournalType(t, pool, "transfer", "Transfer")
	clsWallet := postgrestest.SeedClassification(t, pool, "main_wallet", "Main Wallet", "credit", false)
	clsCustodial := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "debit", true)

	amount := decimal.RequireFromString("100.01")
	orig, err := store.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("frac-noattestor-base"),
		Entries: []core.EntryInput{
			{AccountHolder: 72, CurrencyUID: curID, ClassificationUID: clsWallet, EntryType: core.EntryTypeCredit, Amount: amount},
			{AccountHolder: -72, CurrencyUID: curID, ClassificationUID: clsCustodial, EntryType: core.EntryTypeDebit, Amount: amount},
		},
	})
	require.NoError(t, err)

	rev, err := store.ReverseJournalFraction(ctx, orig.UID, 1, 1, "full", postgrestest.UniqueKey("frac-noattestor-rev"))
	require.NoError(t, err)
	assert.Equal(t, core.AuthStatusUnsignedNoAttestor, rev.AuthStatus)
}

// TestReverseJournalFraction_TxMode_NeverSignsEvenWithAttestor is
// ReverseJournal_TxMode_NeverSignsEvenWithAttestor's fraction-form counterpart.
func TestReverseJournalFraction_TxMode_NeverSignsEvenWithAttestor(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	attestor, _ := newTestAttestor(t, "ed25519-fraction-txmode")
	store := postgres.NewLedgerStore(pool).WithAuth(attestor)

	curID := postgrestest.SeedCurrencyWithExponent(t, pool, "USDC", "USD Coin", 2)
	jtID := postgrestest.SeedJournalType(t, pool, "transfer", "Transfer")
	clsWallet := postgrestest.SeedClassification(t, pool, "main_wallet", "Main Wallet", "credit", false)
	clsCustodial := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "debit", true)

	amount := decimal.RequireFromString("100.01")
	orig, err := store.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("frac-txmode-base"),
		Entries: []core.EntryInput{
			{AccountHolder: 73, CurrencyUID: curID, ClassificationUID: clsWallet, EntryType: core.EntryTypeCredit, Amount: amount},
			{AccountHolder: -73, CurrencyUID: curID, ClassificationUID: clsCustodial, EntryType: core.EntryTypeDebit, Amount: amount},
		},
	})
	require.NoError(t, err)

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	txStore := store.WithDB(tx)

	rev, err := txStore.ReverseJournalFraction(ctx, orig.UID, 1, 1, "full", postgrestest.UniqueKey("frac-txmode-rev"))
	require.NoError(t, err)
	assert.Equal(t, core.AuthStatusUnsignedTxMode, rev.AuthStatus)
}

// blockingAttestor wraps a real core.Attestor and blocks inside Sign until
// proceed is closed, signaling started exactly once first. Used to
// deterministically land a concurrent partial reversal inside the window
// between AuthorizeReversal computing its digest and ReverseJournalFraction
// opening its transaction -- see
// TestReverseJournalFraction_ConcurrentPartialReversalInvalidatesPreAuthorization.
type blockingAttestor struct {
	inner   core.Attestor
	started chan struct{}
	proceed chan struct{}
	once    sync.Once
}

func (a *blockingAttestor) Sign(ctx context.Context, digest []byte) ([]byte, string, error) {
	a.once.Do(func() { close(a.started) })
	<-a.proceed
	return a.inner.Sign(ctx, digest)
}

// TestReverseJournalFraction_ConcurrentPartialReversalInvalidatesPreAuthorization
// pins the race design doc §7.5/board #15 documents (core.JournalWriter.
// AuthorizeReversal's doc comment, reversalEntriesFor's doc comment):
// AuthorizeReversal's num==den ("reverse everything remaining") form derives
// its entries from CURRENT reversal history, so a partial reversal landing
// after AuthorizeReversal ran but before the post's row lock is taken
// invalidates the pre-computed digest. This is NOT a signing bug -- the
// post must fail outright (never silently post unsigned, never silently
// re-sign inside the transaction, never use the stale signature): intent
// invalidated means the caller must retry, exactly as the conservation
// checks (I-2) already required before board #15 existed.
//
// Mechanics: storeA (a blockingAttestor clone) starts a 1/1 reversal and
// blocks inside Sign(), which runs strictly BEFORE ReverseJournalFraction
// opens its own transaction (AuthorizeReversal is called first, in
// ReverseJournalFraction's dispatch function, before pool.Begin). While A is
// blocked there, storeB (an unblocked clone of the SAME underlying store --
// same pool, same rows) commits a real 1/3 partial reversal. Releasing A
// lets its stale, pre-race signature reach the post path, which must reject
// it.
func TestReverseJournalFraction_ConcurrentPartialReversalInvalidatesPreAuthorization(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	realAttestor, _ := newTestAttestor(t, "ed25519-race")
	curID := postgrestest.SeedCurrencyWithExponent(t, pool, "USDC", "USD Coin", 2)
	jtID := postgrestest.SeedJournalType(t, pool, "transfer", "Transfer")
	clsWallet := postgrestest.SeedClassification(t, pool, "main_wallet", "Main Wallet", "credit", false)
	clsCustodial := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "debit", true)

	base := postgres.NewLedgerStore(pool)
	amount := decimal.RequireFromString("100.01")
	orig, err := base.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("race-base"),
		Entries: []core.EntryInput{
			{AccountHolder: 81, CurrencyUID: curID, ClassificationUID: clsWallet, EntryType: core.EntryTypeCredit, Amount: amount},
			{AccountHolder: -81, CurrencyUID: curID, ClassificationUID: clsCustodial, EntryType: core.EntryTypeDebit, Amount: amount},
		},
	})
	require.NoError(t, err)

	blocking := &blockingAttestor{inner: realAttestor, started: make(chan struct{}), proceed: make(chan struct{})}
	storeA := base.WithAuth(blocking)
	storeB := base.WithAuth(realAttestor)

	raceRemainingKey := postgrestest.UniqueKey("race-remaining")
	var revA *core.Journal
	var errA error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		revA, errA = storeA.ReverseJournalFraction(ctx, orig.UID, 1, 1, "race remaining", raceRemainingKey)
	}()

	<-blocking.started

	_, err = storeB.ReverseJournalFraction(ctx, orig.UID, 1, 3, "race partial", postgrestest.UniqueKey("race-partial"))
	require.NoError(t, err, "the concurrent partial reversal must land while A is blocked")

	close(blocking.proceed)
	wg.Wait()

	require.Error(t, errA, "A's stale pre-authorization must be rejected outright")
	assert.ErrorIs(t, errA, core.ErrConflict)
	assert.Nil(t, revA)

	var count int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM journals WHERE idempotency_key=$1", raceRemainingKey).Scan(&count))
	assert.Equal(t, 0, count, "a rejected post must leave no row behind")
}

// TestExecuteTemplateBatch_SignsWithConfiguredAttestor pins the fix for
// ExecuteTemplateBatch's pool-mode path: render + sign every template
// output before opening the batch's transaction (see ExecuteTemplateBatch's
// doc comment for why moving the read-only render earlier does not weaken
// the all-or-nothing write guarantee).
func TestExecuteTemplateBatch_SignsWithConfiguredAttestor(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	attestor, verifier := newTestAttestor(t, "ed25519-batch-signed")
	ledgerStore := postgres.NewLedgerStore(pool).WithAuth(attestor)
	tmplStore := postgres.NewTemplateStore(pool)

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	jtID := postgrestest.SeedJournalType(t, pool, "deposit_confirm", "Deposit Confirm")
	clsWallet := postgrestest.SeedClassification(t, pool, "main_wallet", "Main Wallet", "debit", false)
	clsCustodial := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)

	_, err := tmplStore.CreateTemplate(ctx, core.TemplateInput{
		Code:           "batch_deposit_confirm",
		Name:           "Batch Deposit Confirm",
		JournalTypeUID: jtID,
		Lines: []core.TemplateLineInput{
			{ClassificationUID: clsWallet, EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 1},
			{ClassificationUID: clsCustodial, EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 2},
		},
	})
	require.NoError(t, err)

	requests := []core.TemplateExecutionRequest{
		{TemplateCode: "batch_deposit_confirm", Params: core.TemplateParams{
			HolderID: 91, CurrencyUID: curID, IdempotencyKey: postgrestest.UniqueKey("batch-signed-1"),
			Amounts: map[string]decimal.Decimal{"amount": decimal.NewFromInt(10)}, Source: "test",
		}},
		{TemplateCode: "batch_deposit_confirm", Params: core.TemplateParams{
			HolderID: 92, CurrencyUID: curID, IdempotencyKey: postgrestest.UniqueKey("batch-signed-2"),
			Amounts: map[string]decimal.Decimal{"amount": decimal.NewFromInt(20)}, Source: "test",
		}},
	}

	journals, err := ledgerStore.ExecuteTemplateBatch(ctx, requests)
	require.NoError(t, err)
	require.Len(t, journals, 2)
	for i, j := range journals {
		assert.Equal(t, core.AuthStatusSigned, j.AuthStatus, "batch entry %d", i)
		require.NotEmpty(t, j.AuthSignature, "batch entry %d", i)
	}

	// RenderTemplate is read-only and deterministic given the same template
	// row + params, so re-rendering the first request independently
	// reproduces byte-identical uid-space input to what was signed.
	rendered, err := ledgerStore.RenderTemplate(ctx, "batch_deposit_confirm", requests[0].Params)
	require.NoError(t, err)
	err = core.VerifyJournalAuth(ctx, verifier, *rendered, journals[0].EffectiveAt, journals[0].AuthDigest, journals[0].AuthSignature, journals[0].AuthKeyID)
	require.NoError(t, err)
}

// TestExecuteTemplateBatch_UnsignedNoAttestorInPoolMode is
// ExecuteTemplateBatch's no-Attestor-in-pool-mode counterpart.
func TestExecuteTemplateBatch_UnsignedNoAttestorInPoolMode(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	ledgerStore := postgres.NewLedgerStore(pool) // no attestor
	tmplStore := postgres.NewTemplateStore(pool)

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	jtID := postgrestest.SeedJournalType(t, pool, "deposit_confirm", "Deposit Confirm")
	clsWallet := postgrestest.SeedClassification(t, pool, "main_wallet", "Main Wallet", "debit", false)
	clsCustodial := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)

	_, err := tmplStore.CreateTemplate(ctx, core.TemplateInput{
		Code:           "batch_deposit_confirm",
		Name:           "Batch Deposit Confirm",
		JournalTypeUID: jtID,
		Lines: []core.TemplateLineInput{
			{ClassificationUID: clsWallet, EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 1},
			{ClassificationUID: clsCustodial, EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 2},
		},
	})
	require.NoError(t, err)

	requests := []core.TemplateExecutionRequest{
		{TemplateCode: "batch_deposit_confirm", Params: core.TemplateParams{
			HolderID: 93, CurrencyUID: curID, IdempotencyKey: postgrestest.UniqueKey("batch-noattestor-1"),
			Amounts: map[string]decimal.Decimal{"amount": decimal.NewFromInt(10)}, Source: "test",
		}},
	}

	journals, err := ledgerStore.ExecuteTemplateBatch(ctx, requests)
	require.NoError(t, err)
	require.Len(t, journals, 1)
	assert.Equal(t, core.AuthStatusUnsignedNoAttestor, journals[0].AuthStatus)
}

// TestExecuteTemplateBatch_TxMode_NeverSignsEvenWithAttestor is
// ExecuteTemplateBatch's tx-mode negative contrast.
func TestExecuteTemplateBatch_TxMode_NeverSignsEvenWithAttestor(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	attestor, _ := newTestAttestor(t, "ed25519-batch-txmode")
	store := postgres.NewLedgerStore(pool).WithAuth(attestor)
	tmplStore := postgres.NewTemplateStore(pool)

	curID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	jtID := postgrestest.SeedJournalType(t, pool, "deposit_confirm", "Deposit Confirm")
	clsWallet := postgrestest.SeedClassification(t, pool, "main_wallet", "Main Wallet", "debit", false)
	clsCustodial := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)

	_, err := tmplStore.CreateTemplate(ctx, core.TemplateInput{
		Code:           "batch_deposit_confirm",
		Name:           "Batch Deposit Confirm",
		JournalTypeUID: jtID,
		Lines: []core.TemplateLineInput{
			{ClassificationUID: clsWallet, EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 1},
			{ClassificationUID: clsCustodial, EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 2},
		},
	})
	require.NoError(t, err)

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	txStore := store.WithDB(tx)

	requests := []core.TemplateExecutionRequest{
		{TemplateCode: "batch_deposit_confirm", Params: core.TemplateParams{
			HolderID: 94, CurrencyUID: curID, IdempotencyKey: postgrestest.UniqueKey("batch-txmode-1"),
			Amounts: map[string]decimal.Decimal{"amount": decimal.NewFromInt(10)}, Source: "test",
		}},
	}

	journals, err := txStore.ExecuteTemplateBatch(ctx, requests)
	require.NoError(t, err)
	require.Len(t, journals, 1)
	assert.Equal(t, core.AuthStatusUnsignedTxMode, journals[0].AuthStatus)
}
