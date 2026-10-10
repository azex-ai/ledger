package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/azex-ai/ledger/core"
	"github.com/shopspring/decimal"
)

// These are host-owned read-model types, not new ledger APIs. A holding is
// already aggregated by CurrencyUID; the host selects the balance roles it
// intends to value before calling valueUSD.
type holding struct {
	CurrencyUID string          `json:"currency_uid"`
	Quantity    decimal.Decimal `json:"quantity"`
}

// A map entry means a quote was explicitly supplied, even when its price is
// zero. An absent map entry means unknown, never a zero-priced asset.
type price struct {
	USDPerUnit decimal.Decimal `json:"usd_per_unit"`
	Source     string          `json:"source"`
	AsOf       time.Time       `json:"as_of"`
}

type priceStatus string

const (
	priceValid   priceStatus = "valid"
	priceMissing priceStatus = "missing"
	priceStale   priceStatus = "stale"
	priceFuture  priceStatus = "future"
)

type valuedHolding struct {
	CurrencyUID  string          `json:"currency_uid"`
	CurrencyCode string          `json:"currency_code"`
	Quantity     decimal.Decimal `json:"quantity"`
	Status       priceStatus     `json:"status"`
	UnitPriceUSD string          `json:"unit_price_usd,omitempty"`
	Source       string          `json:"source,omitempty"`
	PriceAsOf    time.Time       `json:"price_as_of,omitzero"`
	ValueUSD     string          `json:"value_usd,omitempty"`
}

type valuation struct {
	AsOf        time.Time       `json:"as_of"`
	Rows        []valuedHolding `json:"rows"`
	ValuedCount int             `json:"valued_count"`
	Complete    bool            `json:"complete"`
	SubtotalUSD string          `json:"subtotal_usd"`
	TotalUSD    string          `json:"total_usd,omitempty"`
}

// valueUSD values non-negative holdings without changing any ledger quantity.
// Prices are selected by UID, not display code. Fresh means 0 <= age <= maxAge;
// stale and future quotes remain visible as evidence but contribute no value.
// Missing quotes likewise make the result incomplete, including for zero
// holdings. Malformed inputs fail instead of becoming missing prices.
//
// Quantities and prices use the core storage bounds (12 integer / 18 fractional
// digits). Products preserve up to 36 fractional digits, with no cent rounding.
// Products and the subtotal must fit the core 12-integer-digit working bound.
// The explicit time makes pricing policy deterministic; there is no live feed,
// global clock, rate persistence, database access or financial authorization.
func valueUSD(asOf time.Time, maxAge time.Duration, currencies map[string]core.Currency, holdings []holding, prices map[string]price) (valuation, error) {
	if asOf.IsZero() || maxAge <= 0 {
		return valuation{}, fmt.Errorf("valuation: timestamp and positive max age required: %w", core.ErrInvalidInput)
	}
	out := valuation{AsOf: asOf.UTC(), Rows: make([]valuedHolding, 0, len(holdings)), Complete: true}
	subtotal := decimal.Zero
	seen := make(map[string]bool, len(holdings))
	for i, h := range holdings {
		currency, ok := currencies[h.CurrencyUID]
		if !ok || h.CurrencyUID == "" || currency.UID != h.CurrencyUID || strings.TrimSpace(currency.Code) == "" {
			return valuation{}, fmt.Errorf("valuation: holding %d has unknown or mismatched currency: %w", i, core.ErrInvalidInput)
		}
		if seen[h.CurrencyUID] {
			return valuation{}, fmt.Errorf("valuation: holding %d duplicates a currency: %w", i, core.ErrInvalidInput)
		}
		seen[h.CurrencyUID] = true
		if currency.Exponent < 0 || currency.Exponent > core.MaxAmountFractionalDigits {
			return valuation{}, fmt.Errorf("valuation: holding %d has invalid currency exponent: %w", i, core.ErrInvalidInput)
		}
		// Before Truncate, Equal, String or any arithmetic that rescales a
		// decimal. Validate even if this holding has no usable price.
		if err := core.ValidateAmountMagnitude("valuation", "quantity", h.Quantity); err != nil {
			return valuation{}, err
		}
		if h.Quantity.IsNegative() || !h.Quantity.Equal(h.Quantity.Truncate(currency.Exponent)) {
			return valuation{}, fmt.Errorf("valuation: holding %d has negative quantity or excess precision: %w", i, core.ErrInvalidInput)
		}
		row := valuedHolding{CurrencyUID: h.CurrencyUID, CurrencyCode: currency.Code, Quantity: h.Quantity, Status: priceMissing}
		if p, ok := prices[h.CurrencyUID]; ok {
			if err := core.ValidateAmountMagnitude("valuation", "usd_per_unit", p.USDPerUnit); err != nil {
				return valuation{}, err
			}
			if p.USDPerUnit.IsNegative() || strings.TrimSpace(p.Source) == "" || p.AsOf.IsZero() {
				return valuation{}, fmt.Errorf("valuation: holding %d needs non-negative price, source and timestamp: %w", i, core.ErrInvalidInput)
			}
			row.UnitPriceUSD, row.Source, row.PriceAsOf = p.USDPerUnit.String(), p.Source, p.AsOf.UTC()
			switch {
			case p.AsOf.After(asOf):
				row.Status = priceFuture
			case p.AsOf.Before(asOf.Add(-maxAge)):
				row.Status = priceStale
			default:
				row.Status = priceValid
				// Both inputs have at most 18 fractional digits, so the fixed
				// 36-place target preserves their exact product. No display
				// rounding feeds back into holdings or the aggregate.
				value, err := core.ConvertAt(h.Quantity, p.USDPerUnit, core.MaxAmountWorkingFractionalDigits, core.RoundHalfEven)
				if err != nil {
					return valuation{}, fmt.Errorf("valuation: holding %d product: %w", i, err)
				}
				subtotal, err = core.Round(subtotal.Add(value), core.MaxAmountWorkingFractionalDigits, core.RoundHalfEven)
				if err != nil {
					return valuation{}, fmt.Errorf("valuation: subtotal: %w", err)
				}
				row.ValueUSD = value.String()
				out.ValuedCount++
			}
		}
		if row.Status != priceValid {
			out.Complete = false
		}
		out.Rows = append(out.Rows, row)
	}
	out.SubtotalUSD = subtotal.String()
	if out.Complete {
		out.TotalUSD = out.SubtotalUSD
	}
	return out, nil
}
