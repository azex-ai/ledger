package core

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDecodeConversionQuotes_DecimalsMustBeStrings pins I-a (2026-10-09
// security review): the string spelling EncodeConversionQuotes writes is the
// only accepted one. A bare JSON number used to decode (shopspring parses the
// raw token), so one quote had two spellings that compare unequal in the
// idempotency payload check.
func TestDecodeConversionQuotes_DecimalsMustBeStrings(t *testing.T) {
	fields := map[string]string{
		"source_quantity": `"1"`,
		"rate":            `"1000"`,
		"target_amount":   `"1000"`,
	}
	build := func(override map[string]string) string {
		parts := []string{`"source_code":"USDC"`, `"target_code":"CREDITS"`, `"source_exponent":6`,
			`"target_exponent":6`, `"version":"v1"`, `"rounding":"half_up"`}
		for _, name := range conversionQuoteDecimalFields {
			v, ok := override[name]
			if !ok {
				v = fields[name]
			}
			if v == "<absent>" {
				continue
			}
			parts = append(parts, `"`+name+`":`+v)
		}
		return `[{` + strings.Join(parts, ",") + `}]`
	}

	quotes, err := DecodeConversionQuotes(build(nil))
	require.NoError(t, err, "the string spelling decodes")
	require.Equal(t, "1000", quotes[0].Rate.String())

	for _, name := range conversionQuoteDecimalFields {
		for spelling, v := range map[string]string{
			"number":          `1000`,
			"fraction number": `1000.5`,
			"exponent number": `1e3`,
			"null":            `null`,
			"absent":          `<absent>`,
			"object":          `{}`,
			"array":           `["1000"]`,
		} {
			t.Run(name+"/"+spelling, func(t *testing.T) {
				_, err := DecodeConversionQuotes(build(map[string]string{name: v}))
				require.ErrorIs(t, err, ErrInvalidInput)
			})
		}
	}

	// A second quote in the list is checked as well, not only the first.
	two := strings.TrimSuffix(build(nil), "]") + "," + strings.TrimPrefix(build(map[string]string{"rate": "1000"}), "[")
	_, err = DecodeConversionQuotes(two)
	require.ErrorIs(t, err, ErrInvalidInput)

	// Whatever Encode writes, Decode reads back.
	encoded, err := EncodeConversionQuotes(quotes)
	require.NoError(t, err)
	_, err = DecodeConversionQuotes(encoded)
	require.NoError(t, err)
}
