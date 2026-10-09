package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/shopspring/decimal"
)

// RateQuoter resolves the directed conversion a host has configured or
// sourced for one unit pair. It is the single port between "where rates come
// from" and "what the ledger does with one".
//
// The ledger stores no rates. A JSON file, a host database table with
// effective dates, a price oracle with a timestamp -- each is an adapter
// behind this one method, and each fills FixedRate.Version with whatever
// identifies the quote in its own world (a configuration version, a feed id
// plus timestamp). The ledger does not distinguish the two.
//
// Resolve the quote OUTSIDE any database transaction (financial.md: no
// external calls inside one), then hand the FixedRate value to the operation
// -- (*ledger.Service).Exchange takes the value, never this port. Not found is
// ErrNotFound; a pair that exists but is misconfigured surfaces from
// FixedRate.Validate at conversion time.
type RateQuoter interface {
	QuoteRate(ctx context.Context, sourceCode, targetCode string) (FixedRate, error)
}

// ConversionQuotesMetadataKey is the journal metadata key under which the
// quotes applied to an operation are recorded, encoded by
// EncodeConversionQuotes. It is evidence, not configuration: it lets a retry
// with a changed price collide (metadata participates in the idempotency
// payload comparison) and lets the holder-facing statement explain a charge
// ("10,000 input tokens at 0.002"). JournalInput.Validate rejects a value under
// this key that DecodeConversionQuotes cannot read, so the read side may treat
// a decode failure as an error rather than a shape to tolerate.
const ConversionQuotesMetadataKey = "conversion_quotes"

// FundingUIDMetadataKey is the journal metadata key an exchange records the
// uid of what funded it under -- typically the confirmed deposit booking (or
// deposit journal) whose arrival triggered a credits purchase. The ledger only
// stores it; a host's business reconciliation joins confirmed deposits against
// exchanges carrying their uid to find "funded but never converted", a gap
// per-currency balance checks cannot see.
const FundingUIDMetadataKey = "funding_uid"

// ConversionQuote is the immutable record of one applied conversion: which
// units, at what precision, how much went in, what rate and rounding were
// used, and what came out. It is what FixedRate.Quote returns and what
// travels in ConversionQuotesMetadataKey.
type ConversionQuote struct {
	SourceCode     string          `json:"source_code"`
	TargetCode     string          `json:"target_code"`
	SourceExponent int32           `json:"source_exponent"`
	TargetExponent int32           `json:"target_exponent"`
	SourceQuantity decimal.Decimal `json:"source_quantity"`
	TargetAmount   decimal.Decimal `json:"target_amount"`
	Rate           decimal.Decimal `json:"rate"`
	Version        string          `json:"version"`
	Rounding       RoundingMode    `json:"rounding"`
}

// Validate checks the quote's own consistency. It does not recompute the
// conversion: a stored quote is evidence of what was applied, and the pair's
// currencies may since have been retired.
//
// SourceCode and TargetCode follow the currency-code rule CurrencyInput
// enforces -- 1-64 characters from [A-Za-z0-9_.-] -- and must differ; Version
// must be non-blank, valid UTF-8 with no control characters
// (validateQuoteVersion). The
// holder statement renders them verbatim, so a code carrying whitespace,
// control or Unicode format characters (a right-to-left override, a
// zero-width joiner) is refused rather than shown.
func (q ConversionQuote) Validate() error {
	if err := validateCurrencyCode("core: conversion quote", "source_code", q.SourceCode); err != nil {
		return err
	}
	if err := validateCurrencyCode("core: conversion quote", "target_code", q.TargetCode); err != nil {
		return err
	}
	if q.SourceCode == q.TargetCode {
		return fmt.Errorf("core: conversion quote: source_code and target_code must be distinct: %w", ErrInvalidInput)
	}
	if err := validateQuoteVersion("core: conversion quote", q.Version); err != nil {
		return err
	}
	if q.SourceExponent < 0 || q.SourceExponent > MaxAmountFractionalDigits ||
		q.TargetExponent < 0 || q.TargetExponent > MaxAmountFractionalDigits {
		return fmt.Errorf("core: conversion quote: exponents must be between 0 and %d: %w", MaxAmountFractionalDigits, ErrInvalidInput)
	}
	if _, err := ParseRoundingMode(q.Rounding.String()); err != nil {
		return fmt.Errorf("core: conversion quote: %w", err)
	}
	if !q.Rate.IsPositive() {
		return fmt.Errorf("core: conversion quote: rate must be positive: %w", ErrInvalidInput)
	}
	if q.SourceQuantity.IsNegative() || q.TargetAmount.IsNegative() {
		return fmt.Errorf("core: conversion quote: quantity and amount must not be negative: %w", ErrInvalidInput)
	}
	for name, v := range map[string]decimal.Decimal{"rate": q.Rate, "source_quantity": q.SourceQuantity, "target_amount": q.TargetAmount} {
		if err := validateAmountIsRescalable("conversion quote", name, v); err != nil {
			return err
		}
	}
	return nil
}

// Quote converts quantity exactly as Convert does and returns the complete
// snapshot of what was applied. Persist the result with the operation; a
// retry must reuse it, never re-quote.
func (r FixedRate) Quote(quantity decimal.Decimal, source, target Currency) (ConversionQuote, error) {
	amount, err := r.Convert(quantity, source, target)
	if err != nil {
		return ConversionQuote{}, err
	}
	return ConversionQuote{
		SourceCode:     source.Code,
		TargetCode:     target.Code,
		SourceExponent: source.Exponent,
		TargetExponent: target.Exponent,
		SourceQuantity: quantity,
		TargetAmount:   amount,
		Rate:           r.Rate,
		Version:        r.Version,
		Rounding:       r.Rounding,
	}, nil
}

// EncodeConversionQuotes renders quotes as the compact JSON array stored under
// ConversionQuotesMetadataKey. Decimals are strings and the rounding mode is
// its name, the same wire discipline as every other amount and enum. An empty
// list is refused: "no conversion happened" is the key's absence, not an empty
// value that reads as a conversion with nothing in it.
func EncodeConversionQuotes(quotes []ConversionQuote) (string, error) {
	if len(quotes) == 0 {
		return "", fmt.Errorf("core: encode conversion quotes: at least one quote required: %w", ErrInvalidInput)
	}
	for i, q := range quotes {
		if err := q.Validate(); err != nil {
			return "", fmt.Errorf("core: encode conversion quotes[%d]: %w", i, err)
		}
	}
	encoded, err := json.Marshal(quotes)
	if err != nil {
		return "", fmt.Errorf("core: encode conversion quotes: %w", err)
	}
	return string(encoded), nil
}

// DecodeConversionQuotes parses what EncodeConversionQuotes produced and
// validates every quote. Unknown fields are an error: a snapshot with extra
// fields is a different contract, and tolerating it here would let two
// encodings coexist under one key. For the same reason the decimal fields
// (source_quantity, rate, target_amount) must be JSON strings: a bare number,
// null or an absent field is refused even where it would parse.
func DecodeConversionQuotes(encoded string) ([]ConversionQuote, error) {
	if strings.TrimSpace(encoded) == "" {
		return nil, fmt.Errorf("core: decode conversion quotes: empty value: %w", ErrInvalidInput)
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(encoded)))
	dec.DisallowUnknownFields()
	var quotes []ConversionQuote
	if err := dec.Decode(&quotes); err != nil {
		return nil, fmt.Errorf("core: decode conversion quotes: %v: %w", err, ErrInvalidInput)
	}
	// More reports array/object elements, not top-level EOF: an extra
	// closing delimiter can make it return false even with invalid data left.
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("core: decode conversion quotes: trailing data: %w", ErrInvalidInput)
	}
	if len(quotes) == 0 {
		return nil, fmt.Errorf("core: decode conversion quotes: empty list: %w", ErrInvalidInput)
	}
	if err := requireStringDecimals(encoded); err != nil {
		return nil, err
	}
	for i, q := range quotes {
		if err := q.Validate(); err != nil {
			return nil, fmt.Errorf("core: decode conversion quotes[%d]: %w", i, err)
		}
	}
	return quotes, nil
}

// conversionQuoteDecimalFields are the ConversionQuote fields that are
// decimals on the wire.
var conversionQuoteDecimalFields = [...]string{"source_quantity", "rate", "target_amount"}

// requireStringDecimals refuses any spelling of a decimal field other than
// the JSON string EncodeConversionQuotes writes. shopspring/decimal would
// also accept a bare JSON number ({"rate":1000}) or null, and since metadata
// takes part in the idempotency payload comparison, two spellings of one
// quote would compare unequal -- a retry that re-encoded the same quote
// would conflict with its own original. One key, one encoding: a number,
// null or an absent field is refused. Called only after the strict decode
// has succeeded, so the shape is already known to be an array of objects.
func requireStringDecimals(encoded string) error {
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(encoded), &raw); err != nil {
		return fmt.Errorf("core: decode conversion quotes: %v: %w", err, ErrInvalidInput)
	}
	for i, q := range raw {
		for _, field := range conversionQuoteDecimalFields {
			v := bytes.TrimSpace(q[field])
			if len(v) == 0 || v[0] != '"' {
				return fmt.Errorf("core: decode conversion quotes[%d]: %s must be a decimal string: %w", i, field, ErrInvalidInput)
			}
		}
	}
	return nil
}

// validateConversionQuotesMetadata is JournalInput.Validate's hook: the one
// well-known typed metadata key must decode, everything else stays free-form.
func validateConversionQuotesMetadata(scope string, metadata map[string]string) error {
	encoded, ok := metadata[ConversionQuotesMetadataKey]
	if !ok {
		return nil
	}
	if _, err := DecodeConversionQuotes(encoded); err != nil {
		return fmt.Errorf("core: %s: metadata[%q]: %w", scope, ConversionQuotesMetadataKey, err)
	}
	return nil
}

// roundingModeNames is the single spelling table for RoundingMode's text
// form, shared by String, ParseRoundingMode and the JSON codec.
var roundingModeNames = map[RoundingMode]string{
	RoundHalfUp:   "half_up",
	RoundHalfEven: "half_even",
	RoundDown:     "down",
	RoundUp:       "up",
}

// String returns the mode's snake_case name (api-contract.md §7), or a
// diagnostic form for a value outside the enum.
func (m RoundingMode) String() string {
	if name, ok := roundingModeNames[m]; ok {
		return name
	}
	return fmt.Sprintf("rounding_mode(%d)", int(m))
}

// ParseRoundingMode is String's inverse. Unknown names are ErrInvalidInput.
func ParseRoundingMode(name string) (RoundingMode, error) {
	for mode, n := range roundingModeNames {
		if n == name {
			return mode, nil
		}
	}
	return 0, fmt.Errorf("core: unknown rounding mode %q: %w", name, ErrInvalidInput)
}

// MarshalText encodes the mode by name, so any JSON that carries a
// RoundingMode carries "half_up", not 0.
func (m RoundingMode) MarshalText() ([]byte, error) {
	name, ok := roundingModeNames[m]
	if !ok {
		return nil, fmt.Errorf("core: marshal rounding mode: %s: %w", m, ErrInvalidInput)
	}
	return []byte(name), nil
}

// UnmarshalText is MarshalText's inverse.
func (m *RoundingMode) UnmarshalText(text []byte) error {
	mode, err := ParseRoundingMode(string(text))
	if err != nil {
		return err
	}
	*m = mode
	return nil
}
