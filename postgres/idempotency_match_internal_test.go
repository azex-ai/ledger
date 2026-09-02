package postgres

// F-P20 (2026-09-02 audit): bookingMetadataObservationVariantKeys grew from
// 1 key (block_number) to 5, but only block_number and (after last wave) the
// review/reject audit keys as a group had dedicated tests
// (postgres/invariants_test.go's TestDepositBooking_IdempotencyKey_Stable*
// tests, which exercise the full CreateBooking round trip through Postgres).
// Neither proves each key in isolation both (a) is safely ignorable and (b)
// does not accidentally widen the exclusion to cover a real business field.
//
// This file tests bookingMetadataMatches directly -- it is a pure function,
// so no database is needed to cover every key exhaustively. The table is
// derived FROM bookingMetadataObservationVariantKeys itself (not a second,
// hand-copied list): adding a sixth key to that slice without adding test
// data for it is automatically covered by this table, closing exactly the
// gap this finding describes ("adding a key to the list without adding a
// test used to be silent").

import "testing"

// TestBookingMetadataMatches_ObservationVariantKeys_TableDriven pins, for
// EVERY key in bookingMetadataObservationVariantKeys:
//  1. two Metadata maps differing ONLY in that key must still match
//     (the idempotent-replay case this exclusion list exists for).
//  2. two Metadata maps differing in a key OUTSIDE the exclusion list must
//     NOT match (the ErrConflict case -- proves the exclusion isn't
//     accidentally swallowing everything).
func TestBookingMetadataMatches_ObservationVariantKeys_TableDriven(t *testing.T) {
	base := func() map[string]string {
		return map[string]string{
			"chain_id":  "1",
			"tx_hash":   "0xstable",
			"txlog_seq": "0",
		}
	}

	for _, key := range bookingMetadataObservationVariantKeys {
		t.Run(key, func(t *testing.T) {
			existing := base()
			existing[key] = "observed-value-A"
			input := base()
			input[key] = "observed-value-B"

			if !bookingMetadataMatches(existing, input) {
				t.Errorf("bookingMetadataMatches must ignore %q -- it is in bookingMetadataObservationVariantKeys, "+
					"but a value-only difference on that key still failed the match", key)
			}
		})
	}

	// Direction 2: a business field outside the exclusion list must still
	// cause a mismatch, proving the exclusion list is narrow, not a
	// catch-all. Run once (not per key) since it does not depend on any
	// individual excluded key.
	t.Run("non_excluded_field_still_conflicts", func(t *testing.T) {
		existing := base()
		input := base()
		input["tx_hash"] = "0xdifferent" // tx_hash is not in the exclusion list
		if bookingMetadataMatches(existing, input) {
			t.Error("bookingMetadataMatches matched on a difference in tx_hash, which is not an observation-variant key -- " +
				"this would let a genuinely different deposit silently collide under the same idempotency key")
		}
	})

	// Guards the table itself: if bookingMetadataObservationVariantKeys is
	// ever emptied by mistake, this test would otherwise pass trivially
	// with zero subtests and look green for the wrong reason.
	if len(bookingMetadataObservationVariantKeys) == 0 {
		t.Fatal("bookingMetadataObservationVariantKeys is empty -- this test has nothing to cover")
	}
}
