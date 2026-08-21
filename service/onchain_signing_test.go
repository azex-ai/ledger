package service_test

// Pin for the RunInTx signing gap fix
// (docs/plans/2026-08-21-tamper-evident-ledger-design.md §7.5, board
// #12/#13). postDepositConfirmedJournal is P5's headline use case (M5:
// forged deposit accounting is exactly what per-journal signing exists to
// defeat) and is composed via TxComposer.RunInTx -- before this fix,
// PostJournal's tx-mode branch never called the Attestor at all, so this
// exact path always posted unsigned regardless of whether one was
// configured. This test drives a real deposit through IngestDeposit to
// "confirmed" with an Attestor configured and asserts the resulting
// journal is genuinely signed and verifiable against the exact input that
// was signed.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/authdev"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/presets"
	"github.com/azex-ai/ledger/service"
)

func TestOnchain_DepositConfirm_SignsViaRunInTx(t *testing.T) {
	const (
		chainID      = int64(1)
		token        = "0xusdttoken"
		currencyCode = "USDT-sign"
	)
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	tmplStore := postgres.NewTemplateStore(pool)
	require.NoError(t, presets.InstallCryptoDepositBundle(ctx, classStore, classStore, tmplStore))

	currencyStore := postgres.NewCurrencyStore(pool)
	cur, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{Code: currencyCode, Name: currencyCode, Exponent: 18})
	require.NoError(t, err)

	bookingStore := postgres.NewBookingStore(pool)

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	attestor, verifier, err := authdev.NewLocalAttestor(priv.Seed(), "onchain-sign-pin")
	require.NoError(t, err)
	ledgerStore := postgres.NewLedgerStore(pool).WithAuth(attestor)

	reader := newFakeChainReader()
	scanner := &fakeChainScanner{balances: make(map[string]decimal.Decimal)}
	sweeper := &fakeSweeper{gasPrice: decimal.NewFromInt(1)}

	deps := service.OnchainDeps{
		Registry:            postgres.NewDepositAddressStore(pool),
		Cursors:             postgres.NewChainCursorStore(pool),
		Booker:              bookingStore,
		BookingReader:       bookingStore,
		Journals:            ledgerStore,
		TxComposer:          &testTxComposer{pool: pool, bookingStore: bookingStore, ledgerStore: ledgerStore},
		Reader:              reader,
		RegistrationRescans: postgres.NewRegistrationRescanStore(pool),
		Scanner:             scanner,
		Sweeper:             sweeper,
		DeadLetters:         postgres.NewIngestDeadLetterStore(pool),
		Currencies:          currencyStore,
		Classifications:     classStore,
	}
	chains := chainSetWithToken(chainID, token, currencyCode, 2)
	onchain := service.NewOnchain(deps, chains)

	da, err := onchain.EnsureDepositAddress(ctx, 9101)
	require.NoError(t, err)

	sighting := core.DepositSighting{
		ChainID:       chainID,
		TxHash:        "0xsigntx1",
		TxLogSeq:      0,
		Token:         token,
		From:          "0xsender",
		To:            da.Address,
		Amount:        decimal.RequireFromString("77"),
		Confirmations: 5, // straight to confirmed -- above the chain's 2-confirmation threshold
		BlockNumber:   100,
	}
	confirmed, err := onchain.IngestDeposit(ctx, sighting)
	require.NoError(t, err)
	require.Equal(t, core.Status("confirmed"), confirmed.Status)
	require.NotEmpty(t, confirmed.JournalUID)

	var authStatus string
	var digest, signature []byte
	var keyID string
	var effectiveAt time.Time
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT auth_status, auth_digest, auth_signature, auth_key_id, effective_at FROM journals WHERE uid = $1",
		confirmed.JournalUID,
	).Scan(&authStatus, &digest, &signature, &keyID, &effectiveAt))

	assert.Equal(t, string(core.AuthStatusSigned), authStatus,
		"the deposit-confirm journal composed via RunInTx must be signed once an Attestor is configured -- this is exactly the gap §7.5 closes")
	require.NotEmpty(t, digest)
	require.NotEmpty(t, signature)
	require.Equal(t, "onchain-sign-pin", keyID)

	// Reconstruct exactly what was signed: RenderTemplate with the same
	// params postDepositConfirmedJournal used. EventUID is left "" here --
	// harmlessly, since CanonicalJournalDigest never covers it (Team
	// Lead's 2026-08-21 ruling, board #12/#13) -- so this reconstruction
	// would verify identically even if the real event uid were filled in.
	rendered, err := ledgerStore.RenderTemplate(ctx, "deposit_confirm", core.TemplateParams{
		HolderID:       da.AccountHolder,
		CurrencyUID:    cur.UID,
		IdempotencyKey: "deposit-confirm-" + confirmed.UID,
		Amounts:        map[string]decimal.Decimal{"amount": sighting.Amount},
		Source:         "onchain",
	})
	require.NoError(t, err)
	require.NoError(t, core.VerifyJournalAuth(ctx, verifier, *rendered, effectiveAt, digest, signature, keyID),
		"the stored signature must verify against the exact input that was signed")

	// The real event uid (now linked via journals.event_id) must ALSO
	// verify -- proving the fix for the false-positive Team Lead caught:
	// a verifier that reconstructs input with the journal's real,
	// persisted EventUID must not spuriously fail just because it differs
	// from whatever (if anything) was present at Authorize time.
	withRealEvent := *rendered
	withRealEvent.EventUID = confirmed.JournalUID // any non-empty value pins "EventUID is ignored", not a specific real event uid
	require.NoError(t, core.VerifyJournalAuth(ctx, verifier, withRealEvent, effectiveAt, digest, signature, keyID),
		"verification must not depend on which EventUID (if any) the reconstructed input carries")
}
