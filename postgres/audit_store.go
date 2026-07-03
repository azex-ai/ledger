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
	dims *dimCache
}

// Compile-time check.
var _ core.AuditQuerier = (*AuditStore)(nil)

// NewAuditStore creates an AuditStore backed by a connection pool.
func NewAuditStore(pool *pgxpool.Pool) *AuditStore {
	return &AuditStore{
		pool: pool,
		db:   pool,
		q:    sqlcgen.New(pool),
		dims: dimCacheFor(pool),
	}
}

// WithDB returns a clone bound to an existing transaction.
func (s *AuditStore) WithDB(db DBTX) *AuditStore {
	return &AuditStore{
		pool: s.pool,
		db:   db,
		q:    sqlcgen.New(db),
		dims: s.dims,
	}
}

// sinceOrEpoch returns t if it is non-zero, otherwise the zero time.
// PostgreSQL's 'epoch'::timestamptz = 1970-01-01 00:00:00 UTC, which is
// what time.Time{} becomes when sent to pgx.
func sinceOrEpoch(t time.Time) time.Time {
	return t // zero time.Time maps to epoch in pgx
}

// ListJournalsByAccount returns journals whose entries touch the given account
// dimension. classificationID=0 matches all classifications.
func (s *AuditStore) ListJournalsByAccount(ctx context.Context, filter core.AuditFilter) ([]core.Journal, string, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}

	currencyID := int64(0)
	if filter.CurrencyUID != "" {
		d, err := s.dims.currencyByUIDOrErr(ctx, s.q, filter.CurrencyUID)
		if err != nil {
			return nil, "", err
		}
		currencyID = d.ID
	}
	classificationID := int64(0)
	if filter.ClassificationUID != "" {
		d, err := s.dims.classByUIDOrErr(ctx, s.q, filter.ClassificationUID)
		if err != nil {
			return nil, "", err
		}
		classificationID = d.ID
	}
	rows, err := s.q.ListJournalsByAccount(ctx, sqlcgen.ListJournalsByAccountParams{
		Holder:           filter.AccountHolder,
		CurrencyID:       currencyID,
		ClassificationID: classificationID,
		Since:            sinceOrEpoch(filter.Since),
		Until:            sinceOrEpoch(filter.Until),
		CursorID:         decodeAuditCursor(filter.Cursor),
		PageLimit:        limit,
	})
	if err != nil {
		return nil, "", fmt.Errorf("postgres: audit: list journals by account: %w", err)
	}
	journals, err := s.journalsFromRows(ctx, rows)
	if err != nil {
		return nil, "", err
	}
	return journals, nextAuditCursor(rows, limit), nil
}

// ListEntriesByJournal returns all entries for a single journal.
func (s *AuditStore) ListEntriesByJournal(ctx context.Context, journalUID string) ([]core.Entry, error) {
	pgUID, err := uidToPG(journalUID)
	if err != nil {
		return nil, err
	}
	j, err := s.q.GetJournalByUID(ctx, pgUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: audit: journal %q: %w", journalUID, core.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: audit: get journal: %w", err)
	}
	rows, err := s.q.ListJournalEntries(ctx, j.ID)
	if err != nil {
		return nil, fmt.Errorf("postgres: audit: list entries by journal: %w", err)
	}
	entries := make([]core.Entry, len(rows))
	for i, r := range rows {
		e, err := entryCore(ctx, s.dims, s.q, r.JournalUid, r.AccountHolder, r.CurrencyID, r.ClassificationID, r.EntryType, r.Amount, r.EffectiveAt, r.CreatedAt)
		if err != nil {
			return nil, err
		}
		entries[i] = *e
	}
	return entries, nil
}

// ListJournalsByTimeRange returns journals created within [filter.Since, filter.Until].
// Zero-value time fields are treated as "unbounded" on that side.
func (s *AuditStore) ListJournalsByTimeRange(ctx context.Context, filter core.AuditFilter) ([]core.Journal, string, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}

	rows, err := s.q.ListJournalsByTimeRange(ctx, sqlcgen.ListJournalsByTimeRangeParams{
		Since:     sinceOrEpoch(filter.Since),
		Until:     sinceOrEpoch(filter.Until),
		CursorID:  decodeAuditCursor(filter.Cursor),
		PageLimit: limit,
	})
	if err != nil {
		return nil, "", fmt.Errorf("postgres: audit: list journals by time range: %w", err)
	}
	journals, err := s.journalsFromRows(ctx, rows)
	if err != nil {
		return nil, "", err
	}
	return journals, nextAuditCursor(rows, limit), nil
}

// TraceBooking returns the booking together with all its events and linked journals.
func (s *AuditStore) TraceBooking(ctx context.Context, bookingUID string) (*core.BookingTrace, error) {
	pgUID, err := uidToPG(bookingUID)
	if err != nil {
		return nil, err
	}
	bookingRow, err := s.q.GetBookingByUID(ctx, pgUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: audit: trace booking %q: %w", bookingUID, core.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: audit: trace booking: get booking: %w", err)
	}
	bookingID := bookingRow.ID

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
		ev, err := eventFromRow(ctx, s.dims, s.q, e)
		if err != nil {
			return nil, err
		}
		events[i] = *ev
	}

	booking, err := bookingFromRow(ctx, s.dims, s.q, bookingRow)
	if err != nil {
		return nil, err
	}
	journals, err := s.journalsFromRows(ctx, journalRows)
	if err != nil {
		return nil, err
	}
	return &core.BookingTrace{
		Booking:  *booking,
		Events:   events,
		Journals: journals,
	}, nil
}

// ListReversals returns the full reversal chain for a journal — the root journal
// plus any journals that transitively reverse it.
func (s *AuditStore) ListReversals(ctx context.Context, journalUID string) ([]core.Journal, error) {
	pgUID, err := uidToPG(journalUID)
	if err != nil {
		return nil, err
	}
	j, err := s.q.GetJournalByUID(ctx, pgUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: audit: journal %q: %w", journalUID, core.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: audit: get journal: %w", err)
	}
	rows, err := s.q.GetReversalChain(ctx, j.ID)
	if err != nil {
		return nil, fmt.Errorf("postgres: audit: list reversals: %w", err)
	}
	return s.journalsFromRows(ctx, rows)
}

// journalsFromRows converts a slice of sqlcgen.Journal rows to core.Journal values.
func (s *AuditStore) journalsFromRows(ctx context.Context, rows []sqlcgen.Journal) ([]core.Journal, error) {
	result := make([]core.Journal, len(rows))
	for i, r := range rows {
		j, err := journalFromRow(ctx, s.dims, s.q, r)
		if err != nil {
			return nil, err
		}
		result[i] = *j
	}
	return result, nil
}

// nextAuditCursor returns the opaque cursor for the next page: the keyset
// position of the last row when the page is full, "" when the result set is
// exhausted.
func nextAuditCursor(rows []sqlcgen.Journal, limit int32) string {
	if int32(len(rows)) < limit {
		return ""
	}
	return encodeCursorString(rows[len(rows)-1].ID)
}

// decodeAuditCursor turns the opaque cursor string back into the internal
// keyset position. "" (or garbage) starts from the beginning — an invalid
// cursor can't leak anything, it just restarts the scan.
func decodeAuditCursor(cursor string) int64 {
	if cursor == "" {
		return 0
	}
	v, err := decodeCursorString(cursor)
	if err != nil {
		return 0
	}
	return v
}
