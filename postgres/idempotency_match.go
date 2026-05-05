package postgres

import (
	"context"
	"fmt"
	"sort"

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

func ensureJournalMatchesInput(ctx context.Context, q *sqlcgen.Queries, existing sqlcgen.Journal, input core.JournalInput) (*core.Journal, error) {
	if existing.JournalTypeID != input.JournalTypeID ||
		existing.ActorID != input.ActorID ||
		existing.Source != input.Source ||
		existing.EventID != input.EventID ||
		journalReversalOf(existing) != input.ReversalOf ||
		!mustNumericToDecimal(existing.TotalDebit).Equal(totalDebit(input.Entries)) ||
		!mustNumericToDecimal(existing.TotalCredit).Equal(totalCredit(input.Entries)) ||
		string(metadataToJSON(journalFromRow(existing).Metadata)) != string(metadataToJSON(input.Metadata)) {
		return nil, fmt.Errorf("postgres: post journal: idempotency key %q payload mismatch: %w", input.IdempotencyKey, core.ErrConflict)
	}

	rows, err := q.ListJournalEntries(ctx, existing.ID)
	if err != nil {
		return nil, fmt.Errorf("postgres: post journal: load existing entries: %w", err)
	}
	if !sameJournalEntries(rows, input.Entries) {
		return nil, fmt.Errorf("postgres: post journal: idempotency key %q entries mismatch: %w", input.IdempotencyKey, core.ErrConflict)
	}

	return journalFromRow(existing), nil
}

func ensureReservationMatchesInput(existing sqlcgen.Reservation, input core.ReserveInput) (*core.Reservation, error) {
	if existing.AccountHolder != input.AccountHolder ||
		existing.CurrencyID != input.CurrencyID ||
		!mustNumericToDecimal(existing.ReservedAmount).Equal(input.Amount) {
		return nil, fmt.Errorf("postgres: reserve: idempotency key %q payload mismatch: %w", input.IdempotencyKey, core.ErrConflict)
	}
	return reservationFromRow(existing), nil
}

func ensureBookingMatchesInput(ctx context.Context, q *sqlcgen.Queries, existing sqlcgen.Booking, input core.CreateBookingInput) (*core.Booking, error) {
	class, err := q.GetClassification(ctx, existing.ClassificationID)
	if err != nil {
		return nil, fmt.Errorf("postgres: create booking: load existing classification: %w", err)
	}

	if class.Code != input.ClassificationCode ||
		existing.AccountHolder != input.AccountHolder ||
		existing.CurrencyID != input.CurrencyID ||
		existing.ChannelName != input.ChannelName ||
		!mustNumericToDecimal(existing.Amount).Equal(input.Amount) ||
		!existing.ExpiresAt.Equal(input.ExpiresAt) ||
		string(existing.Metadata) != string(anyMetadataToJSON(input.Metadata)) {
		return nil, fmt.Errorf("postgres: create booking: idempotency key %q payload mismatch: %w", input.IdempotencyKey, core.ErrConflict)
	}

	return bookingFromRow(existing), nil
}

func journalReversalOf(row sqlcgen.Journal) int64 {
	if row.ReversalOf.Valid {
		return row.ReversalOf.Int64
	}
	return 0
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

func sameJournalEntries(rows []sqlcgen.JournalEntry, input []core.EntryInput) bool {
	if len(rows) != len(input) {
		return false
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
		right[i] = journalEntryFingerprint{
			holder:           entry.AccountHolder,
			currencyID:       entry.CurrencyID,
			classificationID: entry.ClassificationID,
			entryType:        string(entry.EntryType),
			amount:           entry.Amount.String(),
		}
	}

	sort.Slice(left, func(i, j int) bool { return left[i].less(left[j]) })
	sort.Slice(right, func(i, j int) bool { return right[i].less(right[j]) })

	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
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
