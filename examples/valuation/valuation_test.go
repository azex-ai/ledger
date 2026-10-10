package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/azex-ai/ledger/core"
	"github.com/shopspring/decimal"
)

func TestValueUSD_MixedCurrenciesPreserveQuantities(t *testing.T) {
	at, currencies, holdings, prices := fixture()
	before := append([]holding(nil), holdings...)
	got, err := valueUSD(at, time.Hour, currencies, holdings, prices)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Complete || got.TotalUSD != "629.93" || got.SubtotalUSD != "629.93" || got.ValuedCount != 4 {
		t.Fatalf("unexpected result: %+v", got)
	}
	if !reflect.DeepEqual(before, holdings) {
		t.Fatal("valuation mutated original quantities")
	}
	want := []string{"12.34", "617.285", "0.3", "0.005"}
	for i, row := range got.Rows {
		if row.ValueUSD != want[i] || !row.Quantity.Equal(holdings[i].Quantity) || row.Status != priceValid {
			t.Fatalf("row %d: %+v", i, row)
		}
		if row.Source != prices[row.CurrencyUID].Source || !row.PriceAsOf.Equal(at.Add(-time.Minute)) {
			t.Fatalf("price evidence lost: %+v", row)
		}
	}
}

func TestValueUSD_MissingStaleAndFutureDoNotBecomeZero(t *testing.T) {
	at, currencies, holdings, prices := fixture()
	delete(prices, "gift")
	stale := prices["btc"]
	stale.AsOf = at.Add(-time.Hour - time.Nanosecond)
	prices["btc"] = stale
	future := prices["credits"]
	future.AsOf = at.Add(time.Nanosecond)
	prices["credits"] = future
	got, err := valueUSD(at, time.Hour, currencies, holdings, prices)
	if err != nil {
		t.Fatal(err)
	}
	if got.Complete || got.TotalUSD != "" || got.SubtotalUSD != "12.34" || got.ValuedCount != 1 {
		t.Fatalf("incomplete valuation presented as total: %+v", got)
	}
	want := []priceStatus{priceValid, priceStale, priceMissing, priceFuture}
	for i, row := range got.Rows {
		if row.Status != want[i] || !row.Quantity.Equal(holdings[i].Quantity) {
			t.Fatalf("row %d: %+v", i, row)
		}
		if i > 0 && row.ValueUSD != "" {
			t.Fatalf("unpriced row has value %q", row.ValueUSD)
		}
	}
	if got.Rows[1].Source != stale.Source || !got.Rows[1].PriceAsOf.Equal(stale.AsOf) || got.Rows[1].UnitPriceUSD != "50000" {
		t.Fatal("stale price evidence must remain inspectable")
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["total_usd"]; ok {
		t.Fatal("incomplete JSON must omit total_usd")
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(wire["rows"], &rows); err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		if row["quantity"][0] != '"' {
			t.Fatal("quantity must be a JSON decimal string")
		}
		if _, ok := row["value_usd"]; ok && i > 0 {
			t.Fatal("unknown USD value must be absent, never zero")
		}
	}
}

func TestValueUSD_ExactPrecisionAndZeroPrice(t *testing.T) {
	at, currencies, _, prices := fixture()
	prices["btc"] = price{USDPerUnit: decimal.RequireFromString("0.000000000000000001"), Source: "fixture", AsOf: at.Add(-time.Hour)}
	holdings := []holding{{CurrencyUID: "btc", Quantity: decimal.RequireFromString("0.00000001")}}
	got, err := valueUSD(at, time.Hour, currencies, holdings, prices)
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalUSD != "0.00000000000000000000000001" || got.Rows[0].Status != priceValid {
		t.Fatalf("small value or inclusive freshness boundary lost: %+v", got)
	}
	prices["btc"] = price{USDPerUnit: decimal.Zero, Source: "explicit-write-down", AsOf: at}
	got, err = valueUSD(at, time.Hour, currencies, holdings, prices)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Complete || got.TotalUSD != "0" || got.Rows[0].ValueUSD != "0" {
		t.Fatalf("explicit zero price differs from missing price: %+v", got)
	}
}

func TestValueUSD_InvalidInputs(t *testing.T) {
	at, currencies, holdings, prices := fixture()
	cases := []struct {
		name string
		edit func(*time.Time, *time.Duration, map[string]core.Currency, *[]holding, map[string]price)
	}{
		{"unknown currency", func(_ *time.Time, _ *time.Duration, _ map[string]core.Currency, h *[]holding, _ map[string]price) {
			(*h)[0].CurrencyUID = "unknown"
		}},
		{"negative quantity", func(_ *time.Time, _ *time.Duration, _ map[string]core.Currency, h *[]holding, _ map[string]price) {
			(*h)[0].Quantity = decimal.NewFromInt(-1)
		}},
		{"excess quantity precision", func(_ *time.Time, _ *time.Duration, _ map[string]core.Currency, h *[]holding, _ map[string]price) {
			(*h)[0].Quantity = decimal.RequireFromString("1.001")
		}},
		{"huge quantity exponent", func(_ *time.Time, _ *time.Duration, _ map[string]core.Currency, h *[]holding, _ map[string]price) {
			(*h)[0].Quantity = decimal.New(1, math.MaxInt32)
		}},
		{"tiny quantity exponent", func(_ *time.Time, _ *time.Duration, _ map[string]core.Currency, h *[]holding, _ map[string]price) {
			(*h)[0].Quantity = decimal.New(1, math.MinInt32)
		}},
		{"huge price exponent", func(_ *time.Time, _ *time.Duration, _ map[string]core.Currency, _ *[]holding, p map[string]price) {
			v := p["usd"]
			v.USDPerUnit = decimal.New(1, math.MaxInt32)
			p["usd"] = v
		}},
		{"negative price", func(_ *time.Time, _ *time.Duration, _ map[string]core.Currency, _ *[]holding, p map[string]price) {
			v := p["usd"]
			v.USDPerUnit = decimal.NewFromInt(-1)
			p["usd"] = v
		}},
		{"missing source", func(_ *time.Time, _ *time.Duration, _ map[string]core.Currency, _ *[]holding, p map[string]price) {
			v := p["usd"]
			v.Source = " "
			p["usd"] = v
		}},
		{"missing time", func(_ *time.Time, _ *time.Duration, _ map[string]core.Currency, _ *[]holding, p map[string]price) {
			v := p["usd"]
			v.AsOf = time.Time{}
			p["usd"] = v
		}},
		{"currency identity mismatch", func(_ *time.Time, _ *time.Duration, c map[string]core.Currency, _ *[]holding, _ map[string]price) {
			v := c["usd"]
			v.UID = "btc"
			c["usd"] = v
		}},
		{"invalid currency exponent", func(_ *time.Time, _ *time.Duration, c map[string]core.Currency, _ *[]holding, _ map[string]price) {
			v := c["usd"]
			v.Exponent = math.MaxInt32
			c["usd"] = v
		}},
		{"zero age limit", func(_ *time.Time, maxAge *time.Duration, _ map[string]core.Currency, _ *[]holding, _ map[string]price) {
			*maxAge = 0
		}},
		{"zero valuation time", func(now *time.Time, _ *time.Duration, _ map[string]core.Currency, _ *[]holding, _ map[string]price) {
			*now = time.Time{}
		}},
		{"product overflow", func(_ *time.Time, _ *time.Duration, _ map[string]core.Currency, h *[]holding, p map[string]price) {
			(*h)[0].Quantity = decimal.NewFromInt(999999999999)
			v := p["usd"]
			v.USDPerUnit = decimal.NewFromInt(2)
			p["usd"] = v
		}},
		{"subtotal overflow", func(_ *time.Time, _ *time.Duration, _ map[string]core.Currency, h *[]holding, _ map[string]price) {
			(*h)[0].Quantity = decimal.NewFromInt(999999999999)
		}},
		{"duplicate currency", func(_ *time.Time, _ *time.Duration, _ map[string]core.Currency, h *[]holding, _ map[string]price) {
			*h = append(*h, (*h)[0])
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now, cs, hs, ps := fixture()
			maxAge := time.Hour
			tc.edit(&now, &maxAge, cs, &hs, ps)
			if _, err := valueUSD(now, maxAge, cs, hs, ps); !errors.Is(err, core.ErrInvalidInput) {
				t.Fatalf("want invalid input, got %v", err)
			}
		})
	}
	// Missing prices cannot hide invalid quantities.
	delete(prices, "usd")
	holdings[0].Quantity = decimal.New(1, math.MaxInt32)
	if _, err := valueUSD(at, time.Hour, currencies, holdings, prices); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("missing price bypassed quantity validation: %v", err)
	}
}

func TestValueUSD_UIDIdentityAndUTC(t *testing.T) {
	at, currencies, _, prices := fixture()
	other := currencies["usd"]
	other.UID = "other-usd"
	currencies[other.UID] = other // Same display code does not share a price.
	got, err := valueUSD(at.In(time.FixedZone("display", 8*3600)), time.Hour, currencies,
		[]holding{{CurrencyUID: "other-usd", Quantity: decimal.NewFromInt(1)}}, prices)
	if err != nil {
		t.Fatal(err)
	}
	if got.Complete || got.Rows[0].Status != priceMissing || got.AsOf.Location() != time.UTC {
		t.Fatalf("currency identity or UTC violated: %+v", got)
	}
}

func TestValueUSD_EmptyAndUnpricedPortfolios(t *testing.T) {
	at, currencies, _, _ := fixture()
	got, err := valueUSD(at, time.Hour, currencies, nil, nil)
	if err != nil || !got.Complete || got.TotalUSD != "0" || got.Rows == nil {
		t.Fatalf("empty portfolio should be a complete zero total: %+v, %v", got, err)
	}
	holdings := []holding{{CurrencyUID: "usd", Quantity: decimal.Zero}}
	got, err = valueUSD(at, time.Hour, currencies, holdings, nil)
	if err != nil || got.Complete || got.SubtotalUSD != "0" || got.TotalUSD != "" || got.ValuedCount != 0 {
		t.Fatalf("unpriced zero holding should retain missing coverage: %+v, %v", got, err)
	}
	// Compare absolute instants, not saturated Duration differences.
	prices := map[string]price{"usd": {USDPerUnit: decimal.NewFromInt(1), Source: "ancient", AsOf: time.Date(2, 1, 1, 0, 0, 0, 0, time.UTC)}}
	got, err = valueUSD(at, time.Duration(math.MaxInt64), currencies, holdings, prices)
	if err != nil || got.Rows[0].Status != priceStale {
		t.Fatalf("ancient price was accepted: %+v, %v", got, err)
	}
}

func TestRun_PrintsIncompleteValuationAndPropagatesWriterFailure(t *testing.T) {
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatal(err)
	}
	var got valuation
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Complete || got.SubtotalUSD != "629.63" || got.Rows[2].Quantity.String() != "3" || got.Rows[2].Status != priceMissing {
		t.Fatalf("unexpected CLI example: %+v", got)
	}
	if err := run(failingWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("writer failure swallowed: %v", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func fixture() (time.Time, map[string]core.Currency, []holding, map[string]price) {
	at := time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)
	currencies := map[string]core.Currency{
		"usd":     {UID: "usd", Code: "USD", Exponent: 2},
		"btc":     {UID: "btc", Code: "BTC", Exponent: 8},
		"gift":    {UID: "gift", Code: "ROSE", Exponent: 0},
		"credits": {UID: "credits", Code: "CREDITS", Exponent: 3},
	}
	holdings := []holding{
		{CurrencyUID: "usd", Quantity: decimal.RequireFromString("12.34")},
		{CurrencyUID: "btc", Quantity: decimal.RequireFromString("0.01234570")},
		{CurrencyUID: "gift", Quantity: decimal.NewFromInt(3)},
		{CurrencyUID: "credits", Quantity: decimal.NewFromInt(5)},
	}
	prices := map[string]price{}
	for uid, value := range map[string]string{"usd": "1", "btc": "50000", "gift": "0.1", "credits": "0.001"} {
		prices[uid] = price{USDPerUnit: decimal.RequireFromString(value), Source: "fixture-" + uid, AsOf: at.Add(-time.Minute)}
	}
	return at, currencies, holdings, prices
}
