package core

import "fmt"

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
