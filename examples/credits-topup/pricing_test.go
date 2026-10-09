package main

import (
	"context"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
)

func testPricing(t *testing.T) pricing {
	t.Helper()
	p, err := parsePricing(defaultRates)
	require.NoError(t, err)
	return p
}

func TestConfiguredPrices_UnitsAndOverrides(t *testing.T) {
	p := testPricing(t)
	credits := p.units["CREDITS"]
	for _, tc := range []struct{ unit, quantity, want string }{
		{"USDC", "1", "1000"},
		{"INPUT_TOKEN", "10000", "20"},
		{"OUTPUT_TOKEN", "2425", "12.125"},
		{"IMAGE", "1", "25"},
		{"ROSE", "3", "30"},
	} {
		t.Run(tc.unit, func(t *testing.T) {
			got, err := p.price(t.Context(), credits, usageQuantity{tc.unit, decimal.RequireFromString(tc.quantity)})
			require.NoError(t, err)
			require.True(t, got.amount.Equal(decimal.RequireFromString(tc.want)))
		})
	}
	got, err := p.price(t.Context(), credits,
		usageQuantity{"INPUT_TOKEN", decimal.NewFromInt(10000)},
		usageQuantity{"OUTPUT_TOKEN", decimal.NewFromInt(2425)})
	require.NoError(t, err)
	require.Equal(t, "32.125", got.amount.String())
	require.Len(t, got.quotes, 2, "one quote per priced line")

	changed, err := parsePricing([]byte(strings.Replace(string(defaultRates), `"rate": "1000"`, `"rate": "2000"`, 1)))
	require.NoError(t, err)
	got, err = changed.price(t.Context(), credits, usageQuantity{"USDC", decimal.NewFromInt(1)})
	require.NoError(t, err)
	require.Equal(t, "2000", got.amount.String())
}

func TestPricingConfig_RejectsInvalidAndAmbiguousInput(t *testing.T) {
	for _, input := range []string{
		strings.Replace(string(defaultRates), `"rate": "1000"`, `"rate": 1000`, 1),
		strings.Replace(string(defaultRates), `"rate": "1000"`, `"rate": "0"`, 1),
		strings.Replace(string(defaultRates), `"rounding": "half_up"`, `"rounding": "typo"`, 1),
		strings.Replace(string(defaultRates), `"source_code": "USDC"`, `"source_code": "UNKNOWN"`, 1),
		strings.Replace(string(defaultRates), `"target_code": "CREDITS"`, `"target_code": "USDC"`, 1),
		strings.Replace(string(defaultRates), `"source_code": "INPUT_TOKEN"`, `"source_code": "USDC"`, 1),
		strings.Replace(string(defaultRates), `"code": "IMAGE"`, `"code": "USDC"`, 1),
		strings.Replace(string(defaultRates), `"version": "demo-v1"`, `"version": ""`, 1),
		string(defaultRates) + `{}`,
	} {
		_, err := parsePricing([]byte(input))
		require.Error(t, err)
	}
}

func TestConfiguredConsumption_QuoteChangesConflictEvenAtSameCharge(t *testing.T) {
	svc, admin, usdc, creditsUID := fixture(t)
	ctx := t.Context()
	deposit(t, svc, usdc)
	require.NoError(t, purchaseCredits(ctx, svc, usdc, creditsUID, decimal.NewFromInt(1), "purchase"))
	rsv := reserve(t, svc, creditsUID, "quoted-job", 10)
	credits, err := svc.Currencies().GetCurrency(ctx, creditsUID)
	require.NoError(t, err)
	p := testPricing(t)
	key := [2]string{"INPUT_TOKEN", "CREDITS"}
	rate := p.rates[key]
	rate.Rate = decimal.RequireFromString("0.0000006")
	p.rates[key] = rate
	original, err := p.price(ctx, *credits, usageQuantity{"INPUT_TOKEN", decimal.NewFromInt(1)})
	require.NoError(t, err)
	require.Equal(t, "0.000001", original.amount.String())
	metadata, err := original.metadata()
	require.NoError(t, err)
	require.NoError(t, captureUsage(ctx, svc, rsv, original.amount, "quoted-event", false, metadata))

	for _, mutate := range []func(*core.FixedRate){
		func(r *core.FixedRate) { r.Rate = decimal.RequireFromString("0.0000007") },
		func(r *core.FixedRate) { r.Version = "demo-v2" },
		func(r *core.FixedRate) { r.Rounding = core.RoundUp },
	} {
		changed := rate
		mutate(&changed)
		p.rates[key] = changed
		quote, err := p.price(ctx, *credits, usageQuantity{"INPUT_TOKEN", decimal.NewFromInt(1)})
		require.NoError(t, err)
		require.True(t, quote.amount.Equal(original.amount))
		metadata, err := quote.metadata()
		require.NoError(t, err)
		require.ErrorIs(t, captureUsage(ctx, svc, rsv, quote.amount, "quoted-event", false, metadata), core.ErrConflict)
	}
	p.rates[key] = rate
	quote, err := p.price(ctx, *credits, usageQuantity{"INPUT_TOKEN", decimal.NewFromInt(2)})
	require.NoError(t, err)
	require.True(t, quote.amount.Equal(original.amount))
	changedMetadata, err := quote.metadata()
	require.NoError(t, err)
	require.ErrorIs(t, captureUsage(ctx, svc, rsv, quote.amount, "quoted-event", false, changedMetadata), core.ErrConflict)
	require.NoError(t, captureUsage(ctx, svc, rsv, original.amount, "quoted-event", false, metadata))
	balance(t, svc, creditsUID, "999.999999", "0")
	require.Equal(t, 4, journalCount(t, admin))
}

func TestConfiguredExchange_GiftCurrencyAndZeroOutput(t *testing.T) {
	svc, admin, usdc, credits := fixture(t)
	ctx := t.Context()
	deposit(t, svc, usdc)
	p := testPricing(t)
	rate, err := p.QuoteRate(ctx, "USDC", "CREDITS")
	require.NoError(t, err)
	rate.Rate = decimal.RequireFromString("0.0000001")
	rate.Rounding = core.RoundDown
	require.ErrorIs(t, exchange(ctx, svc, usdc, credits, decimal.NewFromInt(1), "tiny", rate), core.ErrInvalidInput)
	balance(t, svc, usdc, "1", "0")
	require.Equal(t, 1, journalCount(t, admin))
	var holds int
	require.NoError(t, admin.QueryRow(ctx, "SELECT count(*) FROM reservations").Scan(&holds))
	require.Zero(t, holds)

	require.NoError(t, purchaseCredits(ctx, svc, usdc, credits, decimal.NewFromInt(1), "purchase"))
	roses, err := ensureCurrency(ctx, svc, "ROSE", "Rose", 0)
	require.NoError(t, err)
	rate, err = p.QuoteRate(ctx, "CREDITS", "ROSE")
	require.NoError(t, err)
	require.NoError(t, exchange(ctx, svc, credits, roses, decimal.NewFromInt(20), "gift-purchase", rate))
	require.NoError(t, exchange(ctx, svc, credits, roses, decimal.NewFromInt(20), "gift-purchase", rate))
	changed := rate
	changed.Rate = decimal.RequireFromString("0.12") // still two whole roses after rounding
	require.ErrorIs(t, exchange(ctx, svc, credits, roses, decimal.NewFromInt(20), "gift-purchase", changed), core.ErrConflict)
	balance(t, svc, credits, "980", "0")
	balance(t, svc, roses, "2", "0")
	require.Equal(t, 5, journalCount(t, admin))
	var paySnapshot, issueSnapshot string
	require.NoError(t, admin.QueryRow(ctx,
		"SELECT metadata->>'conversion_quotes' FROM journals WHERE idempotency_key=$1", "gift-purchase:pay").Scan(&paySnapshot))
	require.NoError(t, admin.QueryRow(ctx,
		"SELECT metadata->>'conversion_quotes' FROM journals WHERE idempotency_key=$1", "gift-purchase:issue").Scan(&issueSnapshot))
	require.Equal(t, paySnapshot, issueSnapshot)
	snapshots, err := core.DecodeConversionQuotes(paySnapshot)
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	require.Equal(t, "CREDITS", snapshots[0].SourceCode)
	require.Equal(t, "ROSE", snapshots[0].TargetCode)
	require.Equal(t, int32(6), snapshots[0].SourceExponent)
	require.Equal(t, int32(0), snapshots[0].TargetExponent)
	require.True(t, snapshots[0].SourceQuantity.Equal(decimal.NewFromInt(20)))
	require.True(t, snapshots[0].TargetAmount.Equal(decimal.NewFromInt(2)))
	require.True(t, snapshots[0].Rate.Equal(decimal.RequireFromString("0.1")))
	require.Equal(t, "demo-v1", snapshots[0].Version)
	require.Equal(t, core.RoundDown, snapshots[0].Rounding)
	require.Contains(t, paySnapshot, `"rounding":"down"`, "the mode travels by name")
	reconciled, err := svc.Reconciler().CheckAccountingEquation(ctx)
	require.NoError(t, err)
	require.True(t, reconciled.Balanced)
}

func TestConfiguredScenario_CustomRatesAndReplay(t *testing.T) {
	svc, admin, usdc, credits := fixture(t)
	data := strings.Replace(string(defaultRates), `"rate": "1000"`, `"rate": "2000"`, 1)
	data = strings.Replace(data, `"rate": "0.002"`, `"rate": "0.003"`, 1)
	p, err := parsePricing([]byte(data))
	require.NoError(t, err)
	for range 2 {
		require.NoError(t, scenario(t.Context(), svc, usdc, credits, p))
		balance(t, svc, credits, "1902.875", "0") // 2000 - 25 - (30 + 12.125) - 30
		balance(t, svc, usdc, "0", "0")
		require.Equal(t, 7, journalCount(t, admin))
	}
	var snapshot string
	require.NoError(t, admin.QueryRow(t.Context(),
		"SELECT metadata->>'conversion_quotes' FROM journals WHERE idempotency_key=$1",
		"credits-demo-v2:tokens:result:charge").Scan(&snapshot))
	require.Contains(t, snapshot, `"source_quantity":"10000"`)
	require.Contains(t, snapshot, `"rate":"0.003"`)
	require.Contains(t, snapshot, `"target_amount":"30"`)
	require.Contains(t, snapshot, `"source_code":"OUTPUT_TOKEN"`)
	changed, err := parsePricing([]byte(strings.ReplaceAll(data, `"version": "demo-v1"`, `"version": "demo-v2"`)))
	require.NoError(t, err)
	require.ErrorIs(t, scenario(t.Context(), svc, usdc, credits, changed), core.ErrConflict)
	balance(t, svc, credits, "1902.875", "0")
	require.Equal(t, 7, journalCount(t, admin))
}

func TestConfiguredScenario_ZeroRoundedPurchaseOrStreamWritesNothing(t *testing.T) {
	for _, pair := range [][2]string{{"USDC", "CREDITS"}, {"STREAM_TOKEN", "CREDITS"}} {
		t.Run(pair[0], func(t *testing.T) {
			svc, admin, usdc, credits := fixture(t)
			p := testPricing(t)
			rate := p.rates[pair]
			rate.Rate = decimal.RequireFromString("0.0000000001")
			p.rates[pair] = rate
			require.ErrorIs(t, scenario(t.Context(), svc, usdc, credits, p), core.ErrInvalidInput)
			require.Zero(t, journalCount(t, admin))
			var reservations, receipts int
			require.NoError(t, admin.QueryRow(t.Context(),
				"SELECT (SELECT count(*) FROM reservations), (SELECT count(*) FROM reservation_operation_receipts)").Scan(&reservations, &receipts))
			require.Zero(t, reservations)
			require.Zero(t, receipts)
			balance(t, svc, usdc, "0", "0")
			balance(t, svc, credits, "0", "0")
		})
	}
}

// Existing money-flow regressions keep an explicit default quote. They exercise
// settlement/locking independently of configuration parsing and usage valuation.
func purchaseCredits(ctx context.Context, svc *ledger.Service, source, target string, amount decimal.Decimal, key string) error {
	p, err := parsePricing(defaultRates)
	if err != nil {
		return err
	}
	rate, err := p.QuoteRate(ctx, "USDC", "CREDITS")
	if err != nil {
		return err
	}
	return exchange(ctx, svc, source, target, amount, key, rate)
}

// exchange is the library primitive with this example's holder filled in.
func exchange(ctx context.Context, svc *ledger.Service, source, target string, amount decimal.Decimal, key string, rate core.FixedRate) error {
	_, err := svc.Exchange(ctx, ledger.ExchangeInput{
		HolderID: userID, SourceCurrencyUID: source, TargetCurrencyUID: target,
		Quantity: amount, Rate: rate, IdempotencyKey: key, Source: "credits-topup-example",
	})
	return err
}

func captureCredits(ctx context.Context, svc *ledger.Service, rsv *core.Reservation, amount decimal.Decimal, key string, partial bool) error {
	return captureUsage(ctx, svc, rsv, amount, key, partial, nil)
}
