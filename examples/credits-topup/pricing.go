package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"

	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
)

//go:embed rates.json
var defaultRates []byte

// pricing is this host's core.RateQuoter adapter: a JSON file of units and
// directed rates. The file, its storage and which version is active are host
// decisions; the ledger only sees FixedRate values through QuoteRate.
//
// A second host might back the same port with a database table carrying
// effective dates, or an oracle for real currency pairs. Nothing in the
// ledger changes for either.
type pricing struct {
	units map[string]core.Currency
	rates map[[2]string]core.FixedRate
}

var _ core.RateQuoter = pricing{}

func parsePricing(data []byte) (pricing, error) {
	var input struct {
		Units []core.Currency `json:"units"`
		Rates []struct {
			SourceCode string `json:"source_code"`
			TargetCode string `json:"target_code"`
			Rate       string `json:"rate"`
			Version    string `json:"version"`
			Rounding   string `json:"rounding"`
		} `json:"rates"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return pricing{}, fmt.Errorf("pricing: decode config: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return pricing{}, fmt.Errorf("pricing: expected a single config object: %w", core.ErrInvalidInput)
	}
	p := pricing{units: make(map[string]core.Currency), rates: make(map[[2]string]core.FixedRate)}
	for _, unit := range input.Units {
		if err := (core.CurrencyInput{Code: unit.Code, Name: unit.Name, Exponent: unit.Exponent}).Validate(); err != nil {
			return pricing{}, fmt.Errorf("pricing: unit: %w", err)
		}
		if _, exists := p.units[unit.Code]; exists {
			return pricing{}, fmt.Errorf("pricing: duplicate unit %q: %w", unit.Code, core.ErrInvalidInput)
		}
		p.units[unit.Code] = unit
	}
	for _, configured := range input.Rates {
		rate, err := decimal.NewFromString(configured.Rate)
		if err != nil {
			return pricing{}, fmt.Errorf("pricing: rate: %w", err)
		}
		mode, err := core.ParseRoundingMode(configured.Rounding)
		if err != nil {
			return pricing{}, fmt.Errorf("pricing: %w", err)
		}
		fixed := core.FixedRate{SourceCode: configured.SourceCode, TargetCode: configured.TargetCode,
			Rate: rate, Version: configured.Version, Rounding: mode}
		if err := fixed.Validate(p.units[fixed.SourceCode], p.units[fixed.TargetCode]); err != nil {
			return pricing{}, fmt.Errorf("pricing: configured pair: %w", err)
		}
		key := [2]string{fixed.SourceCode, fixed.TargetCode}
		if _, exists := p.rates[key]; exists {
			return pricing{}, fmt.Errorf("pricing: duplicate pair %v: %w", key, core.ErrInvalidInput)
		}
		p.rates[key] = fixed
	}
	if len(p.rates) == 0 {
		return pricing{}, fmt.Errorf("pricing: rates required: %w", core.ErrInvalidInput)
	}
	return p, nil
}

// QuoteRate implements core.RateQuoter over the configured directed pairs.
func (p pricing) QuoteRate(_ context.Context, sourceCode, targetCode string) (core.FixedRate, error) {
	rate, ok := p.rates[[2]string{sourceCode, targetCode}]
	if !ok {
		return core.FixedRate{}, fmt.Errorf("pricing: no rate for %s → %s: %w", sourceCode, targetCode, core.ErrNotFound)
	}
	return rate, nil
}

type usageQuantity struct {
	sourceCode string
	quantity   decimal.Decimal
}

// pricedAmount is a valuation: the credits a set of measured quantities cost,
// plus the quote applied to each line. Metered units need no wallet; only the
// resulting credits debit touches a balance.
type pricedAmount struct {
	amount decimal.Decimal
	quotes []core.ConversionQuote
}

func (p pricing) price(ctx context.Context, target core.Currency, usage ...usageQuantity) (pricedAmount, error) {
	configured, ok := p.units[target.Code]
	if !ok || configured.Exponent != target.Exponent || len(usage) == 0 {
		return pricedAmount{}, fmt.Errorf("pricing: target precision or usage does not match configuration: %w", core.ErrInvalidInput)
	}
	result := pricedAmount{amount: decimal.Zero}
	for _, item := range usage {
		rate, err := p.QuoteRate(ctx, item.sourceCode, target.Code)
		if err != nil {
			return pricedAmount{}, err
		}
		// Each line is rounded on its own at the target precision, then
		// summed. A cumulative-pricing host persists cumulative totals instead.
		quote, err := rate.Quote(item.quantity, p.units[item.sourceCode], target)
		if err != nil {
			return pricedAmount{}, err
		}
		result.amount = result.amount.Add(quote.TargetAmount)
		if err := core.ValidateAmountMagnitude("pricing", "total", result.amount); err != nil {
			return pricedAmount{}, err
		}
		result.quotes = append(result.quotes, quote)
	}
	return result, nil
}

// metadata renders the quotes as the journal metadata the ledger recognises.
// Journal idempotency compares this even when two different quantities or
// rates round to the same debit.
func (q pricedAmount) metadata() (map[string]string, error) {
	encoded, err := core.EncodeConversionQuotes(q.quotes)
	if err != nil {
		return nil, fmt.Errorf("pricing: encode quote: %w", err)
	}
	return map[string]string{core.ConversionQuotesMetadataKey: encoded}, nil
}
