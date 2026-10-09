package core

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// r spells one code point; the cases below are invisible in source otherwise.
func r(cp rune) string { return string(cp) }

// currencyCodeCases is the one table both validators are checked against:
// CurrencyInput.Validate (where a code is created) and
// ConversionQuote.Validate (where it is replayed on a holder statement).
var currencyCodeCases = []struct {
	name  string
	code  string
	valid bool
}{
	// Spellings already in use across fixtures, presets and examples.
	{"upper", "USDT", true},
	{"underscore", "INPUT_TOKEN", true},
	{"hyphen", "USDT-TRACE", true},
	{"mixed case", "USDT-late", true},
	{"digits", "USDC-6", true},
	{"unique key shape", "SWEEP-RETRY-USDT-42", true},
	{"bridged-token dot", "USDC.e", true},
	{"dot mid", "BTC.b-2", true},
	{"max length", strings.Repeat("A", maxCurrencyCodeLen), true},

	{"empty", "", false},
	{"too long", strings.Repeat("A", maxCurrencyCodeLen+1), false},
	{"rtl override", r(0x202e) + "CDSU", false},
	{"rtl override mid", "US" + r(0x202e) + "DC", false},
	{"zero-width joiner", "USD" + r(0x200d) + "C", false},
	{"zero-width space", "USDC" + r(0x200b), false},
	{"byte order mark", r(0xfeff) + "USDC", false},
	{"space", "US DC", false},
	{"leading space", " USDC", false},
	{"tab", "USDC" + r(0x09), false},
	{"newline", "USDC" + r(0x0a), false},
	{"nul", "USDC" + r(0x00), false},
	{"del", "USDC" + r(0x7f), false},
	{"markup", "USDC<img>", false},
	{"non-ascii letter", "USD" + r(0x00c7), false},
	{"fullwidth", r(0xff35) + r(0xff33) + r(0xff24) + r(0xff23), false},
}

func TestCurrencyInput_Validate_CodeCharset(t *testing.T) {
	for _, tc := range currencyCodeCases {
		t.Run(tc.name, func(t *testing.T) {
			err := CurrencyInput{Code: tc.code, Name: "x", Exponent: 6}.Validate()
			if tc.valid {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrInvalidInput)
		})
	}
}

func TestConversionQuote_Validate_CodeCharset(t *testing.T) {
	base := ConversionQuote{SourceCode: "USDC", TargetCode: "CREDITS", TargetExponent: 6,
		SourceQuantity: decimal.NewFromInt(1), TargetAmount: decimal.NewFromInt(1000),
		Rate: decimal.NewFromInt(1000), Version: "v1", Rounding: RoundHalfUp}
	for _, tc := range currencyCodeCases {
		if tc.code == base.TargetCode || tc.code == base.SourceCode {
			continue
		}
		for _, side := range []string{"source", "target"} {
			t.Run(side+"/"+tc.name, func(t *testing.T) {
				q := base
				if side == "source" {
					q.SourceCode = tc.code
				} else {
					q.TargetCode = tc.code
				}
				err := q.Validate()
				if tc.valid {
					require.NoError(t, err)
					return
				}
				require.ErrorIs(t, err, ErrInvalidInput)
			})
		}
	}
}

// TestDecodeConversionQuotes_RefusesUnprintableCodes is the review's probe
// (2026-10-09 m-2) turned into a pin: a right-to-left override plus markup,
// and a zero-width joiner, used to decode with err == nil. They travel as
// JSON escapes, the way a hand-built metadata value would carry them.
func TestDecodeConversionQuotes_RefusesUnprintableCodes(t *testing.T) {
	const tail = `","target_code":"CREDITS","source_exponent":6,"target_exponent":6,` +
		`"source_quantity":"1","target_amount":"1000","rate":"1000","version":"v1","rounding":"half_up"}]`
	for name, code := range map[string]string{
		"rtl override + markup": `\u202eCDSU<img src=x onerror=alert(1)>`,
		"zero-width joiner":     `USD\u200dC`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeConversionQuotes(`[{"source_code":"` + code + tail)
			require.ErrorIs(t, err, ErrInvalidInput)
		})
	}
	_, err := DecodeConversionQuotes(`[{"source_code":"USDC` + tail)
	require.NoError(t, err, "the control case decodes")
}
