package ledger_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/authdev"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
)

// newTestKeyGeneration returns an Attestor plus the public half of the same
// key, so a test can assemble a multi-generation verifier by hand the way an
// operator assembles one from stored public keys.
func newTestKeyGeneration(t *testing.T, keyID string) (core.Attestor, ed25519.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	attestor, _, err := authdev.NewLocalAttestor(priv.Seed(), keyID)
	require.NoError(t, err)
	pub, ok := priv.Public().(ed25519.PublicKey)
	require.True(t, ok)
	return attestor, pub
}

// fundAgain posts one more signed funding journal onto an existing
// gateFixture's dimension, through the facade's ordinary write path.
func fundAgain(t *testing.T, ctx context.Context, svc *ledger.Service, fx gateFixture, amount decimal.Decimal, scope string) {
	t.Helper()
	jt, err := svc.JournalTypes().CreateJournalType(ctx, core.JournalTypeInput{
		Code: fmt.Sprintf("rot_%s_%d", scope, time.Now().UnixNano()), Name: "Rotation Fund",
	})
	require.NoError(t, err)
	_, err = svc.JournalWriter().PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jt.UID,
		IdempotencyKey: postgrestest.UniqueKey("rot-fund-" + scope),
		Source:         "key-rotation-test",
		ActorID:        fx.holder,
		Entries: []core.EntryInput{
			{AccountHolder: fx.holder, CurrencyUID: fx.currencyUID, ClassificationUID: fx.walletUID, EntryType: core.EntryTypeDebit, Amount: amount},
			{AccountHolder: core.SystemAccountHolder(fx.holder), CurrencyUID: fx.currencyUID, ClassificationUID: fx.custodialUID, EntryType: core.EntryTypeCredit, Amount: amount},
		},
	})
	require.NoError(t, err)
}

// TestService_KeyRotation_OldGenerationMustStayRegistered pins C-M5
// (2026-09-02 audit, tamper-evident.md M-5): rotating the P5 signing key was
// STRUCTURALLY impossible with the verifier this library ships.
//
// The failure it protects against is not subtle. Journals are append-only and
// each carries the key id it was signed under. A verifier holding only the
// CURRENT key answers ErrUnknownAuthKey for every journal signed under the
// previous one -> core.VerifyJournalAuth wraps that as ErrUnauthorizedJournal
// -> postgres.VerifiedBalanceStore reports the dimension UNDEFINED -> with
// RequireVerifiedBalance=true, withdrawals are refused for EVERY holder with
// history. And before authdev.NewLocalVerifierSet existed there was no way to
// hold two generations at once: NewLocalVerifier takes exactly one key and
// LocalVerifier.keys is unexported with no setter. I-45's operator advice
// ("register the retired key to restore verification coverage") named an
// action the library could not perform.
//
// Everything here goes through ledger.New / ledger.WithAttestor -- the route
// a consumer actually has. postgres.NewVerifiedBalanceStore is never named.
//
// Pinned symbols: authdev.NewLocalVerifierSet, ledger.WithAttestor,
// (*ledger.Service).VerifiedBalanceReader.
func TestService_KeyRotation_OldGenerationMustStayRegistered(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	attestorGen1, pubGen1 := newTestKeyGeneration(t, "rotation-gen-1")
	attestorGen2, pubGen2 := newTestKeyGeneration(t, "rotation-gen-2")

	verifierGen1, err := authdev.NewLocalVerifierSet(map[string]ed25519.PublicKey{"rotation-gen-1": pubGen1})
	require.NoError(t, err)

	// --- Before the rotation: everything signed under gen-1. ---
	svcGen1, err := ledger.New(pool, ledger.WithAttestor(attestorGen1, verifierGen1))
	require.NoError(t, err)
	fx := seedGateFixture(t, ctx, svcGen1, 9501, decimal.NewFromInt(100))

	balance, err := svcGen1.VerifiedBalanceReader().VerifiedBalance(ctx, fx.holder, fx.currencyUID, fx.walletUID)
	require.NoError(t, err, "baseline: a dimension signed entirely under the current key must verify")
	require.True(t, balance.Equal(decimal.NewFromInt(100)), "expected 100, got %s", balance)

	// --- The rotation done WRONG: only the new key registered. ---
	verifierGen2Only, err := authdev.NewLocalVerifierSet(map[string]ed25519.PublicKey{"rotation-gen-2": pubGen2})
	require.NoError(t, err)
	svcGen2Only, err := ledger.New(pool, ledger.WithAttestor(attestorGen2, verifierGen2Only))
	require.NoError(t, err)
	fundAgain(t, ctx, svcGen2Only, fx, decimal.NewFromInt(50), "gen2")

	_, err = svcGen2Only.VerifiedBalanceReader().VerifiedBalance(ctx, fx.holder, fx.currencyUID, fx.walletUID)
	require.ErrorIs(t, err, core.ErrUnauthorizedJournal,
		"dropping the retired key must make the historical journal unverifiable -- this is the fail-closed "+
			"cost of a rotation done wrong, and it is what the RUNBOOK section warns about")

	// The same dimension, seen through the withdrawal gate: refused.
	_, err = svcGen2Only.Reserver().Reserve(ctx, core.ReserveInput{
		AccountHolder:          fx.holder,
		CurrencyUID:            fx.currencyUID,
		Amount:                 decimal.NewFromInt(1),
		IdempotencyKey:         postgrestest.UniqueKey("rot-reserve-broken"),
		ExpiresIn:              10 * time.Minute,
		RequireVerifiedBalance: true,
	})
	require.Error(t, err, "with the retired key unregistered, the gate refuses every holder with history")

	// --- The rotation done RIGHT: both generations registered. ---
	verifierBoth, err := authdev.NewLocalVerifierSet(map[string]ed25519.PublicKey{
		"rotation-gen-1": pubGen1, // retired, still registered
		"rotation-gen-2": pubGen2, // current
	})
	require.NoError(t, err)
	require.Equal(t, []string{"rotation-gen-1", "rotation-gen-2"}, verifierBoth.KeyIDs())

	svcBoth, err := ledger.New(pool, ledger.WithAttestor(attestorGen2, verifierBoth))
	require.NoError(t, err)

	balance, err = svcBoth.VerifiedBalanceReader().VerifiedBalance(ctx, fx.holder, fx.currencyUID, fx.walletUID)
	require.NoError(t, err,
		"a dimension carrying journals from BOTH key generations must verify when both keys are registered; "+
			"without authdev.NewLocalVerifierSet there is no way to express this at all")
	require.True(t, balance.Equal(decimal.NewFromInt(150)), "expected 150 (100 under gen-1 + 50 under gen-2), got %s", balance)

	_, err = svcBoth.Reserver().Reserve(ctx, core.ReserveInput{
		AccountHolder:          fx.holder,
		CurrencyUID:            fx.currencyUID,
		Amount:                 decimal.NewFromInt(10),
		IdempotencyKey:         postgrestest.UniqueKey("rot-reserve-ok"),
		ExpiresIn:              10 * time.Minute,
		RequireVerifiedBalance: true,
	})
	require.NoError(t, err, "with both generations registered the withdrawal gate works across the rotation")
}
