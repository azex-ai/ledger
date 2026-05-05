package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

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
func balancePairsFromEntries(entries []core.EntryInput) []balancePair {
	seen := make(map[balancePair]struct{}, len(entries))
	pairs := make([]balancePair, 0, len(entries))
	for _, e := range entries {
		p := balancePair{holder: e.AccountHolder, currencyID: e.CurrencyID}
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

// acquireBalanceLocks takes pg_advisory_xact_lock(int4, int4) on every
// (holder, currency_id) pair in pairs. Pairs must be presorted (see
// balancePairsFromEntries). The locks are tx-scoped and released at COMMIT/ROLLBACK.
func acquireBalanceLocks(ctx context.Context, q *sqlcgen.Queries, pairs []balancePair) error {
	for _, p := range pairs {
		if p.holder > math.MaxInt32 || p.holder < math.MinInt32 {
			return fmt.Errorf("postgres: post journal: holder %d does not fit in int32 for advisory lock: %w", p.holder, core.ErrInvalidInput)
		}
		if p.currencyID > math.MaxInt32 || p.currencyID < math.MinInt32 {
			return fmt.Errorf("postgres: post journal: currency_id %d does not fit in int32 for advisory lock: %w", p.currencyID, core.ErrInvalidInput)
		}
		if err := q.AcquireBalanceLock(ctx, sqlcgen.AcquireBalanceLockParams{
			Holder:     int32(p.holder),
			CurrencyID: int32(p.currencyID),
		}); err != nil {
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

type queryProvider interface {
	GetTemplateByCode(ctx context.Context, code string) (sqlcgen.EntryTemplate, error)
	GetTemplateLines(ctx context.Context, templateID int64) ([]sqlcgen.EntryTemplateLine, error)
}

// NewLedgerStore creates a new LedgerStore backed by a connection pool. The
// store starts its own transactions for write operations and uses REPEATABLE
// READ isolation for GetBalance.
func NewLedgerStore(pool *pgxpool.Pool) *LedgerStore {
	return &LedgerStore{
		pool: pool,
		db:   pool,
		q:    sqlcgen.New(pool),
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
		attribute.Int64("journal_type_id", input.JournalTypeID),
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

// ReverseJournal creates a reversal journal for the given journal ID.
func (s *LedgerStore) ReverseJournal(ctx context.Context, journalID int64, reason string) (*core.Journal, error) {
	original, err := s.q.GetJournal(ctx, journalID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: reverse journal: journal %d: %w", journalID, core.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: reverse journal: get journal: %w", err)
	}
	if original.ReversalOf.Valid {
		return nil, fmt.Errorf("postgres: reverse journal: journal %d is already a reversal: %w", journalID, core.ErrConflict)
	}

	existingReversal, err := s.q.GetReversalByOriginalJournalID(ctx, int64ToInt8(&journalID))
	if err == nil {
		return nil, fmt.Errorf("postgres: reverse journal: journal %d already reversed by %d: %w", journalID, existingReversal.ID, core.ErrConflict)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: reverse journal: lookup reversal: %w", err)
	}

	entries, err := s.q.ListJournalEntries(ctx, journalID)
	if err != nil {
		return nil, fmt.Errorf("postgres: reverse journal: list entries: %w", err)
	}

	// Build reversed entries (swap debit/credit)
	reversedEntries := make([]core.EntryInput, len(entries))
	for i, e := range entries {
		entryType := core.EntryTypeDebit
		if core.EntryType(e.EntryType) == core.EntryTypeDebit {
			entryType = core.EntryTypeCredit
		}
		reversedEntries[i] = core.EntryInput{
			AccountHolder:    e.AccountHolder,
			CurrencyID:       e.CurrencyID,
			ClassificationID: e.ClassificationID,
			EntryType:        entryType,
			Amount:           mustNumericToDecimal(e.Amount),
		}
	}

	input := core.JournalInput{
		JournalTypeID:  original.JournalTypeID,
		IdempotencyKey: fmt.Sprintf("reversal:%d:%s", journalID, reason),
		Entries:        reversedEntries,
		Source:         "reversal",
		ReversalOf:     journalID,
		Metadata:       map[string]string{"reason": reason},
	}

	return s.PostJournal(ctx, input)
}

func (s *LedgerStore) renderTemplate(ctx context.Context, q queryProvider, templateCode string, params core.TemplateParams) (*core.JournalInput, error) {
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

	tmpl := templateFromRow(tmplRow, lines)
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
		return ensureJournalMatchesInput(ctx, q, existing, input)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: post journal: check idempotency: %w", err)
	}

	if err := acquireIdempotencyLock(ctx, q, input.IdempotencyKey); err != nil {
		return nil, err
	}

	existing, err = q.GetJournalByIdempotencyKey(ctx, input.IdempotencyKey)
	if err == nil {
		return ensureJournalMatchesInput(ctx, q, existing, input)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: post journal: check idempotency after lock: %w", err)
	}

	// Invariant: every balance-mutating tx must take pg_advisory_xact_lock(holder,
	// currency_id) for every affected (holder, currency_id) pair, in sorted order.
	// This serializes against ReserverStore.Reserve (which takes the same lock),
	// preventing TOCTOU races where a reserve reads stale balance while a journal
	// is being committed. Locks are taken in lexicographic (holder, currency_id)
	// order to avoid deadlocks when two journals touch overlapping pairs.
	if err := acquireBalanceLocks(ctx, q, balancePairsFromEntries(input.Entries)); err != nil {
		return nil, err
	}

	debit, credit := input.Totals()

	row, err := q.InsertJournal(ctx, sqlcgen.InsertJournalParams{
		JournalTypeID:  input.JournalTypeID,
		IdempotencyKey: input.IdempotencyKey,
		TotalDebit:     decimalToNumeric(debit),
		TotalCredit:    decimalToNumeric(credit),
		Metadata:       metadataToJSON(input.Metadata),
		ActorID:        input.ActorID,
		Source:         input.Source,
		ReversalOf:     int64ToInt8(zeroInt64ToNil(input.ReversalOf)),
		EventID:        input.EventID,
	})
	if err != nil {
		existing, lookupErr := q.GetJournalByIdempotencyKey(ctx, input.IdempotencyKey)
		if lookupErr == nil {
			return ensureJournalMatchesInput(ctx, q, existing, input)
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

	for i, e := range input.Entries {
		_, err := q.InsertJournalEntry(ctx, sqlcgen.InsertJournalEntryParams{
			JournalID:        row.ID,
			AccountHolder:    e.AccountHolder,
			CurrencyID:       e.CurrencyID,
			ClassificationID: e.ClassificationID,
			EntryType:        string(e.EntryType),
			Amount:           decimalToNumeric(e.Amount),
		})
		if err != nil {
			return nil, wrapStoreError(fmt.Sprintf("postgres: post journal: insert entry[%d]", i), err)
		}

		key := rollupKey{holder: e.AccountHolder, currencyID: e.CurrencyID, classificationID: e.ClassificationID}
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

	if input.EventID != 0 {
		if err := s.linkJournalToEventAndBooking(ctx, q, input.EventID, row.ID); err != nil {
			return nil, err
		}
	}

	return journalFromRow(row), nil
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
func (s *LedgerStore) GetBalance(ctx context.Context, holder int64, currencyID, classificationID int64) (decimal.Decimal, error) {
	ctx, span := ledgerotel.StartSpan(ctx, "ledger.ledger.get_balance",
		attribute.Int64("account_holder", holder),
		attribute.Int64("currency_id", currencyID),
		attribute.Int64("classification_id", classificationID),
	)
	defer span.End()

	if s.pool == nil {
		// Tx mode: use the caller's transaction directly — no inner tx.
		bal, err := s.getBalanceWithQueries(ctx, s.q, holder, currencyID, classificationID)
		ledgerotel.RecordError(span, err)
		return bal, err
	}

	// Pool mode: wrap in REPEATABLE READ to prevent phantom reads between the
	// checkpoint query and the entry-sum query.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		ledgerotel.RecordError(span, err)
		return decimal.Zero, fmt.Errorf("postgres: get balance: begin tx: %w", err)
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

// GetBalances returns balances across all classifications for a (holder, currency).
func (s *LedgerStore) GetBalances(ctx context.Context, holder int64, currencyID int64) ([]core.Balance, error) {
	// Discover all classifications that have entries for this account
	clsRows, err := s.q.DistinctClassificationsForAccount(ctx, sqlcgen.DistinctClassificationsForAccountParams{
		AccountHolder: holder,
		CurrencyID:    currencyID,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: get balances: list classifications: %w", err)
	}

	balances := make([]core.Balance, 0, len(clsRows))
	for _, clsID := range clsRows {
		bal, err := s.GetBalance(ctx, holder, currencyID, clsID)
		if err != nil {
			return nil, fmt.Errorf("postgres: get balances: classification %d: %w", clsID, err)
		}
		balances = append(balances, core.Balance{
			AccountHolder:    holder,
			CurrencyID:       currencyID,
			ClassificationID: clsID,
			Balance:          bal,
		})
	}

	return balances, nil
}

// BatchGetBalances returns balances for multiple holders.
func (s *LedgerStore) BatchGetBalances(ctx context.Context, holderIDs []int64, currencyID int64) (map[int64][]core.Balance, error) {
	result := make(map[int64][]core.Balance, len(holderIDs))
	for _, id := range holderIDs {
		bals, err := s.GetBalances(ctx, id, currencyID)
		if err != nil {
			return nil, fmt.Errorf("postgres: batch get balances: holder %d: %w", id, err)
		}
		result[id] = bals
	}
	return result, nil
}
