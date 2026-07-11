package postgres

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"time"

	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

type journalEntryFingerprint struct {
	holder           int64
	currencyID       int64
	classificationID int64
	entryType        string
	amount           string
}

func (s *LedgerStore) ensureJournalMatchesInput(ctx context.Context, q *sqlcgen.Queries, existing sqlcgen.Journal, input core.JournalInput) (*core.Journal, error) {
	// EffectiveAt is only compared when the caller explicitly set it: a zero
	// value on input means "defaulted to now() at insert time", which is
	// necessarily different across retries and would falsely conflict.
	effectiveAtMismatch := !input.EffectiveAt.IsZero() && !existing.EffectiveAt.Equal(input.EffectiveAt)

	// The comparison happens in uid space: map the stored row's internal
	// references back to uids so a retried uid-space payload compares 1:1.
	existingCore, err := journalFromRow(ctx, s.dims, q, existing)
	if err != nil {
		return nil, err
	}

	if existingCore.JournalTypeUID != input.JournalTypeUID ||
		existing.ActorID != input.ActorID ||
		existing.Source != input.Source ||
		existingCore.EventUID != input.EventUID ||
		existingCore.ReversalOfUID != input.ReversalOfUID ||
		effectiveAtMismatch ||
		!mustNumericToDecimal(existing.TotalDebit).Equal(totalDebit(input.Entries)) ||
		!mustNumericToDecimal(existing.TotalCredit).Equal(totalCredit(input.Entries)) ||
		string(metadataToJSON(existingCore.Metadata)) != string(metadataToJSON(input.Metadata)) {
		return nil, fmt.Errorf("postgres: post journal: idempotency key %q payload mismatch: %w", input.IdempotencyKey, core.ErrConflict)
	}

	rows, err := q.ListJournalEntries(ctx, existing.ID)
	if err != nil {
		return nil, fmt.Errorf("postgres: post journal: load existing entries: %w", err)
	}
	same, err := s.sameJournalEntries(ctx, q, rows, input.Entries)
	if err != nil {
		return nil, err
	}
	if !same {
		return nil, fmt.Errorf("postgres: post journal: idempotency key %q entries mismatch: %w", input.IdempotencyKey, core.ErrConflict)
	}

	return existingCore, nil
}

func (s *ReserverStore) ensureReservationMatchesInput(ctx context.Context, q *sqlcgen.Queries, existing sqlcgen.Reservation, input core.ReserveInput, currencyID int64) (*core.Reservation, error) {
	// ExpiresIn is stored as ExpiresAt, computed from CreatedAt at insert time.
	// Comparing the stored duration against the resolved input duration enforces
	// the "same key + different payload = ErrConflict" contract for expiry too,
	// while a 1s tolerance absorbs db timestamp precision (microseconds in PG).
	storedExpiresIn := existing.ExpiresAt.Sub(existing.CreatedAt)
	expiresInDrift := storedExpiresIn - resolveReservationExpiresIn(input.ExpiresIn)
	if expiresInDrift < -time.Second || expiresInDrift > time.Second {
		return nil, fmt.Errorf("postgres: reserve: idempotency key %q payload mismatch: %w", input.IdempotencyKey, core.ErrConflict)
	}

	if existing.AccountHolder != input.AccountHolder ||
		existing.CurrencyID != currencyID ||
		!mustNumericToDecimal(existing.ReservedAmount).Equal(input.Amount) {
		return nil, fmt.Errorf("postgres: reserve: idempotency key %q payload mismatch: %w", input.IdempotencyKey, core.ErrConflict)
	}
	return reservationFromRow(ctx, s.dims, q, existing)
}

func (s *BookingStore) ensureBookingMatchesInput(ctx context.Context, q *sqlcgen.Queries, existing sqlcgen.Booking, input core.CreateBookingInput) (*core.Booking, error) {
	class, err := q.GetClassification(ctx, existing.ClassificationID)
	if err != nil {
		return nil, fmt.Errorf("postgres: create booking: load existing classification: %w", err)
	}
	cur, err := s.dims.currencyByUIDOrErr(ctx, q, input.CurrencyUID)
	if err != nil {
		return nil, fmt.Errorf("postgres: create booking: %w", err)
	}

	if class.Code != input.ClassificationCode ||
		existing.AccountHolder != input.AccountHolder ||
		existing.CurrencyID != cur.ID ||
		existing.ChannelName != input.ChannelName ||
		!mustNumericToDecimal(existing.Amount).Equal(input.Amount) ||
		!existing.ExpiresAt.Equal(input.ExpiresAt) ||
		// Compare metadata semantically, not by raw byte string: existing.Metadata
		// is the RAW jsonb column value, which Postgres re-serializes in its own
		// canonical form (keys ordered by length-then-lexicographic, ": " with a
		// space) -- never byte-identical to stringMetadataToJSON's compact,
		// alphabetically-sorted output from encoding/json, even when the two
		// maps hold identical key/value pairs. A naive string compare here
		// spuriously ErrConflicts every genuine idempotent replay whose metadata
		// has more than one key. Route existing's bytes through the same
		// jsonToStringMetadata parse used everywhere else so both sides compare
		// as Go maps -- and, per bookingMetadataMatches's doc comment,
		// tolerate "block_number" changing across replays (design doc §3 /
		// I-20).
		!bookingMetadataMatches(jsonToStringMetadata(existing.Metadata), input.Metadata) {
		return nil, fmt.Errorf("postgres: create booking: idempotency key %q payload mismatch: %w", input.IdempotencyKey, core.ErrConflict)
	}

	return bookingFromRow(ctx, s.dims, q, existing)
}

// bookingMetadataObservationVariantKeys are booking Metadata keys this
// file's idempotency comparison treats as observation-variant rather than
// part of the compared payload. Each must match a literal key
// service/onchain.go writes, either into CreateBooking's Metadata directly
// or by merging a later Transition's Metadata onto the booking row (see
// bookingMetadataMatches's doc comment for why each one is here).
var bookingMetadataObservationVariantKeys = []string{
	// core.DepositSighting.BlockNumber, written by IngestDeposit's
	// CreateBooking call.
	"block_number",
	// service.Onchain.routeToReview's reviewReasonMetaKey and
	// RejectReview's rejectReasonMetaKey -- both are added to the booking's
	// Metadata by a Transition call that happens AFTER CreateBooking, never
	// present in CreateBookingInput's own Metadata.
	"review_reason",
	"reject_reason",
}

// bookingMetadataMatches is ensureBookingMatchesInput's booking-Metadata
// comparison. It is maps.Equal for every key except
// bookingMetadataObservationVariantKeys, which are stripped from both sides
// first.
//
// core.DepositSighting.BlockNumber is deliberately persisted on the deposit
// booking (see that field's doc comment) so a later recheck can recompute
// confirmations without re-scanning the chain -- it cannot simply be omitted
// from CreateBooking's Metadata the way Confirmations is (line ~476 in
// service/onchain.go), because a booking that crashes before its first
// pending->confirming transition would otherwise have nowhere to read a
// block number back from at all.
//
// But unlike every other field in Metadata, block_number is expected to
// legitimately differ across two CreateBooking calls carrying the exact same
// idempotency key: a chain reorg can reassign the identical transaction to a
// different block between the first observation (already durably booked)
// and a retried one (e.g. a watcher crash-recovers and re-scans the same
// unconsumed block range after the reorg). Comparing it exactly would turn
// that legitimate idempotent retry into a spurious ErrConflict -- exactly
// the failure design doc §3 / invariant I-20 rule out.
//
// review_reason/reject_reason are the same class of problem, introduced by
// M3 (design doc §9): once a deposit has been routed to review (or rejected
// out of it), its stored Metadata carries a key no CreateBooking replay of
// the original sighting will ever include -- a watcher rescan or a retried
// webhook delivery of the identical transfer re-derives the exact same
// idempotency key and the exact same CreateBookingInput.Metadata (chain
// id/tx hash/txlog_seq/token/block_number only), and must still resolve to
// the existing booking rather than spuriously ErrConflict-ing on a key it
// never had a chance to set. The retry must still resolve to the original
// booking, keeping whichever value the first successful create/transition
// recorded; every other Metadata key is still compared exactly, so this is
// not a general escape hatch.
func bookingMetadataMatches(existing, input map[string]string) bool {
	strip := func(m map[string]string) map[string]string {
		out := make(map[string]string, len(m))
		maps.Copy(out, m)
		for _, k := range bookingMetadataObservationVariantKeys {
			delete(out, k)
		}
		return out
	}
	return maps.Equal(strip(existing), strip(input))
}

func totalDebit(entries []core.EntryInput) decimal.Decimal {
	total := decimal.Zero
	for _, entry := range entries {
		if entry.EntryType == core.EntryTypeDebit {
			total = total.Add(entry.Amount)
		}
	}
	return total
}

func totalCredit(entries []core.EntryInput) decimal.Decimal {
	total := decimal.Zero
	for _, entry := range entries {
		if entry.EntryType == core.EntryTypeCredit {
			total = total.Add(entry.Amount)
		}
	}
	return total
}

func (s *LedgerStore) sameJournalEntries(ctx context.Context, q *sqlcgen.Queries, rows []sqlcgen.ListJournalEntriesRow, input []core.EntryInput) (bool, error) {
	if len(rows) != len(input) {
		return false, nil
	}

	left := make([]journalEntryFingerprint, len(rows))
	for i, row := range rows {
		left[i] = journalEntryFingerprint{
			holder:           row.AccountHolder,
			currencyID:       row.CurrencyID,
			classificationID: row.ClassificationID,
			entryType:        row.EntryType,
			amount:           mustNumericToDecimal(row.Amount).String(),
		}
	}

	right := make([]journalEntryFingerprint, len(input))
	for i, entry := range input {
		cur, err := s.dims.currencyByUIDOrErr(ctx, q, entry.CurrencyUID)
		if err != nil {
			return false, err
		}
		cls, err := s.dims.classByUIDOrErr(ctx, q, entry.ClassificationUID)
		if err != nil {
			return false, err
		}
		right[i] = journalEntryFingerprint{
			holder:           entry.AccountHolder,
			currencyID:       cur.ID,
			classificationID: cls.ID,
			entryType:        string(entry.EntryType),
			amount:           entry.Amount.String(),
		}
	}

	sort.Slice(left, func(i, j int) bool { return left[i].less(left[j]) })
	sort.Slice(right, func(i, j int) bool { return right[i].less(right[j]) })

	for i := range left {
		if left[i] != right[i] {
			return false, nil
		}
	}
	return true, nil
}

func (f journalEntryFingerprint) less(other journalEntryFingerprint) bool {
	if f.holder != other.holder {
		return f.holder < other.holder
	}
	if f.currencyID != other.currencyID {
		return f.currencyID < other.currencyID
	}
	if f.classificationID != other.classificationID {
		return f.classificationID < other.classificationID
	}
	if f.entryType != other.entryType {
		return f.entryType < other.entryType
	}
	return f.amount < other.amount
}
