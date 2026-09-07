package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
)

//go:embed rates.json
var defaultRates []byte

// Configuration belongs to this host. Only FixedRate and Currency are library
// contracts; files, configuration storage and active-version selection are not.
type pricing struct {
	units map[string]core.Currency
	rates map[[2]string]core.FixedRate
}

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
		mode, ok := roundingModes[configured.Rounding]
		if !ok {
			return pricing{}, fmt.Errorf("pricing: unknown rounding %q: %w", configured.Rounding, core.ErrInvalidInput)
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

var roundingModes = map[string]core.RoundingMode{
	"half_up": core.RoundHalfUp, "half_even": core.RoundHalfEven,
	"down": core.RoundDown, "up": core.RoundUp,
}

func (p pricing) rate(source, target string) (core.FixedRate, error) {
	rate, ok := p.rates[[2]string{source, target}]
	if !ok {
		return core.FixedRate{}, fmt.Errorf("pricing: no rate for %s → %s: %w", source, target, core.ErrNotFound)
	}
	return rate, nil
}

type usageQuantity struct {
	sourceCode string
	quantity   decimal.Decimal
}

type pricedAmount struct {
	amount    decimal.Decimal
	snapshots []map[string]string
}

func (p pricing) price(target core.Currency, usage ...usageQuantity) (pricedAmount, error) {
	configured, ok := p.units[target.Code]
	if !ok || configured.Exponent != target.Exponent || len(usage) == 0 {
		return pricedAmount{}, fmt.Errorf("pricing: target precision or usage does not match configuration: %w", core.ErrInvalidInput)
	}
	result := pricedAmount{amount: decimal.Zero}
	for _, item := range usage {
		rate, err := p.rate(item.sourceCode, target.Code)
		if err != nil {
			return pricedAmount{}, err
		}
		quote, err := quoteRate(rate, item.quantity, p.units[item.sourceCode], target)
		if err != nil {
			return pricedAmount{}, err
		}
		result.amount = result.amount.Add(quote.amount)
		if err := core.ValidateAmountMagnitude("pricing", "total", result.amount); err != nil {
			return pricedAmount{}, err
		}
		result.snapshots = append(result.snapshots, quote.snapshots...)
	}
	return result, nil
}

// Capture the quote used for this operation. Journal idempotency compares this
// metadata even if two different quantities/rates round to the same debit.
func quoteRate(rate core.FixedRate, quantity decimal.Decimal, source, target core.Currency) (pricedAmount, error) {
	amount, err := rate.Convert(quantity, source, target)
	if err != nil {
		return pricedAmount{}, err
	}
	return pricedAmount{amount: amount, snapshots: []map[string]string{{
		"source_code": source.Code, "target_code": target.Code,
		"source_quantity": quantity.String(), "target_amount": amount.String(),
		"source_exponent": strconv.Itoa(int(source.Exponent)), "target_exponent": strconv.Itoa(int(target.Exponent)),
		"rate": rate.Rate.String(), "version": rate.Version, "rounding": strconv.Itoa(int(rate.Rounding)),
	}}}, nil
}

func (q pricedAmount) metadata() (map[string]string, error) {
	encoded, err := json.Marshal(q.snapshots)
	if err != nil {
		return nil, fmt.Errorf("pricing: encode quote: %w", err)
	}
	return map[string]string{"conversion_quotes": string(encoded)}, nil
}
