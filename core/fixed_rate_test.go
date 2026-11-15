package core

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fixedRateFixture() (FixedRate, Currency, Currency) {
	return FixedRate{
		SourceCode: "USDC", TargetCode: "CREDITS", Rate: decimal.NewFromInt(1000),
		Version: "price-v1", Rounding: RoundHalfUp,
	}, Currency{Code: "USDC", Exponent: 6}, Currency{Code: "CREDITS", Exponent: 6}
}

func TestFixedRate_ConvertsConfiguredUnits(t *testing.T) {
	cases := []struct {
		name, sourceCode, quantity, rate, want string
		sourceExponent, targetExponent         int32
	}{
		{"USDC", "USDC", "1", "1000", "1000", 6, 6},
		{"changed configuration", "USDC", "2", "750", "1500", 6, 6},
		{"input tokens", "INPUT_TOKEN", "16000", "0.001", "16", 0, 6},
		{"output tokens", "OUTPUT_TOKEN", "1250", "0.0129", "16.125", 0, 6},
		{"gifts", "GIFT", "3", "25", "75", 0, 6},
		{"whole gift with trailing zeros", "GIFT", "1.000", "25", "25", 0, 6},
		{"zero quantity", "INPUT_TOKEN", "0", "0.001", "0", 0, 6},
		{"rate finer than currency precision", "INPUT_TOKEN", "1000000", "0.000000000001", "0.000001", 0, 6},
		{"rounded zero", "INPUT_TOKEN", "1", "0.0000004", "0", 0, 6},
		{"maximum stored amount", "USDC", "999999999999.999999999999999999", "1", "999999999999.999999999999999999", 18, 18},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rate := FixedRate{
				SourceCode: tc.sourceCode, TargetCode: "CREDITS",
				Rate: decimal.RequireFromString(tc.rate), Version: "configured-v2", Rounding: RoundHalfUp,
			}
			// Measured units need no persisted UID, display name, or active flag.
			source := Currency{Code: tc.sourceCode, Exponent: tc.sourceExponent}
			target := Currency{Code: "CREDITS", Exponent: tc.targetExponent}
			require.NoError(t, rate.Validate(source, target))
			got, err := rate.Convert(decimal.RequireFromString(tc.quantity), source, target)
			require.NoError(t, err)
			assert.True(t, got.Equal(decimal.RequireFromString(tc.want)), "got %s, want %s", got, tc.want)
		})
	}
}

func TestFixedRate_UsesTargetRounding(t *testing.T) {
	cases := []struct {
		name, rate, want string
		mode             RoundingMode
	}{
		{"half up tie", "0.005", "0.01", RoundHalfUp},
		{"half even lower tie", "0.005", "0", RoundHalfEven},
		{"half even upper tie", "0.015", "0.02", RoundHalfEven},
		{"down", "0.019", "0.01", RoundDown},
		{"up", "0.011", "0.02", RoundUp},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rate, source, target := fixedRateFixture()
			rate.Rate, rate.Rounding = decimal.RequireFromString(tc.rate), tc.mode
			target.Exponent = 2
			got, err := rate.Convert(decimal.NewFromInt(1), source, target)
			require.NoError(t, err)
			assert.True(t, got.Equal(decimal.RequireFromString(tc.want)), "got %s, want %s", got, tc.want)
		})
	}
}

func TestFixedRate_RejectsInvalidConfiguration(t *testing.T) {
	cases := []struct {
		name   string
		change func(*FixedRate, *Currency, *Currency)
	}{
		{"empty source", func(r *FixedRate, _, _ *Currency) { r.SourceCode = "" }},
		{"blank source", func(r *FixedRate, _, _ *Currency) { r.SourceCode = " " }},
		{"empty target", func(r *FixedRate, _, _ *Currency) { r.TargetCode = "" }},
		{"blank target", func(r *FixedRate, _, _ *Currency) { r.TargetCode = "\t" }},
		{"same units", func(r *FixedRate, s, target *Currency) { r.TargetCode, target.Code = r.SourceCode, s.Code }},
		{"source mismatch", func(_ *FixedRate, s, _ *Currency) { s.Code = "INPUT_TOKEN" }},
		{"target mismatch", func(_ *FixedRate, _, target *Currency) { target.Code = "GIFT" }},
		{"reversed pair", func(_ *FixedRate, s, target *Currency) { *s, *target = *target, *s }},
		{"empty version", func(r *FixedRate, _, _ *Currency) { r.Version = "" }},
		{"blank version", func(r *FixedRate, _, _ *Currency) { r.Version = "  " }},
		{"zero rate", func(r *FixedRate, _, _ *Currency) { r.Rate = decimal.Zero }},
		{"negative rate", func(r *FixedRate, _, _ *Currency) { r.Rate = decimal.NewFromInt(-1) }},
		{"large rate exponent", func(r *FixedRate, _, _ *Currency) { r.Rate = decimal.New(1, math.MaxInt32) }},
		{"small rate exponent", func(r *FixedRate, _, _ *Currency) { r.Rate = decimal.New(1, math.MinInt32) }},
		{"rate magnitude", func(r *FixedRate, _, _ *Currency) { r.Rate = decimal.RequireFromString("1000000000000") }},
		{"rate working precision", func(r *FixedRate, _, _ *Currency) { r.Rate = decimal.New(1, -37) }},
		{"source negative exponent", func(_ *FixedRate, s, _ *Currency) { s.Exponent = -1 }},
		{"source excessive exponent", func(_ *FixedRate, s, _ *Currency) { s.Exponent = math.MaxInt32 }},
		{"target negative exponent", func(_ *FixedRate, _, target *Currency) { target.Exponent = math.MinInt32 }},
		{"target excessive exponent", func(_ *FixedRate, _, target *Currency) { target.Exponent = 19 }},
		{"negative rounding mode", func(r *FixedRate, _, _ *Currency) { r.Rounding = -1 }},
		{"unknown rounding mode", func(r *FixedRate, _, _ *Currency) { r.Rounding = RoundUp + 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rate, source, target := fixedRateFixture()
			tc.change(&rate, &source, &target)
			require.ErrorIs(t, rate.Validate(source, target), ErrInvalidInput)
			got, err := rate.Convert(decimal.NewFromInt(1), source, target)
			require.ErrorIs(t, err, ErrInvalidInput)
			assert.True(t, got.IsZero(), "failed conversion must not return an amount")
		})
	}
}

func TestFixedRate_RejectsInvalidQuantitiesAndResults(t *testing.T) {
	cases := []struct {
		name           string
		quantity, rate decimal.Decimal
		sourceExponent int32
		targetExponent int32
		wantErr        error
	}{
		{"negative quantity", decimal.NewFromInt(-1), decimal.NewFromInt(1), 6, 6, ErrInvalidInput},
		{"fractional indivisible unit", decimal.RequireFromString("1.5"), decimal.NewFromInt(1), 0, 6, ErrPrecisionExceeded},
		{"source exceeds currency precision", decimal.RequireFromString("1.0000001"), decimal.NewFromInt(1), 6, 6, ErrPrecisionExceeded},
		{"large quantity exponent", decimal.New(1, math.MaxInt32), decimal.NewFromInt(1), 6, 6, ErrInvalidInput},
		{"small quantity exponent", decimal.New(1, math.MinInt32), decimal.NewFromInt(1), 6, 6, ErrInvalidInput},
		{"quantity storage precision", decimal.New(1, -19), decimal.NewFromInt(1), 18, 6, ErrInvalidInput},
		{"quantity storage magnitude", decimal.RequireFromString("1000000000000"), decimal.NewFromInt(1), 0, 6, ErrInvalidInput},
		{"product overflow", decimal.RequireFromString("100000000000"), decimal.NewFromInt(10), 0, 6, ErrInvalidInput},
		{"rounding carry exceeds storage", decimal.RequireFromString("999999999999.9"), decimal.NewFromInt(1), 1, 0, ErrInvalidInput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rate, source, target := fixedRateFixture()
			rate.Rate = tc.rate
			source.Exponent, target.Exponent = tc.sourceExponent, tc.targetExponent
			got, err := rate.Convert(tc.quantity, source, target)
			require.ErrorIs(t, err, tc.wantErr)
			assert.True(t, got.IsZero(), "failed conversion must not return an amount")
		})
	}
}

func TestFixedRate_PreservesQuoteWhenRoundedAmountsMatch(t *testing.T) {
	first, source, target := fixedRateFixture()
	first.Rate = decimal.RequireFromString("1.001")
	second := first
	second.Rate, second.Version = decimal.RequireFromString("1.002"), "price-v2"
	target.Exponent = 2
	a, err := first.Convert(decimal.NewFromInt(1), source, target)
	require.NoError(t, err)
	b, err := second.Convert(decimal.NewFromInt(1), source, target)
	require.NoError(t, err)
	assert.True(t, a.Equal(b), "different quotes can round to the same amount")
	firstJSON, err := json.Marshal(first)
	require.NoError(t, err)
	secondJSON, err := json.Marshal(second)
	require.NoError(t, err)
	assert.NotEqual(t, string(firstJSON), string(secondJSON), "persist the quote, not only its rounded amount")
	// rounding travels by name (api-contract.md §7), never as the enum's
	// integer position: a snapshot that said "0" would change meaning the day
	// someone reorders the constants.
	assert.JSONEq(t, `{"source_code":"USDC","target_code":"CREDITS","rate":"1.001","version":"price-v1","rounding":"half_up"}`, string(firstJSON))

	var restored FixedRate
	require.NoError(t, json.Unmarshal(firstJSON, &restored))
	require.NoError(t, restored.Validate(source, target))
	got, err := restored.Convert(decimal.NewFromInt(1), source, target)
	require.NoError(t, err)
	assert.True(t, got.Equal(a))
}

func TestFixedRate_AllowsExistingWorkingRatePrecision(t *testing.T) {
	rate, source, target := fixedRateFixture()
	rate.Rate = decimal.New(1, -MaxAmountWorkingFractionalDigits)
	require.NoError(t, rate.Validate(source, target))
	got, err := rate.Convert(decimal.NewFromInt(1), source, target)
	require.NoError(t, err)
	assert.True(t, got.IsZero())
}

func TestFixedRate_ZeroQuantityDoesNotExhaustWorkingPrecision(t *testing.T) {
	rate, source, target := fixedRateFixture()
	rate.Rate = decimal.New(1, -MaxAmountWorkingFractionalDigits)
	got, err := rate.Convert(decimal.New(0, -MaxAmountFractionalDigits), source, target)
	require.NoError(t, err)
	assert.True(t, got.IsZero())
}
