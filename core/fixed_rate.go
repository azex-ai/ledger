package core

import (
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
)

// FixedRate is a versioned, directed conversion: one SourceCode unit is worth
// Rate TargetCode units. Rate precision is independent of either currency's
// exponent. Currency metadata supplies quantity precision at conversion time.
//
// Hosts may configure this value directly or decode it from configuration.
// Persist the original rate, version, units, exponents, rounding mode and quantity
// with each operation: a retry must use that snapshot, not a newly selected rate.
// FixedRate neither reserves funds nor posts journals.
type FixedRate struct {
	SourceCode string          `json:"source_code"`
	TargetCode string          `json:"target_code"`
	Rate       decimal.Decimal `json:"rate"`
	Version    string          `json:"version"`
	Rounding   RoundingMode    `json:"rounding"`
}

// Validate checks the configured pair, version, rate, rounding and precision.
// Source and target may describe measured units: their UID, Name and IsActive
// fields are not required. Wallet operations must independently resolve and
// authorize the actual stored currencies; this is only a pure conversion value.
func (r FixedRate) Validate(source, target Currency) error {
	if strings.TrimSpace(r.SourceCode) == "" || strings.TrimSpace(r.TargetCode) == "" || r.SourceCode == r.TargetCode {
		return fmt.Errorf("core: fixed rate: source_code and target_code must be non-empty and distinct: %w", ErrInvalidInput)
	}
	if source.Code != r.SourceCode || target.Code != r.TargetCode {
		return fmt.Errorf("core: fixed rate: currency metadata does not match the directed pair: %w", ErrInvalidInput)
	}
	if strings.TrimSpace(r.Version) == "" {
		return fmt.Errorf("core: fixed rate: version required: %w", ErrInvalidInput)
	}
	if source.Exponent < 0 || source.Exponent > MaxAmountFractionalDigits ||
		target.Exponent < 0 || target.Exponent > MaxAmountFractionalDigits {
		return fmt.Errorf("core: fixed rate: currency exponents must be between 0 and %d: %w", MaxAmountFractionalDigits, ErrInvalidInput)
	}
	switch r.Rounding {
	case RoundHalfUp, RoundHalfEven, RoundDown, RoundUp:
	default:
		return fmt.Errorf("core: fixed rate: invalid rounding mode: %w", ErrInvalidInput)
	}
	if !r.Rate.IsPositive() {
		return fmt.Errorf("core: fixed rate: rate must be positive: %w", ErrInvalidInput)
	}
	return validateAmountIsRescalable("fixed rate", "rate", r.Rate)
}

// Convert validates quantity at source.Exponent, then delegates multiplication
// and target rounding to ConvertAt. It never rounds the source quantity.
// Invalid configuration, negative quantities and amounts outside storage bounds
// return ErrInvalidInput; excess source precision returns ErrPrecisionExceeded.
// Zero quantity and a rounded zero result are valid conversions. Callers must
// handle them without posting a zero-amount journal.
func (r FixedRate) Convert(quantity decimal.Decimal, source, target Currency) (decimal.Decimal, error) {
	if err := r.Validate(source, target); err != nil {
		return decimal.Decimal{}, err
	}
	if quantity.IsNegative() {
		return decimal.Decimal{}, fmt.Errorf("core: fixed rate: quantity must not be negative: %w", ErrInvalidInput)
	}
	// Bound the input before Truncate/Equal can rescale a hostile exponent.
	if err := ValidateAmountMagnitude("fixed rate", "quantity", quantity); err != nil {
		return decimal.Decimal{}, err
	}
	if !quantity.Equal(quantity.Truncate(source.Exponent)) {
		return decimal.Decimal{}, fmt.Errorf("core: fixed rate: quantity exceeds source precision: %w", ErrPrecisionExceeded)
	}
	// A zero's representational scale must not consume the working precision
	// available to the rate. This changes only our local decimal value.
	if quantity.IsZero() {
		quantity = decimal.Zero
	}
	amount, err := ConvertAt(quantity, r.Rate, target.Exponent, r.Rounding)
	if err != nil {
		return decimal.Decimal{}, err
	}
	// Rounding can carry a valid twelve-digit intermediate into a thirteenth
	// integer digit. The amount returned to bookkeeping must still fit storage.
	if err := ValidateAmountMagnitude("fixed rate", "converted amount", amount); err != nil {
		return decimal.Decimal{}, err
	}
	return amount, nil
}
