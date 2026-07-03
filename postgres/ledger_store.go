package postgres

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"go.opentelemetry.io/otel/attribute"

	"github.com/azex-ai/ledger/core"
	ledgerotel "github.com/azex-ai/ledger/pkg/otel"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

// Compile-time check: *pgxpool.Pool satisfies DBTX.
var _ DBTX = (*pgxpool.Pool)(nil)

// Compile-time interface assertions.
var (
	_ core.JournalWriter         = (*LedgerStore)(nil)
	_ core.TemplateBatchExecutor = (*LedgerStore)(nil)
	_ core.BalanceReader         = (*LedgerStore)(nil)
)

// LedgerStore implements JournalWriter and BalanceReader using PostgreSQL.
//
// In pool mode (constructed via NewLedgerStore), every write operation that
// requires atomicity starts its own transaction. GetBalance wraps its two
// queries in a REPEATABLE READ transaction to prevent phantom reads.
//
// In tx mode (constructed via NewLedgerStore then bound via withDB), the store
// participates in the caller's transaction. Write operations that previously
// started their own transaction now use the provided pgx.Tx directly (they do
// not call Commit/Rollback — the caller owns the transaction lifecycle).
// GetBalance does NOT start a REPEATABLE READ sub-transaction; the caller's
// transaction isolation level applies instead.
type LedgerStore struct {
	// pool is non-nil only in pool mode. It is used for BeginTx when an
	// explicit isolation level (e.g. REPEATABLE READ for GetBalance) is needed.
	// When nil, the store is tx-bound and must use db directly.
	pool *pgxpool.Pool
	db   DBTX
	q    *sqlcgen.Queries
	dims *dimCache
}

// balancePair identifies a (holder, currency_id) pair targeted by an advisory
// lock. Used to dedupe + sort the entries in a journal before locking.
type balancePair struct {
	holder     int64
	currencyID int64
}

// balancePairsFromEntries returns the unique (holder, currency_id) pairs in
// entries, sorted lexicographically. Sorted order is required to take advisory
// locks in the same global order across concurrent transactions, otherwise
// deadlocks become possible (tx A locks pair P1 then P2 while tx B locks P2
// then P1).
// resolvedEntry is an EntryInput whose uid dimension references have been
// resolved to internal storage ids (plus the dimension metadata the write
// pipeline needs). It exists only inside the postgres adapter — internal ids
// never cross back into core types (api-contract §3).
type resolvedEntry struct {
	core.EntryInput
	currencyID       int64
	classificationID int64
	exponent         int32
	normalSide       core.NormalSide
}

// resolveEntries maps every entry's currency/classification uid to internal
// dimensions via the dims cache (one refresh at most for the whole batch).
func (s *LedgerStore) resolveEntries(ctx context.Context, q *sqlcgen.Queries, entries []core.EntryInput) ([]resolvedEntry, error) {
	out := make([]resolvedEntry, len(entries))
	for i, e := range entries {
		cur, err := s.dims.currencyByUIDOrErr(ctx, q, e.CurrencyUID)
		if err != nil {
			return nil, fmt.Errorf("postgres: entry[%d]: %w", i, err)
		}
		cls, err := s.dims.classByUIDOrErr(ctx, q, e.ClassificationUID)
		if err != nil {
			return nil, fmt.Errorf("postgres: entry[%d]: %w", i, err)
		}
		out[i] = resolvedEntry{
			EntryInput:       e,
			currencyID:       cur.ID,
			classificationID: cls.ID,
			exponent:         cur.Exponent,
			normalSide:       cls.NormalSide,
		}
	}
	return out, nil
}

func balancePairsFromEntries(entries []resolvedEntry) []balancePair {
	seen := make(map[balancePair]struct{}, len(entries))
	pairs := make([]balancePair, 0, len(entries))
	for _, e := range entries {
		p := balancePair{holder: e.AccountHolder, currencyID: e.currencyID}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		pairs = append(pairs, p)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].holder != pairs[j].holder {
			return pairs[i].holder < pairs[j].holder
		}
		return pairs[i].currencyID < pairs[j].currencyID
	})
	return pairs
}

// acquireBalanceLocks takes a transaction-scoped advisory lock on every
// (holder, currency_id) pair in pairs. Pairs must be presorted (see
// balancePairsFromEntries). The locks are tx-scoped and released at COMMIT/ROLLBACK.
func acquireBalanceLocks(ctx context.Context, q *sqlcgen.Queries, pairs []balancePair) error {
	for _, p := range pairs {
		key := fmt.Sprintf("balance:%d:%d", p.holder, p.currencyID)
		if err := q.AcquireBalanceLock(ctx, key); err != nil {
			return fmt.Errorf("postgres: post journal: advisory lock (%d,%d): %w", p.holder, p.currencyID, err)
		}
	}
	return nil
}

func acquireIdempotencyLock(ctx context.Context, q *sqlcgen.Queries, key string) error {
	if err := q.AcquireIdempotencyLock(ctx, key); err != nil {
		return fmt.Errorf("postgres: idempotency lock %q: %w", key, err)
	}
	return nil
}

// NewLedgerStore creates a new LedgerStore backed by a connection pool. The
// store starts its own transactions for write operations and uses REPEATABLE
// READ isolation for GetBalance.
func NewLedgerStore(pool *pgxpool.Pool) *LedgerStore {
	return &LedgerStore{
		pool: pool,
		db:   pool,
		q:    sqlcgen.New(pool),
		dims: dimCacheFor(pool),
	}
}

// WithDB returns a clone of the LedgerStore bound to an existing transaction
// (or any DBTX implementor). The clone shares no mutable state with the
// original and is safe for concurrent use alongside it. The caller owns the
// transaction lifecycle (commit/rollback).
func (s *LedgerStore) WithDB(db DBTX) *LedgerStore {
	return &LedgerStore{
		pool: nil, // tx mode: pool deliberately nil
		db:   db,
		q:    sqlcgen.New(db),
		dims: s.dims,
	}
}

// PostJournal posts a balanced journal within a transaction.
// Idempotent: same key + same payload returns the existing journal; divergent
// payload returns ErrConflict.
//
// In pool mode a new transaction is started and committed here.
// In tx mode (store bound via withDB) the journal is written directly into
// the caller's transaction; commit/rollback is the caller's responsibility.
func (s *LedgerStore) PostJournal(ctx context.Context, input core.JournalInput) (*core.Journal, error) {
	ctx, span := ledgerotel.StartSpan(ctx, "ledger.ledger.post_journal",
		attribute.String("idempotency_key", input.IdempotencyKey),
		attribute.String("journal_type_uid", input.JournalTypeUID),
		attribute.Int64("actor_id", input.ActorID),
		attribute.String("source", input.Source),
	)
	defer span.End()

	if s.pool == nil {
		// Tx mode: use the caller's transaction directly.
		j, err := s.postJournalWithQueries(ctx, s.q, input)
		ledgerotel.RecordError(span, err)
		return j, err
	}

	// Pool mode: own the transaction lifecycle.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return nil, fmt.Errorf("postgres: post journal: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)
	journal, err := s.postJournalWithQueries(ctx, qtx, input)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		ledgerotel.RecordError(span, err)
		return nil, fmt.Errorf("postgres: post journal: commit: %w", err)
	}

	return journal, nil
}

// ExecuteTemplate loads a template by code, renders it, and posts the journal.
func (s *LedgerStore) ExecuteTemplate(ctx context.Context, templateCode string, params core.TemplateParams) (*core.Journal, error) {
	ctx, span := ledgerotel.StartSpan(ctx, "ledger.ledger.execute_template",
		attribute.String("template_code", templateCode),
	)
	defer span.End()

	input, err := s.renderTemplate(ctx, s.q, templateCode, params)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return nil, err
	}

	j, err := s.PostJournal(ctx, *input)
	ledgerotel.RecordError(span, err)
	return j, err
}

// ExecuteTemplateBatch renders and posts multiple templates in a single transaction.
//
// In pool mode a new transaction is started and committed here (all-or-nothing).
// In tx mode (store bound via withDB) all journals are written directly into
// the caller's transaction; commit/rollback is the caller's responsibility.
func (s *LedgerStore) ExecuteTemplateBatch(ctx context.Context, requests []core.TemplateExecutionRequest) ([]*core.Journal, error) {
	if len(requests) == 0 {
		return nil, nil
	}

	if s.pool == nil {
		// Tx mode: write directly into caller's transaction.
		return s.executeTemplateBatchWithQueries(ctx, s.q, requests)
	}

	// Pool mode: own the transaction lifecycle.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: execute template batch: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)
	journals, err := s.executeTemplateBatchWithQueries(ctx, qtx, requests)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: execute template batch: commit: %w", err)
	}

	return journals, nil
}

func (s *LedgerStore) executeTemplateBatchWithQueries(ctx context.Context, q *sqlcgen.Queries, requests []core.TemplateExecutionRequest) ([]*core.Journal, error) {
	inputs := make([]core.JournalInput, len(requests))
	for i, req := range requests {
		input, err := s.renderTemplate(ctx, q, req.TemplateCode, req.Params)
		if err != nil {
			return nil, fmt.Errorf("postgres: execute template batch[%d]: %w", i, err)
		}
		inputs[i] = *input
	}

	journals := make([]*core.Journal, 0, len(inputs))
	for i, input := range inputs {
		journal, err := s.postJournalWithQueries(ctx, q, input)
		if err != nil {
			return nil, fmt.Errorf("postgres: execute template batch[%d]: %w", i, err)
		}
		journals = append(journals, journal)
	}
	return journals, nil
}

// ReverseJournal creates a full reversal journal for the given journal ID.
// It rejects (ErrConflict) if journalID already has any reversal recorded
// against it, full or partial — see ReverseJournalFraction for posting
// additional partial reversals against a journal that already has history.
//
// In pool mode a new transaction is started and committed here: the
// SELECT ... FOR UPDATE row lock on the original journal and the reversal
// insert must share one transaction, so the "no reversal history yet" check
// cannot race a concurrent full or partial reversal. Migration 029 dropped
// the at-most-once unique index on reversal_of — this row lock is the only
// thing standing between two concurrent full reversals (with different
// reasons, hence different idempotency keys) and a 200% reversal. In tx
// mode (store bound via WithDB) it participates in the caller's transaction.
func (s *LedgerStore) ReverseJournal(ctx context.Context, journalUID string, reason string) (*core.Journal, error) {
	if s.pool == nil {
		return s.reverseJournalWithQueries(ctx, s.q, journalUID, reason)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: reverse journal: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)
	journal, err := s.reverseJournalWithQueries(ctx, qtx, journalUID, reason)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: reverse journal: commit: %w", err)
	}
	return journal, nil
}

func (s *LedgerStore) reverseJournalWithQueries(ctx context.Context, q *sqlcgen.Queries, journalUID string, reason string) (*core.Journal, error) {
	pgUID, err := uidToPG(journalUID)
	if err != nil {
		return nil, err
	}
	// Row-lock the original journal for the rest of this transaction, same as
	// ReverseJournalFraction: full and partial reversals of one journal all
	// serialize on this lock, so the history check below sees every committed
	// reversal and no concurrent one can land until we commit.
	original, err := q.GetJournalForUpdateByUID(ctx, pgUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: reverse journal: journal %q: %w", journalUID, core.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: reverse journal: get journal: %w", err)
	}
	journalID := original.ID
	if original.ReversalOf.Valid {
		return nil, fmt.Errorf("postgres: reverse journal: journal %q is already a reversal: %w", journalUID, core.ErrConflict)
	}

	// The derived idempotency key stays keyed on the journal's uid so it is
	// stable across replays and never mentions the internal id.
	expectedKey := fmt.Sprintf("reversal:%s:%s", journalUID, reason)
	existingReversals, err := q.ListReversalsByOriginalJournalID(ctx, int64ToInt8(&journalID))
	if err != nil {
		return nil, fmt.Errorf("postgres: reverse journal: lookup reversals: %w", err)
	}
	if len(existingReversals) > 0 {
		// Same (journal, reason) as an existing reversal → idempotent retry,
		// return it. Any other existing reversal — full or partial, any
		// reason — means this journal already has reversal history; a second
		// full reversal on top of that would double-count whatever was
		// already reversed, so it is rejected regardless of reason text.
		for _, r := range existingReversals {
			if r.IdempotencyKey == expectedKey {
				return journalFromRow(ctx, s.dims, q, r)
			}
		}
		return nil, fmt.Errorf(
			"postgres: reverse journal: journal %q already has %d reversal(s) recorded; use ReverseJournalFraction for further partial reversals: %w",
			journalUID, len(existingReversals), core.ErrConflict,
		)
	}

	entries, err := q.ListJournalEntries(ctx, journalID)
	if err != nil {
		return nil, fmt.Errorf("postgres: reverse journal: list entries: %w", err)
	}

	// Build reversed entries (swap debit/credit), mapping internal dimension
	// ids back to the uid space PostJournal consumes.
	reversedEntries := make([]core.EntryInput, len(entries))
	for i, e := range entries {
		entryType := core.EntryTypeDebit
		if core.EntryType(e.EntryType) == core.EntryTypeDebit {
			entryType = core.EntryTypeCredit
		}
		cur, err := s.dims.currencyByIDOrErr(ctx, q, e.CurrencyID)
		if err != nil {
			return nil, fmt.Errorf("postgres: reverse journal: %w", err)
		}
		cls, err := s.dims.classByIDOrErr(ctx, q, e.ClassificationID)
		if err != nil {
			return nil, fmt.Errorf("postgres: reverse journal: %w", err)
		}
		reversedEntries[i] = core.EntryInput{
			AccountHolder:     e.AccountHolder,
			CurrencyUID:       cur.UID,
			ClassificationUID: cls.UID,
			EntryType:         entryType,
			Amount:            mustNumericToDecimal(e.Amount),
		}
	}

	jt, err := s.dims.jtByIDOrErr(ctx, q, original.JournalTypeID)
	if err != nil {
		return nil, fmt.Errorf("postgres: reverse journal: %w", err)
	}
	input := core.JournalInput{
		JournalTypeUID: jt.UID,
		IdempotencyKey: expectedKey,
		Entries:        reversedEntries,
		Source:         "reversal",
		ReversalOfUID:  journalUID,
		Metadata:       map[string]string{"reason": reason},
	}

	return s.postJournalWithQueries(ctx, q, input)
}

func (s *LedgerStore) renderTemplate(ctx context.Context, q *sqlcgen.Queries, templateCode string, params core.TemplateParams) (*core.JournalInput, error) {
	tmplRow, err := q.GetTemplateByCode(ctx, templateCode)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: execute template: template %q: %w", templateCode, core.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: execute template: get template: %w", err)
	}

	lines, err := q.GetTemplateLines(ctx, tmplRow.ID)
	if err != nil {
		return nil, fmt.Errorf("postgres: execute template: get lines: %w", err)
	}

	tmpl, err := templateFromRow(ctx, s.dims, q, tmplRow, lines)
	if err != nil {
		return nil, fmt.Errorf("postgres: execute template: %w", err)
	}
	input, err := tmpl.Render(params)
	if err != nil {
		return nil, fmt.Errorf("postgres: execute template: render: %w", err)
	}
	return input, nil
}

func (s *LedgerStore) postJournalWithQueries(ctx context.Context, q *sqlcgen.Queries, input core.JournalInput) (*core.Journal, error) {
	if err := input.Validate(); err != nil {
		return nil, fmt.Errorf("postgres: post journal: %w", err)
	}

	existing, err := q.GetJournalByIdempotencyKey(ctx, input.IdempotencyKey)
	if err == nil {
		return s.ensureJournalMatchesInput(ctx, q, existing, input)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: post journal: check idempotency: %w", err)
	}

	if err := acquireIdempotencyLock(ctx, q, input.IdempotencyKey); err != nil {
		return nil, err
	}

	existing, err = q.GetJournalByIdempotencyKey(ctx, input.IdempotencyKey)
	if err == nil {
		return s.ensureJournalMatchesInput(ctx, q, existing, input)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: post journal: check idempotency after lock: %w", err)
	}

	// Resolve every uid reference to internal storage ids up front: entry
	// dimensions, journal type, and the optional event/reversal links. This is
	// the single boundary where uid-space input becomes id-space storage.
	resolved, err := s.resolveEntries(ctx, q, input.Entries)
	if err != nil {
		return nil, fmt.Errorf("postgres: post journal: %w", err)
	}
	jt, err := s.dims.jtByUIDOrErr(ctx, q, input.JournalTypeUID)
	if err != nil {
		return nil, fmt.Errorf("postgres: post journal: %w", err)
	}
	eventID := int64(0)
	if input.EventUID != "" {
		pgUID, err := uidToPG(input.EventUID)
		if err != nil {
			return nil, fmt.Errorf("postgres: post journal: event %q: %w", input.EventUID, core.ErrNotFound)
		}
		id, err := q.GetEventIDByUID(ctx, pgUID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("postgres: post journal: event %q: %w", input.EventUID, core.ErrNotFound)
			}
			return nil, fmt.Errorf("postgres: post journal: resolve event: %w", err)
		}
		eventID = id
	}
	if err := validateEntriesPrecision(resolved); err != nil {
		return nil, err
	}
	reversalOfID := int64(0)
	if input.ReversalOfUID != "" {
		pgUID, err := uidToPG(input.ReversalOfUID)
		if err != nil {
			return nil, fmt.Errorf("postgres: post journal: reversal_of %q: %w", input.ReversalOfUID, core.ErrNotFound)
		}
		orig, err := q.GetJournalByUID(ctx, pgUID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("postgres: post journal: reversal_of %q: %w", input.ReversalOfUID, core.ErrNotFound)
			}
			return nil, fmt.Errorf("postgres: post journal: resolve reversal_of: %w", err)
		}
		reversalOfID = orig.ID
	}

	// EffectiveAt defaults to now() when the caller didn't set it (core.Validate
	// already rejected a future value beyond the clock-skew tolerance).
	effectiveAt := input.EffectiveAt
	if effectiveAt.IsZero() {
		effectiveAt = time.Now()
	}

	// Period close (I-15): reject postings whose effective date falls before
	// the active close line. GetActivePeriodClose returns pgx.ErrNoRows when
	// the period has never been closed — nothing to enforce in that case.
	activeClose, err := q.GetActivePeriodClose(ctx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: post journal: get active period close: %w", err)
	}
	if err == nil && effectiveAt.Before(activeClose.CloseBefore) {
		return nil, fmt.Errorf(
			"postgres: post journal: effective_at %s is before the period close line %s: %w",
			effectiveAt.Format(time.RFC3339), activeClose.CloseBefore.Format(time.RFC3339), core.ErrPeriodClosed,
		)
	}

	// Invariant: every balance-mutating tx must take pg_advisory_xact_lock(holder,
	// currency_id) for every affected (holder, currency_id) pair, in sorted order.
	// This serializes against ReserverStore.Reserve (which takes the same lock),
	// preventing TOCTOU races where a reserve reads stale balance while a journal
	// is being committed. Locks are taken in lexicographic (holder, currency_id)
	// order to avoid deadlocks when two journals touch overlapping pairs.
	if err := acquireBalanceLocks(ctx, q, balancePairsFromEntries(resolved)); err != nil {
		return nil, err
	}

	// Account policy enforcement (I-17): frozen/closed status + min_balance,
	// evaluated inside the same advisory lock so it is TOCTOU-safe against
	// concurrent journals/reserves/policy changes on the same (holder,
	// currency) pairs. Must run before any row below is written since a
	// rejection here must abort the whole journal.
	if err := s.enforceAccountPolicies(ctx, q, resolved); err != nil {
		return nil, err
	}

	debit, credit := input.Totals()

	row, err := q.InsertJournal(ctx, sqlcgen.InsertJournalParams{
		JournalTypeID:  jt.ID,
		IdempotencyKey: input.IdempotencyKey,
		TotalDebit:     decimalToNumeric(debit),
		TotalCredit:    decimalToNumeric(credit),
		Metadata:       metadataToJSON(input.Metadata),
		ActorID:        input.ActorID,
		Source:         input.Source,
		ReversalOf:     int64ToInt8(zeroInt64ToNil(reversalOfID)),
		EventID:        eventID,
		EffectiveAt:    effectiveAt,
		Uid:            newUID(),
	})
	if err != nil {
		existing, lookupErr := q.GetJournalByIdempotencyKey(ctx, input.IdempotencyKey)
		if lookupErr == nil {
			return s.ensureJournalMatchesInput(ctx, q, existing, input)
		}
		if !errors.Is(lookupErr, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: post journal: insert journal: %w (idempotency recheck: %v)", normalizeStoreError(err), lookupErr)
		}
		return nil, wrapStoreError("postgres: post journal: insert journal", err)
	}

	type rollupKey struct {
		holder           int64
		currencyID       int64
		classificationID int64
	}
	seen := make(map[rollupKey]struct{})

	for i, e := range resolved {
		_, err := q.InsertJournalEntry(ctx, sqlcgen.InsertJournalEntryParams{
			JournalID:        row.ID,
			AccountHolder:    e.AccountHolder,
			CurrencyID:       e.currencyID,
			ClassificationID: e.classificationID,
			EntryType:        string(e.EntryType),
			Amount:           decimalToNumeric(e.Amount),
			EffectiveAt:      effectiveAt,
		})
		if err != nil {
			return nil, wrapStoreError(fmt.Sprintf("postgres: post journal: insert entry[%d]", i), err)
		}

		key := rollupKey{holder: e.AccountHolder, currencyID: e.currencyID, classificationID: e.classificationID}
		seen[key] = struct{}{}
	}

	for key := range seen {
		if err := q.EnqueueRollup(ctx, sqlcgen.EnqueueRollupParams{
			AccountHolder:    key.holder,
			CurrencyID:       key.currencyID,
			ClassificationID: key.classificationID,
		}); err != nil {
			return nil, wrapStoreError("postgres: post journal: enqueue rollup", err)
		}
	}

	// Per-currency balance check (replaces the dropped per-row CONSTRAINT
	// TRIGGER from migration 004). One query per posted journal — O(1) per
	// journal versus the trigger's O(N^2). Runs in the same transaction so a
	// failure rolls back the journal and entries together.
	badCurrency, err := q.VerifyJournalBalanced(ctx, row.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, wrapStoreError("postgres: post journal: verify balanced", err)
	}
	if err == nil {
		return nil, fmt.Errorf("postgres: post journal: journal %d unbalanced in currency %d: %w", row.ID, badCurrency, core.ErrUnbalancedJournal)
	}

	if eventID != 0 {
		if err := s.linkJournalToEventAndBooking(ctx, q, eventID, row.ID); err != nil {
			return nil, err
		}
	}

	return journalFromRow(ctx, s.dims, q, row)
}

func (s *LedgerStore) linkJournalToEventAndBooking(ctx context.Context, q *sqlcgen.Queries, eventID, journalID int64) error {
	event, err := q.GetEvent(ctx, eventID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("postgres: post journal: event %d: %w", eventID, core.ErrNotFound)
		}
		return fmt.Errorf("postgres: post journal: get event %d: %w", eventID, err)
	}
	if event.JournalID.Valid && event.JournalID.Int64 != journalID {
		return fmt.Errorf("postgres: post journal: event %d already linked to journal %d: %w", eventID, event.JournalID.Int64, core.ErrConflict)
	}

	if _, err := q.LinkEventJournal(ctx, sqlcgen.LinkEventJournalParams{
		ID:        eventID,
		JournalID: int64ToInt8(&journalID),
	}); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return wrapStoreError("postgres: post journal: link event journal", err)
		}
		current, getErr := q.GetEvent(ctx, eventID)
		if getErr != nil {
			return fmt.Errorf("postgres: post journal: recheck event %d: %w", eventID, getErr)
		}
		if !current.JournalID.Valid || current.JournalID.Int64 != journalID {
			return fmt.Errorf("postgres: post journal: event %d already linked to a different journal: %w", eventID, core.ErrConflict)
		}
	}

	if event.BookingID == 0 {
		return nil
	}

	booking, err := q.GetBooking(ctx, event.BookingID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("postgres: post journal: booking %d from event %d: %w", event.BookingID, eventID, core.ErrNotFound)
		}
		return fmt.Errorf("postgres: post journal: get booking %d: %w", event.BookingID, err)
	}
	if booking.JournalID.Valid && booking.JournalID.Int64 != journalID {
		return fmt.Errorf("postgres: post journal: booking %d already linked to journal %d: %w", event.BookingID, booking.JournalID.Int64, core.ErrConflict)
	}

	if _, err := q.LinkBookingJournal(ctx, sqlcgen.LinkBookingJournalParams{
		ID:        event.BookingID,
		JournalID: int64ToInt8(&journalID),
	}); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return wrapStoreError("postgres: post journal: link booking journal", err)
		}
		current, getErr := q.GetBooking(ctx, event.BookingID)
		if getErr != nil {
			return fmt.Errorf("postgres: post journal: recheck booking %d: %w", event.BookingID, getErr)
		}
		if !current.JournalID.Valid || current.JournalID.Int64 != journalID {
			return fmt.Errorf("postgres: post journal: booking %d already linked to a different journal: %w", event.BookingID, core.ErrConflict)
		}
	}

	return nil
}

// GetBalance computes balance for a single (holder, currency, classification) dimension.
// Balance = checkpoint.balance + delta (entries since checkpoint).
// Delta computation respects normal_side of the classification.
//
// In pool mode, both queries run inside a REPEATABLE READ transaction to
// prevent phantom reads from concurrent journal writes between the two queries.
//
// In tx mode (store bound via withDB), no sub-transaction is started; the
// caller's transaction isolation level applies. If the caller requires
// snapshot consistency, it should begin its transaction with REPEATABLE READ
// before calling GetBalance.
func (s *LedgerStore) GetBalance(ctx context.Context, holder int64, currencyUID, classificationUID string) (decimal.Decimal, error) {
	ctx, span := ledgerotel.StartSpan(ctx, "ledger.ledger.get_balance",
		attribute.Int64("account_holder", holder),
		attribute.String("currency_uid", currencyUID),
		attribute.String("classification_uid", classificationUID),
	)
	defer span.End()

	cur, err := s.dims.currencyByUIDOrErr(ctx, s.q, currencyUID)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return decimal.Zero, err
	}
	cls, err := s.dims.classByUIDOrErr(ctx, s.q, classificationUID)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return decimal.Zero, err
	}
	currencyID, classificationID := cur.ID, cls.ID

	if s.pool == nil {
		// Tx mode: use the caller's transaction directly — no inner tx.
		bal, err := s.getBalanceWithQueries(ctx, s.q, holder, currencyID, classificationID)
		ledgerotel.RecordError(span, err)
		return bal, err
	}

	// Pool mode: wrap in REPEATABLE READ to prevent phantom reads between the
	// checkpoint query and the entry-sum query.
	tx, txErr := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if txErr != nil {
		ledgerotel.RecordError(span, txErr)
		return decimal.Zero, fmt.Errorf("postgres: get balance: begin tx: %w", txErr)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)
	bal, err := s.getBalanceWithQueries(ctx, qtx, holder, currencyID, classificationID)
	ledgerotel.RecordError(span, err)
	return bal, err
}

// getBalanceWithQueries is the shared inner implementation of GetBalance. It
// executes against whichever *sqlcgen.Queries is provided (pool-backed or
// tx-backed). The caller is responsible for transaction lifecycle.
func (s *LedgerStore) getBalanceWithQueries(ctx context.Context, q *sqlcgen.Queries, holder, currencyID, classificationID int64) (decimal.Decimal, error) {
	// Get checkpoint (may not exist yet)
	var checkpointBalance decimal.Decimal
	var sinceEntryID int64

	cp, err := q.GetBalanceCheckpoint(ctx, sqlcgen.GetBalanceCheckpointParams{
		AccountHolder:    holder,
		CurrencyID:       currencyID,
		ClassificationID: classificationID,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return decimal.Zero, fmt.Errorf("postgres: get balance: checkpoint: %w", err)
	}
	if err == nil {
		checkpointBalance = mustNumericToDecimal(cp.Balance)
		sinceEntryID = cp.LastEntryID
	}

	// Get entry sums since checkpoint
	sums, err := q.SumEntriesSinceCheckpoint(ctx, sqlcgen.SumEntriesSinceCheckpointParams{
		AccountHolder: holder,
		CurrencyID:    currencyID,
		SinceEntryID:  sinceEntryID,
	})
	if err != nil {
		return decimal.Zero, fmt.Errorf("postgres: get balance: sum entries: %w", err)
	}

	// We need the normal_side to compute balance direction.
	// For now, sum debits and credits for the specific classification.
	var debitSum, creditSum decimal.Decimal
	for _, row := range sums {
		if row.ClassificationID != classificationID {
			continue
		}
		amount, err := anyToDecimal(row.Total)
		if err != nil {
			return decimal.Zero, fmt.Errorf("postgres: get balance: convert total: %w", err)
		}
		switch core.EntryType(row.EntryType) {
		case core.EntryTypeDebit:
			debitSum = debitSum.Add(amount)
		case core.EntryTypeCredit:
			creditSum = creditSum.Add(amount)
		}
	}

	// Get classification to determine normal_side
	cls, err := q.GetClassification(ctx, classificationID)
	if err != nil {
		return decimal.Zero, fmt.Errorf("postgres: get balance: get classification %d: %w", classificationID, err)
	}
	normalSide := core.NormalSide(cls.NormalSide)

	// Compute delta based on normal_side:
	// debit-normal: balance increases with debits, decreases with credits
	// credit-normal: balance increases with credits, decreases with debits
	var delta decimal.Decimal
	switch normalSide {
	case core.NormalSideDebit:
		delta = debitSum.Sub(creditSum)
	case core.NormalSideCredit:
		delta = creditSum.Sub(debitSum)
	default:
		// Default to debit-normal
		delta = debitSum.Sub(creditSum)
	}

	return checkpointBalance.Add(delta), nil
}

// GetBalanceBreakdown aggregates the holder's classification balances by
// core.BalanceRole and layers reservation holds on top:
//
//	pending   = Σ balance(role=pending)
//	locked    = Σ balance(role=locked) + held (reservations)
//	available = Σ balance(role=available) − held
//	total     = available + locked + pending
//
// In pool mode the whole read runs inside one REPEATABLE READ transaction so
// the role sums and the holds figure describe the same point in time. In tx
// mode the caller's transaction (and isolation level) applies.
func (s *LedgerStore) GetBalanceBreakdown(ctx context.Context, holder int64, currencyUID string) (*core.BalanceBreakdown, error) {
	ctx, span := ledgerotel.StartSpan(ctx, "ledger.ledger.get_balance_breakdown",
		attribute.Int64("account_holder", holder),
		attribute.String("currency_uid", currencyUID),
	)
	defer span.End()

	cur, err := s.dims.currencyByUIDOrErr(ctx, s.q, currencyUID)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return nil, err
	}

	if s.pool == nil {
		b, err := s.getBalanceBreakdownWithQueries(ctx, s.q, holder, cur.ID, currencyUID)
		ledgerotel.RecordError(span, err)
		return b, err
	}

	tx, txErr := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if txErr != nil {
		ledgerotel.RecordError(span, txErr)
		return nil, fmt.Errorf("postgres: get balance breakdown: begin tx: %w", txErr)
	}
	defer tx.Rollback(ctx)

	b, err := s.getBalanceBreakdownWithQueries(ctx, s.q.WithTx(tx), holder, cur.ID, currencyUID)
	ledgerotel.RecordError(span, err)
	return b, err
}

func (s *LedgerStore) getBalanceBreakdownWithQueries(ctx context.Context, q *sqlcgen.Queries, holder, currencyID int64, currencyUID string) (*core.BalanceBreakdown, error) {
	roleSums, err := s.sumBalancesByRoleWithQueries(ctx, q, holder, currencyID)
	if err != nil {
		return nil, fmt.Errorf("postgres: get balance breakdown: %w", err)
	}

	heldRaw, err := q.SumActiveReservations(ctx, sqlcgen.SumActiveReservationsParams{
		AccountHolder: holder,
		CurrencyID:    currencyID,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: get balance breakdown: sum reservations: %w", err)
	}
	held, err := anyToDecimal(heldRaw)
	if err != nil {
		return nil, fmt.Errorf("postgres: get balance breakdown: convert held: %w", err)
	}

	available := roleSums[core.BalanceRoleAvailable].Sub(held)
	pending := roleSums[core.BalanceRolePending]
	locked := roleSums[core.BalanceRoleLocked].Add(held)

	return &core.BalanceBreakdown{
		AccountHolder: holder,
		CurrencyUID:   currencyUID,
		Available:     available,
		Pending:       pending,
		Locked:        locked,
		Total:         available.Add(locked).Add(pending),
	}, nil
}

// sumBalancesByRoleWithQueries sums checkpoint+delta balances of every
// classification the holder has entries in, bucketed by the classification's
// balance_role. Role-less (”) classifications are skipped. Roles are read
// fresh from the config table (not the dims cache) because SetBalanceRole can
// retag a classification after creation — the dims cache only holds immutable
// fields.
func (s *LedgerStore) sumBalancesByRoleWithQueries(ctx context.Context, q *sqlcgen.Queries, holder, currencyID int64) (map[core.BalanceRole]decimal.Decimal, error) {
	dims, err := q.ListClassificationDims(ctx)
	if err != nil {
		return nil, fmt.Errorf("list classification roles: %w", err)
	}
	roleByClassID := make(map[int64]core.BalanceRole, len(dims))
	for _, d := range dims {
		roleByClassID[d.ID] = core.BalanceRole(d.BalanceRole)
	}

	clsIDs, err := q.DistinctClassificationsForAccount(ctx, sqlcgen.DistinctClassificationsForAccountParams{
		AccountHolder: holder,
		CurrencyID:    currencyID,
	})
	if err != nil {
		return nil, fmt.Errorf("list account classifications: %w", err)
	}

	sums := map[core.BalanceRole]decimal.Decimal{
		core.BalanceRoleAvailable: decimal.Zero,
		core.BalanceRolePending:   decimal.Zero,
		core.BalanceRoleLocked:    decimal.Zero,
	}
	for _, clsID := range clsIDs {
		role := roleByClassID[clsID]
		if role == core.BalanceRoleNone {
			continue
		}
		bal, err := s.getBalanceWithQueries(ctx, q, holder, currencyID, clsID)
		if err != nil {
			return nil, fmt.Errorf("balance for classification %d: %w", clsID, err)
		}
		sums[role] = sums[role].Add(bal)
	}
	return sums, nil
}

// GetBalances returns balances across all classifications for a (holder, currency).
func (s *LedgerStore) GetBalances(ctx context.Context, holder int64, currencyUID string) ([]core.Balance, error) {
	cur, err := s.dims.currencyByUIDOrErr(ctx, s.q, currencyUID)
	if err != nil {
		return nil, err
	}
	// Discover all classifications that have entries for this account
	clsRows, err := s.q.DistinctClassificationsForAccount(ctx, sqlcgen.DistinctClassificationsForAccountParams{
		AccountHolder: holder,
		CurrencyID:    cur.ID,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: get balances: list classifications: %w", err)
	}

	balances := make([]core.Balance, 0, len(clsRows))
	for _, clsID := range clsRows {
		cls, err := s.dims.classByIDOrErr(ctx, s.q, clsID)
		if err != nil {
			return nil, err
		}
		bal, err := s.GetBalance(ctx, holder, currencyUID, cls.UID)
		if err != nil {
			return nil, fmt.Errorf("postgres: get balances: classification %s: %w", cls.UID, err)
		}
		balances = append(balances, core.Balance{
			AccountHolder:     holder,
			CurrencyUID:       currencyUID,
			ClassificationUID: cls.UID,
			Balance:           bal,
		})
	}

	return balances, nil
}

// BatchGetBalances returns balances for multiple holders.
func (s *LedgerStore) BatchGetBalances(ctx context.Context, holderIDs []int64, currencyUID string) (map[int64][]core.Balance, error) {
	result := make(map[int64][]core.Balance, len(holderIDs))
	for _, id := range holderIDs {
		bals, err := s.GetBalances(ctx, id, currencyUID)
		if err != nil {
			return nil, fmt.Errorf("postgres: batch get balances: holder %d: %w", id, err)
		}
		result[id] = bals
	}
	return result, nil
}
