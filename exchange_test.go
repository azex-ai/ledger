package ledger_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
)

// exchangeFixture is a holder with a confirmed 1 USDC deposit, a CREDITS
// currency to buy, and the FX presets Exchange composes.
type exchangeFixture struct {
	svc         *ledger.Service
	pool        *pgxpool.Pool
	holder      int64
	usdc        string
	credits     string
	depositUID  string
	rate        core.FixedRate
	walletClass string
}

func seedExchangeFixture(t *testing.T, ctx context.Context) exchangeFixture {
	t.Helper()
	pool := postgrestest.SetupDB(t)
	svc, err := ledger.New(pool)
	require.NoError(t, err)
	require.NoError(t, svc.InstallExtendedPresets(ctx))

	suffix := time.Now().UnixNano()
	usdc, err := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{Code: fmt.Sprintf("USDC_%d", suffix), Name: "USD Coin", Exponent: 6})
	require.NoError(t, err)
	credits, err := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{Code: fmt.Sprintf("CREDITS_%d", suffix), Name: "Credits", Exponent: 6})
	require.NoError(t, err)
	wallet, err := svc.Classifications().GetByCode(ctx, "main_wallet")
	require.NoError(t, err)

	const holder = int64(7301)
	deposit, err := svc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
		HolderID: holder, CurrencyUID: usdc.UID, IdempotencyKey: postgrestest.UniqueKey("exchange-deposit"),
		Amounts: map[string]decimal.Decimal{"amount": decimal.NewFromInt(1)},
	})
	require.NoError(t, err)

	return exchangeFixture{
		svc: svc, pool: pool, holder: holder, usdc: usdc.UID, credits: credits.UID,
		depositUID: deposit.UID, walletClass: wallet.UID,
		rate: core.FixedRate{SourceCode: usdc.Code, TargetCode: credits.Code,
			Rate: decimal.NewFromInt(1000), Version: "price-v1", Rounding: core.RoundHalfUp},
	}
}

func (f exchangeFixture) balance(t *testing.T, ctx context.Context, currency string) decimal.Decimal {
	t.Helper()
	got, err := f.svc.BalanceReader().GetBalance(ctx, f.holder, currency, f.walletClass)
	require.NoError(t, err)
	return got
}

func (f exchangeFixture) held(t *testing.T, ctx context.Context, currency string) decimal.Decimal {
	t.Helper()
	got, err := f.svc.Reserver().HeldAmount(ctx, f.holder, currency)
	require.NoError(t, err)
	return got
}

func (f exchangeFixture) journalCount(t *testing.T, ctx context.Context) int {
	t.Helper()
	var n int
	require.NoError(t, f.pool.QueryRow(ctx, "SELECT count(*) FROM journals").Scan(&n))
	return n
}

func (f exchangeFixture) input(key string) ledger.ExchangeInput {
	return ledger.ExchangeInput{
		HolderID: f.holder, SourceCurrencyUID: f.usdc, TargetCurrencyUID: f.credits,
		Quantity: decimal.NewFromInt(1), Rate: f.rate, IdempotencyKey: key,
		FundingUID: f.depositUID, Source: "exchange-test",
		Metadata: map[string]string{"purchase_id": key},
	}
}

func TestExchange_MovesBothBalancesAndRecordsTheQuote(t *testing.T) {
	ctx := context.Background()
	f := seedExchangeFixture(t, ctx)

	res, err := f.svc.Exchange(ctx, f.input("purchase-1"))
	require.NoError(t, err)
	require.True(t, res.Quote.TargetAmount.Equal(decimal.NewFromInt(1000)))
	require.NotEmpty(t, res.ReservationUID)
	require.NotEmpty(t, res.SellJournalUID)
	require.NotEmpty(t, res.BuyJournalUID)

	require.True(t, f.balance(t, ctx, f.usdc).IsZero(), "the deposited USDC left the wallet")
	require.True(t, f.balance(t, ctx, f.credits).Equal(decimal.NewFromInt(1000)))
	require.True(t, f.held(t, ctx, f.usdc).IsZero(), "the hold was settled, not left outstanding")
	require.Equal(t, 3, f.journalCount(t, ctx), "deposit + two FX legs")

	// Both legs carry the same quote and the funding reference, so a
	// statement can explain the purchase and a reconciliation can join it
	// back to the deposit.
	for _, uid := range []string{res.SellJournalUID, res.BuyJournalUID} {
		var encoded, funding, purchase string
		require.NoError(t, f.pool.QueryRow(ctx,
			"SELECT metadata->>$2, metadata->>$3, metadata->>'purchase_id' FROM journals WHERE uid=$1",
			uid, core.ConversionQuotesMetadataKey, core.FundingUIDMetadataKey).Scan(&encoded, &funding, &purchase))
		quotes, err := core.DecodeConversionQuotes(encoded)
		require.NoError(t, err)
		require.Len(t, quotes, 1)
		require.Equal(t, res.Quote.SourceCode, quotes[0].SourceCode)
		require.True(t, quotes[0].TargetAmount.Equal(res.Quote.TargetAmount))
		require.Equal(t, "price-v1", quotes[0].Version)
		require.Equal(t, f.depositUID, funding)
		require.Equal(t, "purchase-1", purchase, "host metadata is preserved alongside the ledger's keys")
	}

	// The holder-facing view sees the two legs as two rows with the quote
	// attached, and nothing about how the ledger produced them.
	rows, _, err := f.svc.HolderReader().ListHolderTransactions(ctx, f.holder, "", 10)
	require.NoError(t, err)
	var quoted int
	for _, row := range rows {
		if row.UID == res.SellJournalUID || row.UID == res.BuyJournalUID {
			require.Len(t, row.Quotes, 1)
			require.True(t, row.Quotes[0].Rate.Equal(decimal.NewFromInt(1000)))
			quoted++
		} else {
			require.Empty(t, row.Quotes, "a journal without a quote has no quotes, not a fabricated one")
		}
	}
	require.Equal(t, 2, quoted)
}

func TestExchange_ReplayIsNoOpAndChangedQuoteConflicts(t *testing.T) {
	ctx := context.Background()
	f := seedExchangeFixture(t, ctx)

	first, err := f.svc.Exchange(ctx, f.input("purchase-2"))
	require.NoError(t, err)
	before := f.journalCount(t, ctx)

	again, err := f.svc.Exchange(ctx, f.input("purchase-2"))
	require.NoError(t, err)
	require.Equal(t, first, again, "replay returns the original result")
	require.Equal(t, before, f.journalCount(t, ctx))
	require.True(t, f.balance(t, ctx, f.credits).Equal(decimal.NewFromInt(1000)), "replay must not move money again")

	// Same key, different quote that still rounds to 1000: the metadata
	// snapshot is what makes this collide, not the amount.
	changed := f.input("purchase-2")
	changed.Rate.Rate = decimal.RequireFromString("1000.0000001")
	changed.Rate.Version = "price-v2"
	_, err = f.svc.Exchange(ctx, changed)
	require.ErrorIs(t, err, core.ErrConflict)

	// Same key, different funding reference: also a different payload.
	refunded := f.input("purchase-2")
	refunded.FundingUID = "some-other-deposit"
	_, err = f.svc.Exchange(ctx, refunded)
	require.ErrorIs(t, err, core.ErrConflict)

	require.Equal(t, before, f.journalCount(t, ctx))
	require.True(t, f.balance(t, ctx, f.credits).Equal(decimal.NewFromInt(1000)))
}

func TestExchange_RefusesBeforeWritingAnything(t *testing.T) {
	ctx := context.Background()
	f := seedExchangeFixture(t, ctx)
	before := f.journalCount(t, ctx)

	// Output rounds to zero: refused before any reserve or journal.
	tiny := f.input("tiny")
	tiny.Rate.Rate = decimal.RequireFromString("0.0000001")
	tiny.Rate.Rounding = core.RoundDown
	_, err := f.svc.Exchange(ctx, tiny)
	require.ErrorIs(t, err, core.ErrInvalidInput)

	// Quote for the wrong pair.
	wrongPair := f.input("wrong-pair")
	wrongPair.Rate.SourceCode = "NOT_USDC"
	_, err = f.svc.Exchange(ctx, wrongPair)
	require.ErrorIs(t, err, core.ErrInvalidInput)

	// Host metadata may not squat on the ledger's keys.
	squat := f.input("squat")
	squat.Metadata[core.ConversionQuotesMetadataKey] = "[]"
	_, err = f.svc.Exchange(ctx, squat)
	require.ErrorIs(t, err, core.ErrInvalidInput)

	// Not enough source balance: the reservation refuses, nothing posts.
	tooMuch := f.input("too-much")
	tooMuch.Quantity = decimal.NewFromInt(2)
	_, err = f.svc.Exchange(ctx, tooMuch)
	require.ErrorIs(t, err, core.ErrInsufficientBalance)

	for _, in := range []ledger.ExchangeInput{
		{}, // everything missing
		func() ledger.ExchangeInput { i := f.input("neg"); i.Quantity = decimal.NewFromInt(-1); return i }(),
		func() ledger.ExchangeInput { i := f.input("same"); i.TargetCurrencyUID = f.usdc; return i }(),
		func() ledger.ExchangeInput { i := f.input("sys"); i.HolderID = -f.holder; return i }(),
	} {
		_, err = f.svc.Exchange(ctx, in)
		require.ErrorIs(t, err, core.ErrInvalidInput)
	}

	require.Equal(t, before, f.journalCount(t, ctx))
	require.True(t, f.balance(t, ctx, f.usdc).Equal(decimal.NewFromInt(1)))
	require.True(t, f.held(t, ctx, f.usdc).IsZero())
	var holds int
	require.NoError(t, f.pool.QueryRow(ctx, "SELECT count(*) FROM reservations").Scan(&holds))
	require.Zero(t, holds, "a refused exchange leaves no reservation row behind")
}

func TestExchange_JoinsTheCallersTransaction(t *testing.T) {
	ctx := context.Background()
	f := seedExchangeFixture(t, ctx)
	_, err := f.pool.Exec(ctx, "CREATE TABLE host_deposits (uid text PRIMARY KEY, purchase_journal_uid text NOT NULL DEFAULT '')")
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, "INSERT INTO host_deposits (uid) VALUES ($1)", f.depositUID)
	require.NoError(t, err)

	// The host marks its own deposit row converted in the same commit as
	// the exchange -- the shape a deposit-to-credits job needs.
	var composed *ledger.ExchangeResult
	require.NoError(t, f.svc.RunInTx(ctx, func(tx *ledger.Service) error {
		res, err := tx.Exchange(ctx, f.input("composed"))
		if err != nil {
			return err
		}
		composed = res
		_, err = tx.DBTX().Exec(ctx, "UPDATE host_deposits SET purchase_journal_uid = $2 WHERE uid = $1", f.depositUID, res.BuyJournalUID)
		return err
	}))
	var linked string
	require.NoError(t, f.pool.QueryRow(ctx, "SELECT purchase_journal_uid FROM host_deposits WHERE uid=$1", f.depositUID).Scan(&linked))
	require.Equal(t, composed.BuyJournalUID, linked)
	require.True(t, f.balance(t, ctx, f.credits).Equal(decimal.NewFromInt(1000)))

	// And a failure after the exchange rolls the exchange back with it.
	f2 := seedExchangeFixture(t, ctx)
	sentinel := fmt.Errorf("host write failed")
	err = f2.svc.RunInTx(ctx, func(tx *ledger.Service) error {
		if _, err := tx.Exchange(ctx, f2.input("rolled-back")); err != nil {
			return err
		}
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)
	require.True(t, f2.balance(t, ctx, f2.usdc).Equal(decimal.NewFromInt(1)), "USDC came back")
	require.True(t, f2.balance(t, ctx, f2.credits).IsZero(), "no credits were issued")
	require.True(t, f2.held(t, ctx, f2.usdc).IsZero())
	require.Equal(t, 1, f2.journalCount(t, ctx), "only the deposit remains")
}

func TestExchange_MissingTemplateIsAnErrorNotAPartialWrite(t *testing.T) {
	ctx := context.Background()
	f := seedExchangeFixture(t, ctx)
	in := f.input("no-template")
	in.BuyTemplateCode = "fx_buy_does_not_exist"
	_, err := f.svc.Exchange(ctx, in)
	require.ErrorIs(t, err, core.ErrNotFound)
	require.True(t, f.balance(t, ctx, f.usdc).Equal(decimal.NewFromInt(1)))
	require.True(t, f.held(t, ctx, f.usdc).IsZero())
	require.Equal(t, 1, f.journalCount(t, ctx))
}
