package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/authdev"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
)

// TestAuthorizePostAuthorizedKeepsRunInTxVerifiable pins the claim
// runInTxAuthorizationDemo (in main.go) demonstrates end to end: Authorize +
// PostAuthorized composed inside RunInTx keeps a journal signed and its
// dimension verifiable, while PostJournal (or ExecuteTemplate /
// ExecuteTemplateBatch, which reduce to the same write path) called directly
// inside RunInTx always produces auth_status=unsigned_tx_mode and makes the
// dimension it touches UNDEFINED under VerifiedBalanceReader.
//
// This is the pin E-M12 (docs/audits/2026-09-02-deep-audit/TODO.md) asks for:
// "同一测试里用 ExecuteTemplate-in-tx 落另一笔，断言该维度转为 UNDEFINED。两个
// 方向都钉住" -- PostJournal-in-tx is the same write path ExecuteTemplate
// reduces to (both call through to LedgerStore.PostJournal without an
// AuthorizedJournal), so pinning it here pins ExecuteTemplate's behavior too.
func TestAuthorizePostAuthorizedKeepsRunInTxVerifiable(t *testing.T) {
	pool := postgrestest.SetupDB(t)

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	attestor, verifier, err := authdev.NewLocalAttestor(priv.Seed(), "test-key")
	require.NoError(t, err)

	svc, err := ledger.New(pool, ledger.WithAttestor(attestor, verifier))
	require.NoError(t, err)

	currencyUID := postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD")
	jtUID := postgrestest.SeedJournalType(t, pool, "te_credit", "Tamper-Evident Credit")
	walletUID := postgrestest.SeedClassificationWithRole(t, pool, "te_wallet", "TE Wallet", "debit", false, "available")
	custodyUID := postgrestest.SeedClassificationWithRole(t, pool, "te_custodial", "TE Custodial", "credit", true, "")

	const holder int64 = 424242
	ctx := context.Background()

	journalInput := func(amount decimal.Decimal, key string) core.JournalInput {
		return core.JournalInput{
			JournalTypeUID: jtUID,
			IdempotencyKey: key,
			Source:         "test",
			Entries: []core.EntryInput{
				{AccountHolder: holder, CurrencyUID: currencyUID, ClassificationUID: walletUID,
					EntryType: core.EntryTypeDebit, Amount: amount},
				{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: currencyUID, ClassificationUID: custodyUID,
					EntryType: core.EntryTypeCredit, Amount: amount},
			},
		}
	}

	// Direction 1: Authorize (outside the transaction) + PostAuthorized
	// (inside it) stays signed and verifiable.
	authorized, err := svc.Authorize(ctx, journalInput(decimal.RequireFromString("10.00"), "authorized-in-tx"))
	require.NoError(t, err)

	var signed *core.Journal
	require.NoError(t, svc.RunInTx(ctx, func(tx *ledger.Service) error {
		var err error
		signed, err = tx.JournalWriter().PostAuthorized(ctx, authorized)
		return err
	}))
	require.Equal(t, core.AuthStatusSigned, signed.AuthStatus,
		"a journal composed via Authorize+PostAuthorized inside RunInTx must be signed")

	verified, err := svc.VerifiedBalanceReader().VerifiedBalance(ctx, holder, currencyUID, walletUID)
	require.NoError(t, err, "the dimension must still be verifiable after an Authorize+PostAuthorized write")
	require.True(t, verified.Equal(decimal.RequireFromString("10.00")), "VerifiedBalance = %s, want 10.00", verified)

	// Direction 2: PostJournal called directly inside RunInTx (the same
	// write path ExecuteTemplate/ExecuteTemplateBatch use) is unsigned, and
	// contaminates the dimension it shares with the signed journal above.
	var unsigned *core.Journal
	require.NoError(t, svc.RunInTx(ctx, func(tx *ledger.Service) error {
		var err error
		unsigned, err = tx.JournalWriter().PostJournal(ctx, journalInput(decimal.RequireFromString("5.00"), "unsigned-in-tx"))
		return err
	}))
	require.Equal(t, core.AuthStatusUnsignedTxMode, unsigned.AuthStatus,
		"a journal posted directly inside RunInTx (no Authorize) must be unsigned_tx_mode")

	_, err = svc.VerifiedBalanceReader().VerifiedBalance(ctx, holder, currencyUID, walletUID)
	require.True(t, errors.Is(err, core.ErrUnauthorizedJournal),
		"an unsigned_tx_mode journal contributing to this dimension must make VerifiedBalance UNDEFINED, got: %v", err)

	// The plain balance sees both -- this journal really did move money, it
	// is simply not verifiable.
	plain, err := svc.BalanceReader().GetBalance(ctx, holder, currencyUID, walletUID)
	require.NoError(t, err)
	require.True(t, plain.Equal(decimal.RequireFromString("15.00")), "GetBalance = %s, want 15.00", plain)
}
