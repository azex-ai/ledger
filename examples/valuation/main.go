// Command valuation demonstrates a host-owned USD read model using local data.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/azex-ai/ledger/core"
	"github.com/shopspring/decimal"
)

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func run(w io.Writer) error {
	// Fixtures, not live prices. A real host supplies its original balances,
	// currency metadata and already-resolved price observations here.
	asOf := time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)
	currencies := map[string]core.Currency{
		"fiat-usd":    {UID: "fiat-usd", Code: "USD", Exponent: 2},
		"crypto-btc":  {UID: "crypto-btc", Code: "BTC", Exponent: 8},
		"gift-rose":   {UID: "gift-rose", Code: "ROSE", Exponent: 0},
		"app-credits": {UID: "app-credits", Code: "CREDITS", Exponent: 3},
	}
	holdings := []holding{
		{CurrencyUID: "fiat-usd", Quantity: decimal.RequireFromString("12.34")},
		{CurrencyUID: "crypto-btc", Quantity: decimal.RequireFromString("0.01234570")},
		{CurrencyUID: "gift-rose", Quantity: decimal.NewFromInt(3)},
		{CurrencyUID: "app-credits", Quantity: decimal.NewFromInt(5)},
	}
	prices := map[string]price{
		"fiat-usd":    {USDPerUnit: decimal.NewFromInt(1), Source: "fixture:usd-numeraire", AsOf: asOf},
		"crypto-btc":  {USDPerUnit: decimal.NewFromInt(50000), Source: "fixture:market", AsOf: asOf.Add(-time.Minute)},
		"app-credits": {USDPerUnit: decimal.RequireFromString("0.001"), Source: "fixture:business-config-v1", AsOf: asOf},
		// ROSE deliberately has no price. Its quantity still appears.
	}
	result, err := valueUSD(asOf, 5*time.Minute, currencies, holdings, prices)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		return fmt.Errorf("valuation: encode output: %w", err)
	}
	return nil
}
