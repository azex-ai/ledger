package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

// Compile-time interface assertions.
var (
	_ core.JournalWriter = (*LedgerStore)(nil)
	_ core.BalanceReader  = (*LedgerStore)(nil)
)

// LedgerStore implements JournalWriter and BalanceReader using PostgreSQL.
type LedgerStore struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

// NewLedgerStore creates a new LedgerStore.
func NewLedgerStore(pool *pgxpool.Pool) *LedgerStore {
	return &LedgerStore{
		pool: pool,
		q:    sqlcgen.New(pool),
	}
}

// PostJournal posts a balanced journal within a transaction.
// Idempotent: returns existing journal if idempotency_key already exists.
func (s *LedgerStore) PostJournal(ctx context.Context, input core.JournalInput) (*core.Journal, error) {
	if err := input.Validate(); err != nil {
		return nil, fmt.Errorf("postgres: post journal: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: post journal: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)

	// Check idempotency
	existing, err := qtx.GetJournalByIdempotencyKey(ctx, input.IdempotencyKey)
	if err == nil {
		// Already exists — return it
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("postgres: post journal: commit idempotent: %w", err)
		}
		return journalFromRow(existing), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: post journal: check idempotency: %w", err)
	}

	debit, credit := input.Totals()

	row, err := qtx.InsertJournal(ctx, sqlcgen.InsertJournalParams{
		JournalTypeID:  input.JournalTypeID,
		IdempotencyKey: input.IdempotencyKey,
		TotalDebit:     decimalToNumeric(debit),
		TotalCredit:    decimalToNumeric(credit),
		Metadata:       metadataToJSON(input.Metadata),
		ActorID:        int64ToInt8(input.ActorID),
		Source:         stringToText(input.Source),
		ReversalOf:     int64ToInt8(input.ReversalOf),
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: post journal: insert journal: %w", err)
	}

	// Track unique (holder, currency, classification) for rollup enqueue
	type rollupKey struct {
		holder         int64
		currencyID     int64
		classificationID int64
	}
	seen := make(map[rollupKey]struct{})

	for i, e := range input.Entries {
		_, err := qtx.InsertJournalEntry(ctx, sqlcgen.InsertJournalEntryParams{
			JournalID:        row.ID,
			AccountHolder:    e.AccountHolder,
			CurrencyID:       e.CurrencyID,
			ClassificationID: e.ClassificationID,
			EntryType:        string(e.EntryType),
			Amount:           decimalToNumeric(e.Amount),
		})
		if err != nil {
			return nil, fmt.Errorf("postgres: post journal: insert entry[%d]: %w", i, err)
		}

		key := rollupKey{e.AccountHolder, e.CurrencyID, e.ClassificationID}
		seen[key] = struct{}{}
	}

	// Enqueue rollup for each unique dimension
	for key := range seen {
		if err := qtx.EnqueueRollup(ctx, sqlcgen.EnqueueRollupParams{
			AccountHolder:    key.holder,
			CurrencyID:       key.currencyID,
			ClassificationID: key.classificationID,
		}); err != nil {
			return nil, fmt.Errorf("postgres: post journal: enqueue rollup: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: post journal: commit: %w", err)
	}

	return journalFromRow(row), nil
}

// ExecuteTemplate loads a template by code, renders it, and posts the journal.
func (s *LedgerStore) ExecuteTemplate(ctx context.Context, templateCode string, params core.TemplateParams) (*core.Journal, error) {
	tmplRow, err := s.q.GetTemplateByCode(ctx, templateCode)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: execute template: template %q not found", templateCode)
		}
		return nil, fmt.Errorf("postgres: execute template: get template: %w", err)
	}

	lines, err := s.q.GetTemplateLines(ctx, tmplRow.ID)
	if err != nil {
		return nil, fmt.Errorf("postgres: execute template: get lines: %w", err)
	}

	tmpl := templateFromRow(tmplRow, lines)
	input, err := tmpl.Render(params)
	if err != nil {
		return nil, fmt.Errorf("postgres: execute template: render: %w", err)
	}

	return s.PostJournal(ctx, *input)
}

// ReverseJournal creates a reversal journal for the given journal ID.
func (s *LedgerStore) ReverseJournal(ctx context.Context, journalID int64, reason string) (*core.Journal, error) {
	original, err := s.q.GetJournal(ctx, journalID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: reverse journal: journal %d not found", journalID)
		}
		return nil, fmt.Errorf("postgres: reverse journal: get journal: %w", err)
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

	reversalOf := journalID
	input := core.JournalInput{
		JournalTypeID:  original.JournalTypeID,
		IdempotencyKey: fmt.Sprintf("reversal:%d:%s", journalID, reason),
		Entries:        reversedEntries,
		Source:         "reversal",
		ReversalOf:     &reversalOf,
		Metadata:       map[string]string{"reason": reason},
	}

	return s.PostJournal(ctx, input)
}

// GetBalance computes balance for a single (holder, currency, classification) dimension.
// Balance = checkpoint.balance + delta (entries since checkpoint).
// Delta computation respects normal_side of the classification.
func (s *LedgerStore) GetBalance(ctx context.Context, holder int64, currencyID, classificationID int64) (decimal.Decimal, error) {
	// Get checkpoint (may not exist yet)
	var checkpointBalance decimal.Decimal
	var sinceEntryID int64

	cp, err := s.q.GetBalanceCheckpoint(ctx, sqlcgen.GetBalanceCheckpointParams{
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
	sums, err := s.q.SumEntriesSinceCheckpoint(ctx, sqlcgen.SumEntriesSinceCheckpointParams{
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
	cls, err := s.q.ListClassifications(ctx, false)
	if err != nil {
		return decimal.Zero, fmt.Errorf("postgres: get balance: list classifications: %w", err)
	}
	var normalSide core.NormalSide
	for _, c := range cls {
		if c.ID == classificationID {
			normalSide = core.NormalSide(c.NormalSide)
			break
		}
	}

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
	// Get all checkpoints for this holder+currency
	checkpoints, err := s.q.GetBalanceCheckpoints(ctx, sqlcgen.GetBalanceCheckpointsParams{
		AccountHolder: holder,
		CurrencyID:    currencyID,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: get balances: checkpoints: %w", err)
	}

	// Find the minimum sinceEntryID across all checkpoints (or 0)
	cpMap := make(map[int64]decimal.Decimal) // classificationID -> balance
	var minEntryID int64
	for _, cp := range checkpoints {
		bal := mustNumericToDecimal(cp.Balance)
		cpMap[cp.ClassificationID] = bal
		if minEntryID == 0 || cp.LastEntryID < minEntryID {
			minEntryID = cp.LastEntryID
		}
	}

	// Get entry sums since the earliest checkpoint
	sums, err := s.q.SumEntriesSinceCheckpoint(ctx, sqlcgen.SumEntriesSinceCheckpointParams{
		AccountHolder: holder,
		CurrencyID:    currencyID,
		SinceEntryID:  minEntryID,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: get balances: sum entries: %w", err)
	}

	// Get classifications for normal_side
	allCls, err := s.q.ListClassifications(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("postgres: get balances: classifications: %w", err)
	}
	normalSides := make(map[int64]core.NormalSide)
	for _, c := range allCls {
		normalSides[c.ID] = core.NormalSide(c.NormalSide)
	}

	// Build delta map: classificationID -> (debitSum, creditSum)
	type sumPair struct{ debit, credit decimal.Decimal }
	deltaMap := make(map[int64]sumPair)
	for _, row := range sums {
		amount, err := anyToDecimal(row.Total)
		if err != nil {
			return nil, fmt.Errorf("postgres: get balances: convert: %w", err)
		}
		sp := deltaMap[row.ClassificationID]
		switch core.EntryType(row.EntryType) {
		case core.EntryTypeDebit:
			sp.debit = sp.debit.Add(amount)
		case core.EntryTypeCredit:
			sp.credit = sp.credit.Add(amount)
		}
		deltaMap[row.ClassificationID] = sp
	}

	// Collect all classification IDs
	clsIDs := make(map[int64]struct{})
	for id := range cpMap {
		clsIDs[id] = struct{}{}
	}
	for id := range deltaMap {
		clsIDs[id] = struct{}{}
	}

	var balances []core.Balance
	for clsID := range clsIDs {
		checkpoint := cpMap[clsID]
		sp := deltaMap[clsID]
		ns := normalSides[clsID]
		var delta decimal.Decimal
		switch ns {
		case core.NormalSideDebit:
			delta = sp.debit.Sub(sp.credit)
		case core.NormalSideCredit:
			delta = sp.credit.Sub(sp.debit)
		default:
			delta = sp.debit.Sub(sp.credit)
		}
		balances = append(balances, core.Balance{
			AccountHolder:    holder,
			CurrencyID:       currencyID,
			ClassificationID: clsID,
			Balance:          checkpoint.Add(delta),
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
