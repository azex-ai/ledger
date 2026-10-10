package core

import (
	"math"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// Exercise the first disallowed values without using a pathological exponent:
// these cases can safely demonstrate the missing check on an unfixed version.
func TestRound_RejectsExponentOutsideWorkingRange(t *testing.T) {
	for _, exponent := range []int32{-37, 37} {
		for _, mode := range []RoundingMode{RoundHalfUp, RoundHalfEven, RoundDown, RoundUp, RoundingMode(99)} {
			_, err := Round(decimal.NewFromInt(1), exponent, mode)
			require.ErrorIs(t, err, ErrInvalidInput, "exponent=%d mode=%d", exponent, mode)
		}
	}
}

func TestConvertAt_RejectsExponentOutsideWorkingRange(t *testing.T) {
	for _, exponent := range []int32{-37, 37} {
		_, err := ConvertAt(decimal.NewFromInt(1), decimal.NewFromInt(1), exponent, RoundHalfUp)
		require.ErrorIs(t, err, ErrInvalidInput, "exponent=%d", exponent)
	}
}

func TestAllocate_RejectsExponentOutsideWorkingRange(t *testing.T) {
	for _, exponent := range []int32{-1, 37} {
		_, err := Allocate(decimal.NewFromInt(1), []decimal.Decimal{decimal.NewFromInt(1)}, exponent)
		require.ErrorIs(t, err, ErrInvalidInput, "exponent=%d", exponent)
	}
}

func TestRound_ExtremeTargetExponentRejected(t *testing.T) {
	for _, exponent := range []int32{math.MinInt32, -1_000_000_000, 1_000_000_000, math.MaxInt32} {
		for _, amount := range []decimal.Decimal{decimal.NewFromInt(1), decimal.NewFromInt(-1), decimal.Zero} {
			for _, mode := range []RoundingMode{RoundHalfUp, RoundHalfEven, RoundDown, RoundUp, RoundingMode(99)} {
				_, err := Round(amount, exponent, mode)
				require.ErrorIs(t, err, ErrInvalidInput, "exponent=%d mode=%d", exponent, mode)
				require.ErrorContains(t, err, "target exponent")
			}
		}
	}
}

func TestConvertAt_ExtremeTargetExponentRejected(t *testing.T) {
	for _, exponent := range []int32{math.MinInt32, -1_000_000_000, 1_000_000_000, math.MaxInt32} {
		_, err := ConvertAt(decimal.NewFromInt(1), decimal.NewFromInt(2), exponent, RoundHalfUp)
		require.ErrorIs(t, err, ErrInvalidInput, "exponent=%d", exponent)
		require.ErrorContains(t, err, "target exponent")
	}

	// The target is rejected even when the operands themselves would fail
	// magnitude validation or yield a product outside the working range.
	for _, amount := range []decimal.Decimal{decimal.New(1, math.MaxInt32), decimal.NewFromInt(999_999_999_999)} {
		_, err := ConvertAt(amount, amount, math.MaxInt32, RoundHalfUp)
		require.ErrorIs(t, err, ErrInvalidInput)
		require.ErrorContains(t, err, "target exponent", "target validation must precede operand expansion")
	}
}

func TestAllocate_ExtremeTargetExponentRejected(t *testing.T) {
	for _, exponent := range []int32{math.MinInt32, -1_000_000_000, 1_000_000_000, math.MaxInt32} {
		for _, total := range []decimal.Decimal{decimal.NewFromInt(1), decimal.NewFromInt(-1), decimal.Zero} {
			_, err := Allocate(total, []decimal.Decimal{decimal.NewFromInt(1), decimal.NewFromInt(1)}, exponent)
			require.ErrorIs(t, err, ErrInvalidInput, "exponent=%d", exponent)
			require.ErrorContains(t, err, "target exponent")
		}
	}
}

func TestRound_NegativeExponentModes(t *testing.T) {
	for _, tc := range []struct {
		mode RoundingMode
		want int64
	}{
		{RoundHalfUp, 1300},
		{RoundHalfEven, 1200},
		{RoundDown, 1200},
		{RoundUp, 1300},
		{RoundingMode(99), 1300}, // preserve the existing default mode
	} {
		for _, sign := range []int64{1, -1} {
			got, err := Round(decimal.NewFromInt(1250*sign), -2, tc.mode)
			require.NoError(t, err)
			require.True(t, got.Equal(decimal.NewFromInt(tc.want*sign)), "mode=%d sign=%d got=%s", tc.mode, sign, got)
			converted, err := ConvertAt(decimal.NewFromInt(625*sign), decimal.NewFromInt(2), -2, tc.mode)
			require.NoError(t, err)
			require.True(t, converted.Equal(got), "conversion must retain the same coarse rounding")
		}
	}
}

func TestRound_WorkingExponentBounds(t *testing.T) {
	for _, mode := range []RoundingMode{RoundHalfUp, RoundHalfEven, RoundDown, RoundUp} {
		// An input at the helper's 36-digit working precision is preserved;
		// clipping the target to a currency's 18 digits would lose information.
		amount := decimal.New(12345, -36)
		got, err := Round(amount, 36, mode)
		require.NoError(t, err)
		require.True(t, got.Equal(amount))
		converted, err := ConvertAt(decimal.New(12345, -18), decimal.New(1, -18), 36, mode)
		require.NoError(t, err)
		require.True(t, converted.Equal(amount))

		got, err = Round(decimal.NewFromInt(1), -36, mode)
		require.NoError(t, err)
		want := decimal.Zero
		if mode == RoundUp {
			want = decimal.New(1, 36)
		}
		require.True(t, got.Equal(want))
		converted, err = ConvertAt(decimal.NewFromInt(1), decimal.NewFromInt(1), -36, mode)
		require.NoError(t, err)
		require.True(t, converted.Equal(want))
	}
}

func TestAllocate_WorkingExponentBoundaryConservesTotal(t *testing.T) {
	// The nonterminating 1/3 split exercises real use of the 36-digit target
	// and the deterministic remainder, beyond ordinary currency precision.
	for _, sign := range []int64{1, -1} {
		total := decimal.NewFromInt(sign)
		shares, err := Allocate(total, []decimal.Decimal{decimal.NewFromInt(1), decimal.NewFromInt(1), decimal.NewFromInt(1)}, 36)
		require.NoError(t, err)
		require.Len(t, shares, 3)
		expected := []decimal.Decimal{
			decimal.RequireFromString("0.333333333333333333333333333333333334"),
			decimal.RequireFromString("0.333333333333333333333333333333333333"),
			decimal.RequireFromString("0.333333333333333333333333333333333333"),
		}
		sum := decimal.Zero
		for i, share := range shares {
			want := expected[i].Mul(decimal.NewFromInt(sign))
			require.True(t, share.Equal(want), "share[%d]=%s want %s", i, share, want)
			sum = sum.Add(share)
		}
		require.True(t, sum.Equal(total))
	}
}
