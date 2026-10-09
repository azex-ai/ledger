package core

import (
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// TestQuoteVersion_RejectsInvalidUTF8 pins the 2026-10-09 second-opinion
// finding folded into m-2: encoding/json writes every invalid UTF-8 byte as
// U+FFFD, so two different invalid versions produced the same
// conversion_quotes payload and a changed-version retry replayed instead of
// raising ErrConflict (I-3). Every gate a version passes through on its way
// into a journal must refuse it: FixedRate.Validate (Convert/Quote, which
// Exchange calls first), ConversionQuote.Validate, and therefore
// EncodeConversionQuotes / DecodeConversionQuotes.
func TestQuoteVersion_RejectsInvalidUTF8(t *testing.T) {
	ff := "v" + string([]byte{0xff})
	fe := "v" + string([]byte{0xfe})

	// The collapse this guards against is real: two different inputs, one
	// encoding.
	a, err := json.Marshal(ff)
	require.NoError(t, err)
	b, err := json.Marshal(fe)
	require.NoError(t, err)
	require.Equal(t, string(a), string(b), "precondition: encoding/json maps distinct invalid bytes to the same U+FFFD")

	usdc := Currency{Code: "USDC", Exponent: 6}
	credits := Currency{Code: "CREDITS", Exponent: 6}
	rate := FixedRate{SourceCode: "USDC", TargetCode: "CREDITS", Rate: decimal.NewFromInt(1000), Version: "v1", Rounding: RoundHalfUp}
	require.NoError(t, rate.Validate(usdc, credits), "control: a valid version passes")

	for _, version := range []string{ff, fe, "price-" + string([]byte{0xc3})} {
		bad := rate
		bad.Version = version

		require.ErrorIs(t, bad.Validate(usdc, credits), ErrInvalidInput, "FixedRate.Validate")
		_, err := bad.Quote(decimal.NewFromInt(1), usdc, credits)
		require.ErrorIs(t, err, ErrInvalidInput, "FixedRate.Quote -- Exchange's first step")

		good, err := rate.Quote(decimal.NewFromInt(1), usdc, credits)
		require.NoError(t, err)
		q := good
		q.Version = version
		require.ErrorIs(t, q.Validate(), ErrInvalidInput, "ConversionQuote.Validate")
		_, err = EncodeConversionQuotes([]ConversionQuote{q})
		require.ErrorIs(t, err, ErrInvalidInput, "EncodeConversionQuotes")
	}

	// A valid non-ASCII version is still a version: only broken encodings
	// are refused, not Unicode.
	ok := rate
	ok.Version = "pre" + string(rune(0xe7)) + "o-v1"
	require.NoError(t, ok.Validate(usdc, credits))
}
