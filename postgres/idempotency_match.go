package postgres

import (
	"context"
	"fmt"
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
		string(existing.Metadata) != string(stringMetadataToJSON(input.Metadata)) {
		return nil, fmt.Errorf("postgres: create booking: idempotency key %q payload mismatch: %w", input.IdempotencyKey, core.ErrConflict)
	}

	return bookingFromRow(ctx, s.dims, q, existing)
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
