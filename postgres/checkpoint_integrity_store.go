package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

var _ core.CheckpointIntegrityStore = (*CheckpointIntegrityStore)(nil)

// CheckpointIntegrityStore implements core.CheckpointIntegrityStore: trusted,
// entries-only balance operations that never consult balance_checkpoints
// (docs/plans/2026-08-21-tamper-evident-ledger-design.md §4, I-23).
//
// In pool mode (constructed via NewCheckpointIntegrityStore), RebuildCheckpoint
// starts its own transaction. In tx mode (bound via WithDB), it participates
// in the caller's transaction — commit/rollback is the caller's
// responsibility, matching ReserverStore's convention.
type CheckpointIntegrityStore struct {
	// pool is non-nil only in pool mode; nil signals tx mode.
	pool *pgxpool.Pool
	db   DBTX
	q    *sqlcgen.Queries
	dims *dimCache
}

// NewCheckpointIntegrityStore creates a CheckpointIntegrityStore backed by a
// connection pool.
func NewCheckpointIntegrityStore(pool *pgxpool.Pool) *CheckpointIntegrityStore {
	return &CheckpointIntegrityStore{
		pool: pool,
		db:   pool,
		q:    sqlcgen.New(pool),
		dims: dimCacheFor(pool),
	}
}

// WithDB returns a clone of the store bound to an existing transaction.
func (s *CheckpointIntegrityStore) WithDB(db DBTX) *CheckpointIntegrityStore {
	return &CheckpointIntegrityStore{
		dims: s.dims,
		pool: nil, // tx mode
		db:   db,
		q:    sqlcgen.New(db),
	}
}

// RecomputeBalance ignores balance_checkpoints entirely and sums every
// journal_entries row for the dimension from entry 0. See
// core.CheckpointIntegrityStore for why callers on the withdrawal /
// large-amount path must use this instead of BalanceReader.GetBalance.
func (s *CheckpointIntegrityStore) RecomputeBalance(ctx context.Context, holder int64, currencyUID, classificationUID string) (decimal.Decimal, error) {
	cur, err := s.dims.currencyByUIDOrErr(ctx, s.q, currencyUID)
	if err != nil {
		return decimal.Zero, err
	}
	cls, err := s.dims.classByUIDOrErr(ctx, s.q, classificationUID)
	if err != nil {
		return decimal.Zero, err
	}

	row, err := s.q.RecomputeCheckpointFromEntries(ctx, sqlcgen.RecomputeCheckpointFromEntriesParams{
		AccountHolder:    holder,
		CurrencyID:       cur.ID,
		ClassificationID: cls.ID,
	})
	if err != nil {
		return decimal.Zero, fmt.Errorf("postgres: recompute balance: %w", err)
	}
	balance, err := numericToDecimal(row.Balance)
	if err != nil {
		return decimal.Zero, fmt.Errorf("postgres: recompute balance: convert: %w", err)
	}
	return balance, nil
}

// RebuildCheckpoint is the trusted operator entry point that repairs a
// checkpoint already found to have drifted (reconcile's checkpoint_balance
// check). See core.CheckpointIntegrityStore for the full contract.
func (s *CheckpointIntegrityStore) RebuildCheckpoint(ctx context.Context, holder int64, currencyUID, classificationUID string) (*core.BalanceCheckpoint, error) {
	cur, err := s.dims.currencyByUIDOrErr(ctx, s.q, currencyUID)
	if err != nil {
		return nil, err
	}
	cls, err := s.dims.classByUIDOrErr(ctx, s.q, classificationUID)
	if err != nil {
		return nil, err
	}

	if s.pool == nil {
		// Tx mode: participate in the caller's transaction directly.
		return s.rebuildWithQueries(ctx, s.q, holder, cur.ID, cls.ID)
	}

	// Pool mode: own the transaction lifecycle.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: rebuild checkpoint: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	qtx := s.q.WithTx(tx)
	cp, err := s.rebuildWithQueries(ctx, qtx, holder, cur.ID, cls.ID)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: rebuild checkpoint: commit: %w", err)
	}
	return cp, nil
}

// rebuildWithQueries runs the lock + precondition + recompute + overwrite
// sequence against the supplied queries handle, so pool mode (own tx) and tx
// mode (caller's tx) share one implementation.
func (s *CheckpointIntegrityStore) rebuildWithQueries(ctx context.Context, q *sqlcgen.Queries, holder, currencyID, classificationID int64) (*core.BalanceCheckpoint, error) {
	// Lock the (holder, currency_id) dimension: the same advisory-lock key
	// space PostJournal and Reserve take before mutating balance state (see
	// acquireBalanceLocks), so no concurrent journal post or reserve for this
	// pair can commit while the rebuild is in flight.
	if err := acquireBalanceLocks(ctx, q, []balancePair{{holder: holder, currencyID: currencyID}}); err != nil {
		return nil, fmt.Errorf("postgres: rebuild checkpoint: %w", err)
	}

	// Refuse if a rollup_queue item is still pending/claimed for this exact
	// dimension: a worker may already hold the (possibly poisoned) checkpoint
	// in memory and would otherwise re-clobber the fix with
	// poisoned-base-plus-delta the moment its write lands. This advisory lock
	// does not exclude the rollup worker (it never takes this lock space), so
	// the precondition is the actual guard here.
	pending, err := q.CountPendingRollupForDimension(ctx, sqlcgen.CountPendingRollupForDimensionParams{
		AccountHolder:    holder,
		CurrencyID:       currencyID,
		ClassificationID: classificationID,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: rebuild checkpoint: count pending rollup: %w", err)
	}
	if pending > 0 {
		return nil, fmt.Errorf("postgres: rebuild checkpoint: holder=%d currency_id=%d classification_id=%d: %w",
			holder, currencyID, classificationID, core.ErrRollupPending)
	}

	row, err := q.RecomputeCheckpointFromEntries(ctx, sqlcgen.RecomputeCheckpointFromEntriesParams{
		AccountHolder:    holder,
		CurrencyID:       currencyID,
		ClassificationID: classificationID,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: rebuild checkpoint: recompute: %w", err)
	}
	balance, err := numericToDecimal(row.Balance)
	if err != nil {
		return nil, fmt.Errorf("postgres: rebuild checkpoint: convert balance: %w", err)
	}
	lastEntryAt, err := anyToTime(row.LastEntryAt)
	if err != nil {
		return nil, fmt.Errorf("postgres: rebuild checkpoint: convert last_entry_at: %w", err)
	}

	// Unconditional overwrite (RebuildBalanceCheckpoint has no monotonic
	// guard) -- see that query's doc comment for why UpsertBalanceCheckpoint
	// cannot be reused here.
	if err := q.RebuildBalanceCheckpoint(ctx, sqlcgen.RebuildBalanceCheckpointParams{
		AccountHolder:    holder,
		CurrencyID:       currencyID,
		ClassificationID: classificationID,
		Balance:          decimalToNumeric(balance),
		LastEntryID:      row.LastEntryID,
		LastEntryAt:      lastEntryAt,
	}); err != nil {
		return nil, fmt.Errorf("postgres: rebuild checkpoint: write: %w", err)
	}

	return &core.BalanceCheckpoint{
		AccountHolder:    holder,
		CurrencyID:       currencyID,
		ClassificationID: classificationID,
		Balance:          balance,
		LastEntryID:      row.LastEntryID,
		LastEntryAt:      lastEntryAt,
	}, nil
}
