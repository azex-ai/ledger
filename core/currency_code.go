package core

import "fmt"

// maxCurrencyCodeLen bounds a currency code. The longest code anywhere in this
// repository's fixtures, presets and examples is under 32 characters; 64 is a
// bound against pathology, not a business rule.
const maxCurrencyCodeLen = 64

// validateCurrencyCode enforces the one currency-code charset, shared by
// CurrencyInput.Validate (where a code is born) and ConversionQuote.Validate
// (where a code is replayed back to a holder as the explanation of a charge):
//
//	1-64 characters from [A-Za-z0-9_-]
//
// ASCII letters of either case, digits, underscore and hyphen are exactly
// what the codes in use already spell ("USDT", "CREDITS", "INPUT_TOKEN",
// "USDT-late"). Everything else is refused -- in particular whitespace,
// control characters and Unicode format characters (category Cf: the
// right-to-left override U+202E, zero-width joiner U+200D, ...), which can
// make "1 USDC -> 1000 CREDITS" render reversed or make two different codes
// look identical on a statement. A narrower allowlist rather than a
// denylist of the dangerous classes: there is no confusable that can get
// through a 64-symbol ASCII alphabet.
func validateCurrencyCode(scope, field, code string) error {
	if code == "" {
		return fmt.Errorf("%s: %s required: %w", scope, field, ErrInvalidInput)
	}
	if len(code) > maxCurrencyCodeLen {
		return fmt.Errorf("%s: %s exceeds %d characters: %w", scope, field, maxCurrencyCodeLen, ErrInvalidInput)
	}
	for i := 0; i < len(code); i++ {
		c := code[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return fmt.Errorf("%s: %s must use only A-Z, a-z, 0-9, '_' and '-': %w", scope, field, ErrInvalidInput)
		}
	}
	return nil
}
