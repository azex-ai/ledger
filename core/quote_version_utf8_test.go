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

	// The collision itself is gone: the two invalid spellings no longer reach
	// an encoding at all, so they cannot share one payload.
	quote, err := rate.Quote(decimal.NewFromInt(1), usdc, credits)
	require.NoError(t, err)
	payloads := map[string]bool{}
	for _, version := range []string{ff, fe} {
		q := quote
		q.Version = version
		encoded, err := EncodeConversionQuotes([]ConversionQuote{q})
		require.ErrorIs(t, err, ErrInvalidInput)
		require.Empty(t, encoded)
		if encoded != "" {
			payloads[encoded] = true
		}
	}
	require.Empty(t, payloads, "no invalid-UTF-8 version produced a payload")

	// Control characters are refused too (C0, DEL, C1).
	for name, version := range map[string]string{
		"nul":     "v1" + string(rune(0x00)),
		"newline": "v1" + string(rune(0x0a)),
		"del":     "v1" + string(rune(0x7f)),
		"c1 nel":  "v1" + string(rune(0x85)),
	} {
		t.Run("control/"+name, func(t *testing.T) {
			bad := rate
			bad.Version = version
			require.ErrorIs(t, bad.Validate(usdc, credits), ErrInvalidInput)
			q := quote
			q.Version = version
			require.ErrorIs(t, q.Validate(), ErrInvalidInput)
		})
	}

	// A valid non-ASCII version is still a version: only broken encodings
	// and control characters are refused, not Unicode.
	ok := rate
	ok.Version = "pre" + string(rune(0xe7)) + "o-v1"
	require.NoError(t, ok.Validate(usdc, credits))
}
