package core

import (
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
)

// Field-level upper bounds for the two free-form fields every write carries.
//
// Why they exist (lead addendum to the 2026-09-02 audit, from
// w1-ledgerstore's sibling scan): Metadata and Source had no bound at any
// layer. The HTTP surface caps a request body (Config.MaxBodyBytes, 256 KB
// by default), which hides the gap for one of the two consumption modes --
// but library mode has no such cap, so a consumer could hand PostJournal a
// megabyte-sized metadata map and the ledger would store it, on an
// append-only table it can never compact. "The body limit covers it" is the
// shape of reasoning that leaves the other mode unprotected.
//
// The numbers are deliberately generous: metadata is for correlation ids,
// tx hashes and short labels, and nothing in this repository's presets or
// examples comes within an order of magnitude of them. They are a bound
// against pathology, not a business rule -- so they are checked in core,
// once, where both consumption modes pass through, rather than in the HTTP
// handler where only one does.
const (
	// MaxSourceLen bounds Source, a short scope label ("api", "worker",
	// "webhook", a service name).
	MaxSourceLen = 256
	// MaxMetadataKeys bounds how many metadata entries one write may carry.
	MaxMetadataKeys = 64
	// MaxMetadataKeyLen bounds one metadata key.
	MaxMetadataKeyLen = 128
	// MaxMetadataValueLen bounds one metadata value.
	MaxMetadataValueLen = 2048
	// MaxMetadataTotalLen bounds the sum of all keys and values, so many
	// medium-sized entries cannot add up to what no single entry may be.
	MaxMetadataTotalLen = 16384
)

// validateFreeformFields checks Source and Metadata against the bounds above.
// scope names the caller for the error message ("journal", "booking", ...).
func validateFreeformFields(scope string, source string, metadata map[string]string) error {
	if len(source) > MaxSourceLen {
		return fmt.Errorf("core: %s: source is %d bytes, limit %d: %w", scope, len(source), MaxSourceLen, ErrInvalidInput)
	}
	if len(metadata) > MaxMetadataKeys {
		return fmt.Errorf("core: %s: metadata has %d keys, limit %d: %w", scope, len(metadata), MaxMetadataKeys, ErrInvalidInput)
	}
	total := 0
	for k, v := range metadata {
		if len(k) > MaxMetadataKeyLen {
			return fmt.Errorf("core: %s: metadata key is %d bytes, limit %d: %w", scope, len(k), MaxMetadataKeyLen, ErrInvalidInput)
		}
		if len(v) > MaxMetadataValueLen {
			return fmt.Errorf("core: %s: metadata[%q] is %d bytes, limit %d: %w", scope, k, len(v), MaxMetadataValueLen, ErrInvalidInput)
		}
		total += len(k) + len(v)
	}
	if total > MaxMetadataTotalLen {
		return fmt.Errorf("core: %s: metadata totals %d bytes, limit %d: %w", scope, total, MaxMetadataTotalLen, ErrInvalidInput)
	}
	return nil
}

// --- Amount magnitude ---

// MaxAmountIntegerDigits is the widest amount this ledger can store: the
// integer part of NUMERIC(30,18), which is 30 total digits minus 18
// fractional ones.
//
// It is a bound against pathology in exactly the sense the Metadata and
// Source bounds above are, and it was missing for the same reason they
// were: the HTTP surface looked like it covered the problem (a body cap
// bounds how many DIGITS a caller can type) and library mode has no such
// cap. But for amounts the body cap does not cover it even in HTTP mode,
// because a short string can name an enormous number.
//
// The failure it prevents (found by FuzzJournalValidate inside its 30s CI
// budget, 2026-09-03): `{"amount": "1E999999999"}` is nineteen bytes.
// shopspring/decimal stores it lazily as coefficient 1 with exponent
// 999999999, so parsing, IsPositive, Equal, Add and Truncate are all
// microseconds -- JournalInput.Validate passed it, and so did the
// per-currency precision check, whose `amount.Equal(amount.Truncate(18))`
// is true for a value with no fractional digits. Nothing looked at it
// until pgx rendered the decimal to bind a NUMERIC parameter, at which
// point String() expands a billion digits: PostJournal did not return
// after ninety seconds, burning CPU in math/big and allocating without
// bound. Any credential that can post a journal could stop the process,
// and library-mode callers had nothing in front of them at all.
//
// The check has to run before any arithmetic, not just before storage:
// adding two decimals with different exponents rescales one of them, which
// expands it the same way.
const MaxAmountIntegerDigits = 30 - 18

// MaxAmountFractionalDigits is the other side of NUMERIC(30,18). Currency
// exponents bound this more tightly per currency (I-16, enforced in the
// adapter where the currency is known); this is the absolute floor, so a
// value that no currency could ever accept is refused in core, in both
// consumption modes, before it reaches an adapter.
const MaxAmountFractionalDigits = 18

// ValidateAmountMagnitude refuses an amount this ledger could not store.
//
// It is constant time and does not materialize the value: Exponent() is a
// field read, and Coefficient() is the integer the caller's digits parsed
// into -- already bounded by the length of what they sent. Neither expands
// the 10^exponent scaling factor, which is the operation that does not
// terminate.
//
// scope and field name the caller for the error message ("journal
// entry[0]", "reserve"), because an amount rejected with no indication of
// which one it was is a support ticket.
func ValidateAmountMagnitude(scope, field string, amount decimal.Decimal) error {
	exponent := int(amount.Exponent())
	if exponent > 0 && exponent > MaxAmountIntegerDigits {
		// Short-circuit before Coefficient(): an exponent this large is
		// already unstorable whatever the coefficient is, and this is the
		// shape the fuzz target found.
		return fmt.Errorf(
			"core: %s: %s has an exponent of %d, and this ledger stores NUMERIC(30,18) -- at most %d integer digits: %w",
			scope, field, exponent, MaxAmountIntegerDigits, ErrInvalidInput)
	}
	if exponent < -MaxAmountFractionalDigits {
		return fmt.Errorf(
			"core: %s: %s has %d fractional digits, and this ledger stores NUMERIC(30,18) -- at most %d: %w",
			scope, field, -exponent, MaxAmountFractionalDigits, ErrInvalidInput)
	}

	// coefficientDigits + exponent is the number of integer digits the
	// value would need. Both operands are small by now: the exponent is
	// bounded above, and the coefficient's digit count is bounded by the
	// caller's own input length.
	//
	// TrimPrefix, not len(): a negative coefficient renders with a leading
	// "-", which would count as a digit and refuse -999999999999 -- a
	// perfectly storable overdraft limit. The acceptance control caught it.
	integerDigits := len(strings.TrimPrefix(amount.Coefficient().String(), "-")) + exponent
	if integerDigits > MaxAmountIntegerDigits {
		return fmt.Errorf(
			"core: %s: %s needs %d integer digits, and this ledger stores NUMERIC(30,18) -- at most %d: %w",
			scope, field, integerDigits, MaxAmountIntegerDigits, ErrInvalidInput)
	}
	return nil
}
