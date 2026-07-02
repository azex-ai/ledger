package core

import (
	"errors"
	"math/big"
	"math/rand"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Round ---

func TestRound_HalfUp(t *testing.T) {
	cases := []struct {
		in, want string
		places   int32
	}{
		{"5.45", "5.5", 1},
		{"5.44", "5.4", 1},
		{"-5.45", "-5.5", 1},
		{"0.005", "0.01", 2},
		{"0", "0", 2},
		{"1.230000", "1.23", 2}, // already within precision — untouched
	}
	for _, tc := range cases {
		d := decimal.RequireFromString(tc.in)
		got := Round(d, tc.places, RoundHalfUp)
		assert.Truef(t, got.Equal(decimal.RequireFromString(tc.want)), "Round(%s, %d, HalfUp) = %s, want %s", tc.in, tc.places, got, tc.want)
	}
}

func TestRound_HalfEven(t *testing.T) {
	cases := []struct {
		in, want string
		places   int32
	}{
		{"5.45", "5.4", 1}, // 4 is even
		{"5.55", "5.6", 1}, // 6 is even
		{"5.65", "5.6", 1},
		{"-5.45", "-5.4", 1},
	}
	for _, tc := range cases {
		d := decimal.RequireFromString(tc.in)
		got := Round(d, tc.places, RoundHalfEven)
		assert.Truef(t, got.Equal(decimal.RequireFromString(tc.want)), "Round(%s, %d, HalfEven) = %s, want %s", tc.in, tc.places, got, tc.want)
	}
}

func TestRound_Down(t *testing.T) {
	cases := []struct {
		in, want string
		places   int32
	}{
		{"5.99", "5.9", 1},
		{"-5.99", "-5.9", 1},
		{"0.001", "0", 2},
	}
	for _, tc := range cases {
		d := decimal.RequireFromString(tc.in)
		got := Round(d, tc.places, RoundDown)
		assert.Truef(t, got.Equal(decimal.RequireFromString(tc.want)), "Round(%s, %d, Down) = %s, want %s", tc.in, tc.places, got, tc.want)
	}
}

func TestRound_Up(t *testing.T) {
	cases := []struct {
		in, want string
		places   int32
	}{
		{"5.01", "5.1", 1},
		{"-5.01", "-5.1", 1},
		{"5.00", "5.0", 1}, // exact — no change
	}
	for _, tc := range cases {
		d := decimal.RequireFromString(tc.in)
		got := Round(d, tc.places, RoundUp)
		assert.Truef(t, got.Equal(decimal.RequireFromString(tc.want)), "Round(%s, %d, Up) = %s, want %s", tc.in, tc.places, got, tc.want)
	}
}

// --- ConvertAt ---

func TestConvertAt_MatchesHandCalculation(t *testing.T) {
	cases := []struct {
		amount, rate, want string
		targetExponent     int32
		mode               RoundingMode
	}{
		// 100 USDT at 1 USDT = 7.23 CNY -> 723.00 CNY
		{"100", "7.23", "723", 2, RoundHalfUp},
		// 1 USD -> JPY at 149.371, exponent 0: 149.371 rounds half-up to 149
		{"1", "149.371", "149", 0, RoundHalfUp},
		// same but rate pushes to x.5 boundary
		{"2", "0.125", "0.25", 2, RoundHalfUp}, // 0.25 exact, no rounding needed
		{"1", "0.005", "0.01", 2, RoundHalfUp},
		{"1", "0.005", "0.00", 2, RoundDown},
	}
	for _, tc := range cases {
		amount := decimal.RequireFromString(tc.amount)
		rate := decimal.RequireFromString(tc.rate)
		got := ConvertAt(amount, rate, tc.targetExponent, tc.mode)
		want := decimal.RequireFromString(tc.want)
		assert.Truef(t, got.Equal(want), "ConvertAt(%s, %s, %d, %v) = %s, want %s", tc.amount, tc.rate, tc.targetExponent, tc.mode, got, tc.want)
	}
}

// --- Allocate ---

func TestAllocate_SumEqualsTotal_KnownCases(t *testing.T) {
	cases := []struct {
		total   string
		weights []string
		want    []string
		exp     int32
	}{
		{"100", []string{"1", "1", "1"}, []string{"33.34", "33.33", "33.33"}, 2},
		{"100", []string{"1", "1"}, []string{"50", "50"}, 2},
		{"0.03", []string{"1", "1"}, []string{"0.02", "0.01"}, 2}, // largest remainder gets the extra cent
		{"-100", []string{"1", "1", "1"}, []string{"-33.34", "-33.33", "-33.33"}, 2},
		{"10", []string{"0.3", "0.7"}, []string{"3", "7"}, 2},
		{"1", []string{"1", "1", "1"}, []string{"0.34", "0.33", "0.33"}, 2},
	}
	for _, tc := range cases {
		total := decimal.RequireFromString(tc.total)
		weights := make([]decimal.Decimal, len(tc.weights))
		for i, w := range tc.weights {
			weights[i] = decimal.RequireFromString(w)
		}
		got, err := Allocate(total, weights, tc.exp)
		require.NoError(t, err)
		require.Len(t, got, len(tc.want))

		sum := decimal.Zero
		for i, share := range got {
			sum = sum.Add(share)
			assert.Truef(t, share.Equal(share.Truncate(tc.exp)), "share[%d]=%s exceeds exponent %d", i, share, tc.exp)
		}
		assert.Truef(t, sum.Equal(total), "sum(shares)=%s != total=%s", sum, total)

		if tc.want != nil {
			for i, w := range tc.want {
				assert.Truef(t, got[i].Equal(decimal.RequireFromString(w)), "share[%d] = %s, want %s", i, got[i], w)
			}
		}
	}
}

func TestAllocate_RejectsNegativeWeight(t *testing.T) {
	_, err := Allocate(decimal.RequireFromString("100"), []decimal.Decimal{decimal.RequireFromString("-1"), decimal.RequireFromString("2")}, 2)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)
}

func TestAllocate_RejectsAllZeroWeights(t *testing.T) {
	_, err := Allocate(decimal.RequireFromString("100"), []decimal.Decimal{decimal.Zero, decimal.Zero}, 2)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)
}

func TestAllocate_RejectsEmptyWeights(t *testing.T) {
	_, err := Allocate(decimal.RequireFromString("100"), nil, 2)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)
}

func TestAllocate_RejectsOverPrecisionTotal(t *testing.T) {
	_, err := Allocate(decimal.RequireFromString("100.001"), []decimal.Decimal{decimal.RequireFromString("1"), decimal.RequireFromString("1")}, 2)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)
}

func TestAllocate_ZeroTotal(t *testing.T) {
	got, err := Allocate(decimal.Zero, []decimal.Decimal{decimal.RequireFromString("1"), decimal.RequireFromString("2")}, 2)
	require.NoError(t, err)
	for _, share := range got {
		assert.True(t, share.IsZero())
	}
}

func TestAllocate_SingleWeightGetsEverything(t *testing.T) {
	got, err := Allocate(decimal.RequireFromString("99.99"), []decimal.Decimal{decimal.RequireFromString("1")}, 2)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.True(t, got[0].Equal(decimal.RequireFromString("99.99")))
}

func TestAllocate_ExponentZero(t *testing.T) {
	// JPY-style: no fractional units allowed at all.
	got, err := Allocate(decimal.RequireFromString("10"), []decimal.Decimal{decimal.RequireFromString("1"), decimal.RequireFromString("1"), decimal.RequireFromString("1")}, 0)
	require.NoError(t, err)
	sum := decimal.Zero
	for _, share := range got {
		require.True(t, share.Equal(share.Truncate(0)))
		sum = sum.Add(share)
	}
	assert.True(t, sum.Equal(decimal.RequireFromString("10")))
}

// Property: Allocate(total, weights, exponent) always sums to exactly total
// and every share is representable at exponent decimal places, regardless of
// how total/weights/exponent are chosen (within valid input constraints).
// This is the invariant the largest-remainder algorithm exists to guarantee —
// run it for a few seconds as `go test -run FuzzAllocate -fuzz FuzzAllocate`.
func FuzzAllocate(f *testing.F) {
	f.Add(int64(10000), int64(2), int64(3), int64(3), int64(3), int32(2))
	f.Add(int64(1), int64(1), int64(1), int64(1), int64(1), int32(0))
	f.Add(int64(-99999), int64(1), int64(7), int64(0), int64(0), int32(2))
	f.Add(int64(0), int64(1), int64(1), int64(1), int64(1), int32(4))

	f.Fuzz(func(t *testing.T, totalUnits, w1, w2, w3, w4 int64, exp int32) {
		if exp < 0 || exp > 18 {
			t.Skip()
		}
		total := decimal.NewFromBigInt(big.NewInt(totalUnits), -exp)

		rawWeights := []int64{w1, w2, w3, w4}
		weights := make([]decimal.Decimal, 0, len(rawWeights))
		for _, w := range rawWeights {
			if w < 0 {
				w = -w
			}
			weights = append(weights, decimal.NewFromInt(w))
		}
		allZero := true
		for _, w := range weights {
			if !w.IsZero() {
				allZero = false
				break
			}
		}
		if allZero {
			t.Skip()
		}

		shares, err := Allocate(total, weights, exp)
		require.NoError(t, err)
		require.Len(t, shares, len(weights))

		sum := decimal.Zero
		for i, share := range shares {
			require.Truef(t, share.Equal(share.Truncate(exp)), "share[%d]=%s exceeds exponent %d", i, share, exp)
			sum = sum.Add(share)
		}
		require.Truef(t, sum.Equal(total), "sum(shares)=%s != total=%s (weights=%v, exp=%d)", sum, total, weights, exp)
	})
}

// Property (non-fuzz, seeded random): same invariant as FuzzAllocate but
// exercised with a broader random distribution including larger weight
// counts, run at normal `go test` speed (no -fuzz flag required).
func TestAllocateInvariant_SumAlwaysEqualsTotal(t *testing.T) {
	rng := rand.New(rand.NewSource(0x5EED))

	for trial := 0; trial < 500; trial++ {
		exp := int32(rng.Intn(7)) // 0..6
		totalUnits := rng.Int63n(1_000_000_000) - 500_000_000
		total := decimal.NewFromBigInt(big.NewInt(totalUnits), -exp)

		n := 1 + rng.Intn(12)
		weights := make([]decimal.Decimal, n)
		allZero := true
		for i := range weights {
			w := rng.Int63n(1000)
			weights[i] = decimal.NewFromInt(w)
			if w != 0 {
				allZero = false
			}
		}
		if allZero {
			weights[0] = decimal.NewFromInt(1)
		}

		shares, err := Allocate(total, weights, exp)
		require.NoError(t, err)

		sum := decimal.Zero
		for i, share := range shares {
			require.Truef(t, share.Equal(share.Truncate(exp)), "trial %d: share[%d]=%s exceeds exponent %d", trial, i, share, exp)
			sum = sum.Add(share)
		}
		require.Truef(t, sum.Equal(total), "trial %d: sum(shares)=%s != total=%s", trial, sum, total)
	}
}

func TestErrPrecisionExceededIsDistinctSentinel(t *testing.T) {
	assert.False(t, errors.Is(ErrPrecisionExceeded, ErrInvalidInput))
}
