package core

import (
	"fmt"
	"math/big"
	"sort"

	"github.com/shopspring/decimal"
)

// RoundingMode selects how Round (and ConvertAt) resolve a value that falls
// between two representable amounts at the target exponent. The zero value
// (RoundHalfUp) is the conventional default for user-facing amounts.
type RoundingMode int

const (
	// RoundHalfUp rounds to the nearest representable amount, breaking exact
	// ties away from zero (5.45 -> 5.5, -5.45 -> -5.5). The conventional
	// default for prices and most user-facing totals.
	RoundHalfUp RoundingMode = iota
	// RoundHalfEven ("banker's rounding") breaks exact ties toward the
	// nearest even digit (5.45 -> 5.4, 5.55 -> 5.6). Use where repeated
	// rounding must not introduce a systematic bias (e.g. aggregating many
	// small roundings over time).
	RoundHalfEven
	// RoundDown truncates toward zero. Use when the platform must not round
	// in the counterparty's favor (e.g. a fee floor, or FX where any residue
	// must remain the platform's, not the user's).
	RoundDown
	// RoundUp rounds away from zero. Use when under-crediting the
	// counterparty is the unacceptable direction (e.g. minimum chargeable
	// unit, gas estimation).
	RoundUp
)

// Round rounds d to exponent decimal places using mode. exponent is
// typically a currency's Currency.Exponent. Round never fails — an
// unrecognized mode falls back to RoundHalfUp.
func Round(d decimal.Decimal, exponent int32, mode RoundingMode) decimal.Decimal {
	switch mode {
	case RoundHalfEven:
		return d.RoundBank(exponent)
	case RoundDown:
		return d.RoundDown(exponent)
	case RoundUp:
		return d.RoundUp(exponent)
	case RoundHalfUp:
		return d.Round(exponent)
	default:
		return d.Round(exponent)
	}
}

// ConvertAt converts amount to another currency at rate, rounding the result
// to targetExponent decimal places using mode. Callers must post the
// resulting entries themselves (e.g. via the fx_sell/fx_buy presets); the FX
// residue introduced by rounding is expected to land on a settlement account
// — see docs/COOKBOOK.md's rounding decision table.
func ConvertAt(amount, rate decimal.Decimal, targetExponent int32, mode RoundingMode) decimal.Decimal {
	return Round(amount.Mul(rate), targetExponent, mode)
}

// Allocate splits total into len(weights) shares proportional to weights,
// each rounded to at most exponent decimal places, such that the shares sum
// to exactly total — no cent is ever lost or manufactured.
//
// Algorithm: largest-remainder method computed in exact rational arithmetic
// (math/big.Rat), never floating point or decimal division (which would
// introduce precision loss the whole point of this function is to avoid).
// Each weight's ideal share is computed exactly, floored to exponent places,
// and the leftover (total minus the sum of floors, always a small whole
// number of "smallest units" at exponent precision) is distributed one unit
// at a time to the shares with the largest truncated remainder — ties broken
// by input order, so results are deterministic and reproducible.
//
// total may be negative (e.g. allocating a refund across several original
// charges): Allocate splits |total| and negates every resulting share, so a
// -100 total split 50/50 by weight yields [-50, -50].
//
// total must already be exactly representable at exponent decimal places
// (Allocate never silently rounds its input — round total explicitly first
// if needed). weights must be non-negative and not all zero. The returned
// slice is in the same order as weights.
func Allocate(total decimal.Decimal, weights []decimal.Decimal, exponent int32) ([]decimal.Decimal, error) {
	if len(weights) == 0 {
		return nil, fmt.Errorf("core: allocate: weights must not be empty: %w", ErrInvalidInput)
	}
	if exponent < 0 {
		return nil, fmt.Errorf("core: allocate: exponent must not be negative: %w", ErrInvalidInput)
	}
	if !total.Equal(total.Truncate(exponent)) {
		return nil, fmt.Errorf("core: allocate: total %s has more than %d decimal place(s): %w", total.String(), exponent, ErrInvalidInput)
	}

	totalWeight := decimal.Zero
	for i, w := range weights {
		if w.IsNegative() {
			return nil, fmt.Errorf("core: allocate: weight[%d] must not be negative: %w", i, ErrInvalidInput)
		}
		totalWeight = totalWeight.Add(w)
	}
	if !totalWeight.IsPositive() {
		return nil, fmt.Errorf("core: allocate: weights must not all be zero: %w", ErrInvalidInput)
	}

	negative := total.IsNegative()
	// total is already validated exact at `exponent` places, so shifting by
	// exponent and taking the integer component is lossless: absUnits is the
	// exact count of "smallest units" (10^-exponent) in |total|.
	absUnits := total.Abs().Shift(exponent).BigInt()
	totalWeightRat := totalWeight.Rat()

	type allocation struct {
		idx       int
		floor     *big.Int
		remainder *big.Rat // fractional part of the ideal (unrounded) share, in [0, 1)
	}

	allocations := make([]allocation, len(weights))
	floorSum := new(big.Int)
	for i, w := range weights {
		// idealUnits = absUnits * w_i / totalWeight, exact rational — no
		// decimal.Div/DivisionPrecision involved anywhere in this function.
		idealUnits := new(big.Rat).SetInt(absUnits)
		idealUnits.Mul(idealUnits, w.Rat())
		idealUnits.Quo(idealUnits, totalWeightRat)

		// idealUnits >= 0 always (absUnits >= 0, w_i >= 0), so truncating
		// integer division (Num/Denom, both non-negative in a normalized
		// big.Rat) is equivalent to floor.
		floor := new(big.Int).Quo(idealUnits.Num(), idealUnits.Denom())
		remainder := new(big.Rat).Sub(idealUnits, new(big.Rat).SetInt(floor))

		allocations[i] = allocation{idx: i, floor: floor, remainder: remainder}
		floorSum.Add(floorSum, floor)
	}

	// leftover = absUnits - floorSum = sum of all remainders, which (being a
	// sum of len(weights) values each < 1) is a non-negative integer strictly
	// less than len(weights) — safe to distribute one unit per allocation.
	leftover := new(big.Int).Sub(absUnits, floorSum)

	sort.SliceStable(allocations, func(a, b int) bool {
		if cmp := allocations[a].remainder.Cmp(allocations[b].remainder); cmp != 0 {
			return cmp > 0
		}
		return allocations[a].idx < allocations[b].idx
	})

	leftoverN := leftover.Int64() // bounded by len(weights); safe to narrow
	for i := int64(0); i < leftoverN && i < int64(len(allocations)); i++ {
		allocations[i].floor.Add(allocations[i].floor, big.NewInt(1))
	}

	result := make([]decimal.Decimal, len(weights))
	for _, a := range allocations {
		share := decimal.NewFromBigInt(a.floor, -exponent)
		if negative {
			share = share.Neg()
		}
		result[a.idx] = share
	}
	return result, nil
}
