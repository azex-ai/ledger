package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestRoundingMode_TextRoundTrip(t *testing.T) {
	for _, mode := range []RoundingMode{RoundHalfUp, RoundHalfEven, RoundDown, RoundUp} {
		parsed, err := ParseRoundingMode(mode.String())
		require.NoError(t, err)
		require.Equal(t, mode, parsed)

		encoded, err := json.Marshal(mode)
		require.NoError(t, err)
		require.Equal(t, `"`+mode.String()+`"`, string(encoded))
		var decoded RoundingMode
		require.NoError(t, json.Unmarshal(encoded, &decoded))
		require.Equal(t, mode, decoded)
	}
	require.Equal(t, "half_up", RoundHalfUp.String())
	require.Equal(t, "half_even", RoundHalfEven.String())
	require.Equal(t, "down", RoundDown.String())
	require.Equal(t, "up", RoundUp.String())

	_, err := ParseRoundingMode("bankers")
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = json.Marshal(RoundingMode(99))
	require.Error(t, err)
	var bad RoundingMode
	require.ErrorIs(t, json.Unmarshal([]byte(`"nearest"`), &bad), ErrInvalidInput)
	// The pre-existing integer spelling is not accepted: a snapshot is a
	// contract, and two encodings of one field is how contracts drift.
	require.Error(t, json.Unmarshal([]byte(`0`), &bad))
}

func TestFixedRate_Quote_SnapshotsEveryInput(t *testing.T) {
	input := Currency{Code: "INPUT_TOKEN", Exponent: 0}
	credits := Currency{Code: "CREDITS", Exponent: 6}
	rate := FixedRate{SourceCode: "INPUT_TOKEN", TargetCode: "CREDITS",
		Rate: decimal.RequireFromString("0.002"), Version: "price-v1", Rounding: RoundHalfUp}

	quote, err := rate.Quote(decimal.NewFromInt(10000), input, credits)
	require.NoError(t, err)
	require.Equal(t, "INPUT_TOKEN", quote.SourceCode)
	require.Equal(t, "CREDITS", quote.TargetCode)
	require.Equal(t, int32(0), quote.SourceExponent)
	require.Equal(t, int32(6), quote.TargetExponent)
	require.True(t, quote.SourceQuantity.Equal(decimal.NewFromInt(10000)))
	require.True(t, quote.TargetAmount.Equal(decimal.NewFromInt(20)))
	require.True(t, quote.Rate.Equal(rate.Rate))
	require.Equal(t, "price-v1", quote.Version)
	require.Equal(t, RoundHalfUp, quote.Rounding)
	require.NoError(t, quote.Validate())

	// Quote validates exactly as Convert does: a wrong pair is refused
	// before any arithmetic.
	_, err = rate.Quote(decimal.NewFromInt(1), credits, input)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = rate.Quote(decimal.RequireFromString("1.5"), input, credits)
	require.ErrorIs(t, err, ErrPrecisionExceeded)
}

func TestConversionQuotes_EncodeDecode(t *testing.T) {
	quotes := []ConversionQuote{
		{SourceCode: "INPUT_TOKEN", TargetCode: "CREDITS", TargetExponent: 6,
			SourceQuantity: decimal.NewFromInt(10000), TargetAmount: decimal.NewFromInt(20),
			Rate: decimal.RequireFromString("0.002"), Version: "price-v1", Rounding: RoundHalfUp},
		{SourceCode: "OUTPUT_TOKEN", TargetCode: "CREDITS", TargetExponent: 6,
			SourceQuantity: decimal.NewFromInt(2425), TargetAmount: decimal.RequireFromString("12.125"),
			Rate: decimal.RequireFromString("0.005"), Version: "price-v1", Rounding: RoundHalfUp},
	}
	encoded, err := EncodeConversionQuotes(quotes)
	require.NoError(t, err)
	// Decimals travel as strings and the rounding mode by name: the same
	// wire discipline as every other amount (api-contract.md §4, §7).
	require.Contains(t, encoded, `"source_quantity":"10000"`)
	require.Contains(t, encoded, `"rate":"0.002"`)
	require.Contains(t, encoded, `"rounding":"half_up"`)
	require.False(t, strings.ContainsAny(encoded, " \n"), "compact encoding, it lives in a bounded metadata value")

	decoded, err := DecodeConversionQuotes(encoded)
	require.NoError(t, err)
	require.Len(t, decoded, 2)
	for i := range quotes {
		require.Equal(t, quotes[i].SourceCode, decoded[i].SourceCode)
		require.True(t, quotes[i].SourceQuantity.Equal(decoded[i].SourceQuantity))
		require.True(t, quotes[i].TargetAmount.Equal(decoded[i].TargetAmount))
		require.True(t, quotes[i].Rate.Equal(decoded[i].Rate))
		require.Equal(t, quotes[i].Version, decoded[i].Version)
		require.Equal(t, quotes[i].Rounding, decoded[i].Rounding)
	}

	// Metadata is map[string]string; the encoding has to fit one value.
	metadata := map[string]string{ConversionQuotesMetadataKey: encoded}
	require.NoError(t, validateFreeformFields("test", "", metadata))

	_, err = EncodeConversionQuotes(nil)
	require.ErrorIs(t, err, ErrInvalidInput, "an empty quote list must not be written: absence is the empty metadata key")
	_, err = DecodeConversionQuotes("")
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = DecodeConversionQuotes("[]")
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = DecodeConversionQuotes(`[{"source_code":"A","target_code":"B","rate":"-1"}]`)
	require.ErrorIs(t, err, ErrInvalidInput, "decode validates each quote, it does not only parse")
	_, err = DecodeConversionQuotes(`[{"source_code":"A","target_code":"B","rate":"1","rounding":"half_up","source_quantity":"1","target_amount":"1","unknown":1}]`)
	require.ErrorIs(t, err, ErrInvalidInput, "unknown fields are a different snapshot shape, not forward compatibility")
}

func TestConversionQuote_Validate(t *testing.T) {
	good := ConversionQuote{SourceCode: "USDC", TargetCode: "CREDITS", TargetExponent: 6,
		SourceQuantity: decimal.NewFromInt(1), TargetAmount: decimal.NewFromInt(1000),
		Rate: decimal.NewFromInt(1000), Version: "v1", Rounding: RoundHalfUp}
	require.NoError(t, good.Validate())

	for name, mutate := range map[string]func(*ConversionQuote){
		"same codes":        func(q *ConversionQuote) { q.TargetCode = q.SourceCode },
		"empty source":      func(q *ConversionQuote) { q.SourceCode = "" },
		"zero rate":         func(q *ConversionQuote) { q.Rate = decimal.Zero },
		"negative quantity": func(q *ConversionQuote) { q.SourceQuantity = decimal.NewFromInt(-1) },
		"negative amount":   func(q *ConversionQuote) { q.TargetAmount = decimal.NewFromInt(-1) },
		"empty version":     func(q *ConversionQuote) { q.Version = "" },
		"bad rounding":      func(q *ConversionQuote) { q.Rounding = RoundingMode(7) },
		"bad exponent":      func(q *ConversionQuote) { q.TargetExponent = MaxAmountFractionalDigits + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			q := good
			mutate(&q)
			require.ErrorIs(t, q.Validate(), ErrInvalidInput)
		})
	}
}

func TestJournalInput_Validate_RejectsMalformedConversionQuotes(t *testing.T) {
	valid := JournalInput{
		IdempotencyKey: "k", JournalTypeUID: "jt",
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyUID: "c", ClassificationUID: "a", EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(1)},
			{AccountHolder: -1, CurrencyUID: "c", ClassificationUID: "b", EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(1)},
		},
	}
	require.NoError(t, valid.Validate())

	encoded, err := EncodeConversionQuotes([]ConversionQuote{{
		SourceCode: "USDC", TargetCode: "CREDITS", TargetExponent: 6,
		SourceQuantity: decimal.NewFromInt(1), TargetAmount: decimal.NewFromInt(1000),
		Rate: decimal.NewFromInt(1000), Version: "v1", Rounding: RoundHalfUp,
	}})
	require.NoError(t, err)
	withQuotes := valid
	withQuotes.Metadata = map[string]string{ConversionQuotesMetadataKey: encoded}
	require.NoError(t, withQuotes.Validate())

	// A complete quote array must be the only JSON value, including when
	// the next byte is a closing delimiter rather than another value.
	for _, tc := range []struct {
		name   string
		suffix string
		valid  bool
	}{
		{name: "trailing whitespace", suffix: " \t\r\n", valid: true},
		{name: "closing object", suffix: "}"},
		{name: "closing array", suffix: "]"},
		{name: "closing delimiter and garbage", suffix: "}garbage"},
		{name: "second object", suffix: " {}"},
		{name: "second array", suffix: " []"},
		{name: "second null", suffix: " null"},
		{name: "trailing garbage", suffix: " garbage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := encoded + tc.suffix
			quotes, decodeErr := DecodeConversionQuotes(payload)
			journal := valid
			journal.Metadata = map[string]string{ConversionQuotesMetadataKey: payload}
			if tc.valid {
				require.NoError(t, decodeErr)
				require.Len(t, quotes, 1)
				require.NoError(t, journal.Validate())
				return
			}
			require.ErrorIs(t, decodeErr, ErrInvalidInput)
			require.Nil(t, quotes)
			require.ErrorIs(t, journal.Validate(), ErrInvalidInput)
		})
	}

	malformed := valid
	malformed.Metadata = map[string]string{ConversionQuotesMetadataKey: `{"rate":"1000"}`}
	err = malformed.Validate()
	require.ErrorIs(t, err, ErrInvalidInput)
	require.Contains(t, err.Error(), ConversionQuotesMetadataKey)

	// Other metadata keys stay free-form; only the well-known key is typed.
	other := valid
	other.Metadata = map[string]string{"memo": `{"not":"json"`}
	require.NoError(t, other.Validate())
}

// staticRates is the smallest possible RateQuoter: what a host adapter over a
// JSON file, a database table or a price oracle all reduce to.
type staticRates map[[2]string]FixedRate

func (r staticRates) QuoteRate(_ context.Context, sourceCode, targetCode string) (FixedRate, error) {
	rate, ok := r[[2]string{sourceCode, targetCode}]
	if !ok {
		return FixedRate{}, ErrNotFound
	}
	return rate, nil
}

func TestRateQuoter_PortIsOneMethodOverFixedRate(t *testing.T) {
	var quoter RateQuoter = staticRates{
		{"USDC", "CREDITS"}: {SourceCode: "USDC", TargetCode: "CREDITS",
			Rate: decimal.NewFromInt(1000), Version: "v1", Rounding: RoundHalfUp},
	}
	rate, err := quoter.QuoteRate(context.Background(), "USDC", "CREDITS")
	require.NoError(t, err)
	quote, err := rate.Quote(decimal.NewFromInt(2), Currency{Code: "USDC", Exponent: 6}, Currency{Code: "CREDITS", Exponent: 6})
	require.NoError(t, err)
	require.True(t, quote.TargetAmount.Equal(decimal.NewFromInt(2000)))
	_, err = quoter.QuoteRate(context.Background(), "CREDITS", "USDC")
	require.ErrorIs(t, err, ErrNotFound, "direction is explicit; the reverse pair is its own configuration")
}

// TestConversionQuote_Validate_TargetAmountFollowsTheRate pins m-5's second
// half (2026-10-09 security review): a quote's target_amount must be what its
// own quantity, rate, rounding and target exponent produce. The review's
// "lying-amount" probe (1 x 1000 -> 999999) used to decode and validate, so a
// holder statement would have explained a charge with numbers that do not
// follow from its rate.
func TestConversionQuote_Validate_TargetAmountFollowsTheRate(t *testing.T) {
	good := ConversionQuote{SourceCode: "USDC", TargetCode: "CREDITS", SourceExponent: 6, TargetExponent: 6,
		SourceQuantity: decimal.NewFromInt(1), TargetAmount: decimal.NewFromInt(1000),
		Rate: decimal.NewFromInt(1000), Version: "v1", Rounding: RoundHalfUp}
	require.NoError(t, good.Validate())

	// A quote FixedRate.Quote produced is consistent by construction, including
	// one where rounding actually happened.
	rate := FixedRate{SourceCode: "OUTPUT_TOKEN", TargetCode: "CREDITS",
		Rate: decimal.RequireFromString("0.0000005"), Version: "v1", Rounding: RoundUp}
	rounded, err := rate.Quote(decimal.NewFromInt(3), Currency{Code: "OUTPUT_TOKEN"}, Currency{Code: "CREDITS", Exponent: 6})
	require.NoError(t, err)
	require.Equal(t, "0.000002", rounded.TargetAmount.String())
	require.NoError(t, rounded.Validate())

	lying := good
	lying.TargetAmount = decimal.NewFromInt(999999)
	err = lying.Validate()
	require.ErrorIs(t, err, ErrInvalidInput)
	require.ErrorContains(t, err, "target_amount 999999")

	// The probe at journal write time: JournalInput.Validate runs the same
	// check through validateConversionQuotesMetadata.
	journal := JournalInput{
		IdempotencyKey: "k", JournalTypeUID: "jt",
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyUID: "c", ClassificationUID: "a", EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(1)},
			{AccountHolder: -1, CurrencyUID: "c", ClassificationUID: "b", EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(1)},
		},
		Metadata: map[string]string{ConversionQuotesMetadataKey: `[{"source_code":"USDC","target_code":"CREDITS",` +
			`"source_exponent":6,"target_exponent":6,"source_quantity":"1","target_amount":"999999",` +
			`"rate":"1000","version":"v1","rounding":"half_up"}]`},
	}
	require.ErrorIs(t, journal.Validate(), ErrInvalidInput)

	// The amount is right for exponent 6 but the quote claims exponent 2:
	// 0.0021 x 1 rounds to 0.00 there, not 0.0021.
	exponent := ConversionQuote{SourceCode: "A", TargetCode: "B", TargetExponent: 2,
		SourceQuantity: decimal.NewFromInt(1), TargetAmount: decimal.RequireFromString("0.0021"),
		Rate: decimal.RequireFromString("0.0021"), Version: "v1", Rounding: RoundHalfUp}
	require.ErrorIs(t, exponent.Validate(), ErrInvalidInput)
	exponent.TargetExponent = 6
	require.NoError(t, exponent.Validate())

	// A different rounding mode than the one that produced the amount.
	mode := rounded
	mode.Rounding = RoundDown
	require.ErrorIs(t, mode.Validate(), ErrInvalidInput)

	// A source quantity finer than its declared exponent is not a quantity
	// FixedRate.Convert would have accepted.
	fine := good
	fine.SourceExponent = 0
	fine.SourceQuantity = decimal.RequireFromString("1.5")
	fine.TargetAmount = decimal.NewFromInt(1500)
	err = fine.Validate()
	require.ErrorIs(t, err, ErrInvalidInput)
	require.ErrorIs(t, err, ErrPrecisionExceeded)
}
