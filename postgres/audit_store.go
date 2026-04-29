package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

// AuditStore implements core.AuditQuerier using PostgreSQL.
// All methods are read-only; no data is mutated.
type AuditStore struct {
	pool *pgxpool.Pool
	db   DBTX
	q    *sqlcgen.Queries
}

// Compile-time check.
var _ core.AuditQuerier = (*AuditStore)(nil)

// NewAuditStore creates an AuditStore backed by a connection pool.
func NewAuditStore(pool *pgxpool.Pool) *AuditStore {
	return &AuditStore{
		pool: pool,
		db:   pool,
		q:    sqlcgen.New(pool),
	}
}

// WithDB returns a clone bound to an existing transaction.
func (s *AuditStore) WithDB(db DBTX) *AuditStore {
	return &AuditStore{
		pool: s.pool,
		db:   db,
		q:    sqlcgen.New(db),
	}
}

// epoch is the zero-value sentinel used to signal "no time filter" to the
// SQL queries (they compare against 'epoch'::timestamptz).
var epoch = time.Time{}

// sinceOrEpoch returns t if it is non-zero, otherwise the zero time.
// PostgreSQL's 'epoch'::timestamptz = 1970-01-01 00:00:00 UTC, which is
// what time.Time{} becomes when sent to pgx.
func sinceOrEpoch(t time.Time) time.Time {
	return t // zero time.Time maps to epoch in pgx
}

// ListJournalsByAccount returns journals whose entries touch the given account
// dimension. classificationID=0 matches all classifications.
func (s *AuditStore) ListJournalsByAccount(ctx context.Context, filter core.AuditFilter) ([]core.Journal, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}

	rows, err := s.q.ListJournalsByAccount(ctx, sqlcgen.ListJournalsByAccountParams{
		Holder:           filter.AccountHolder,
		CurrencyID:       filter.CurrencyID,
		ClassificationID: filter.ClassificationID,
		Since:            sinceOrEpoch(filter.Since),
		Until:            sinceOrEpoch(filter.Until),
		CursorID:         filter.Cursor,
		PageLimit:        limit,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: audit: list journals by account: %w", err)
	}
	return journalsFromRows(rows), nil
}

// ListEntriesByJournal returns all entries for a single journal.
func (s *AuditStore) ListEntriesByJournal(ctx context.Context, journalID int64) ([]core.Entry, error) {
	rows, err := s.q.ListJournalEntries(ctx, journalID)
	if err != nil {
		return nil, fmt.Errorf("postgres: audit: list entries by journal: %w", err)
	}
	entries := make([]core.Entry, len(rows))
	for i, r := range rows {
		entries[i] = *entryFromRow(r)
	}
	return entries, nil
}

// ListJournalsByTimeRange returns journals created within [filter.Since, filter.Until].
// Zero-value time fields are treated as "unbounded" on that side.
func (s *AuditStore) ListJournalsByTimeRange(ctx context.Context, filter core.AuditFilter) ([]core.Journal, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}

	rows, err := s.q.ListJournalsByTimeRange(ctx, sqlcgen.ListJournalsByTimeRangeParams{
		Since:     sinceOrEpoch(filter.Since),
		Until:     sinceOrEpoch(filter.Until),
		CursorID:  filter.Cursor,
		PageLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: audit: list journals by time range: %w", err)
	}
	return journalsFromRows(rows), nil
}

// TraceBooking returns the booking together with all its events and linked journals.
func (s *AuditStore) TraceBooking(ctx context.Context, bookingID int64) (*core.BookingTrace, error) {
	// Fetch the booking first.
	bookingRow, err := s.q.GetBooking(ctx, bookingID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: audit: trace booking: id %d: %w", bookingID, core.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: audit: trace booking: get booking: %w", err)
	}

	// Fetch all events.
	eventRows, err := s.q.TraceBookingEvents(ctx, bookingID)
	if err != nil {
		return nil, fmt.Errorf("postgres: audit: trace booking: list events: %w", err)
	}

	// Fetch all journals linked to those events.
	journalRows, err := s.q.TraceBookingJournals(ctx, bookingID)
	if err != nil {
		return nil, fmt.Errorf("postgres: audit: trace booking: list journals: %w", err)
	}

	events := make([]core.Event, len(eventRows))
	for i, e := range eventRows {
		events[i] = *eventFromRow(e)
	}

	return &core.BookingTrace{
		Booking:  *bookingFromRow(bookingRow),
		Events:   events,
		Journals: journalsFromRows(journalRows),
	}, nil
}

// ListReversals returns the full reversal chain for a journal — the root journal
// plus any journals that transitively reverse it.
func (s *AuditStore) ListReversals(ctx context.Context, journalID int64) ([]core.Journal, error) {
	rows, err := s.q.GetReversalChain(ctx, journalID)
	if err != nil {
		return nil, fmt.Errorf("postgres: audit: list reversals: %w", err)
	}
	return journalsFromRows(rows), nil
}

// journalsFromRows converts a slice of sqlcgen.Journal rows to core.Journal values.
func journalsFromRows(rows []sqlcgen.Journal) []core.Journal {
	result := make([]core.Journal, len(rows))
	for i, r := range rows {
		result[i] = *journalFromRow(r)
	}
	return result
}
