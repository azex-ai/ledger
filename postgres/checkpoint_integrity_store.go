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
	"github.com/azex-ai/ledger/service"
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
		dims: dimCacheForTx(s.dims),
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
// check). See core.CheckpointIntegrityStore for the full contract, including
// why every call durably records itself in checkpoint_rebuilds.
func (s *CheckpointIntegrityStore) RebuildCheckpoint(ctx context.Context, holder int64, currencyUID, classificationUID string, actorID int64) (*core.BalanceCheckpoint, error) {
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
		cp, err := s.rebuildWithQueries(ctx, s.q, holder, cur.ID, cls.ID, actorID)
		if err != nil {
			return nil, err
		}
		return toCoreCheckpoint(cp, currencyUID, classificationUID), nil
	}

	// Pool mode: own the transaction lifecycle.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: rebuild checkpoint: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	qtx := s.q.WithTx(tx)
	cp, err := s.rebuildWithQueries(ctx, qtx, holder, cur.ID, cls.ID, actorID)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: rebuild checkpoint: commit: %w", err)
	}
	return toCoreCheckpoint(cp, currencyUID, classificationUID), nil
}

// toCoreCheckpoint translates rebuildWithQueries' internal, id-keyed result
// (service.BalanceCheckpoint) into the uid-based type
// CheckpointIntegrityStore.RebuildCheckpoint actually returns to library
// consumers (core.BalanceCheckpoint, I-18). The uids are the caller's own
// input, not a re-resolved value: RebuildCheckpoint already validated
// currencyUID/classificationUID resolve to cp's CurrencyID/ClassificationID
// before rebuildWithQueries ever ran, so no extra lookup is needed here.
func toCoreCheckpoint(cp *service.BalanceCheckpoint, currencyUID, classificationUID string) *core.BalanceCheckpoint {
	return &core.BalanceCheckpoint{
		AccountHolder:     cp.AccountHolder,
		CurrencyUID:       currencyUID,
		ClassificationUID: classificationUID,
		Balance:           cp.Balance,
		LastEntryAt:       cp.LastEntryAt,
	}
}

// rebuildWithQueries runs the lock + precondition + recompute + overwrite +
// audit sequence against the supplied queries handle, so pool mode (own tx)
// and tx mode (caller's tx) share one implementation. The audit insert
// shares the SAME transaction as the overwrite (both go through q), so a
// repair can never commit without its forensic record, and the record can
// never exist without the repair having happened.
func (s *CheckpointIntegrityStore) rebuildWithQueries(ctx context.Context, q *sqlcgen.Queries, holder, currencyID, classificationID, actorID int64) (*service.BalanceCheckpoint, error) {
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

	// Read the current (possibly poisoned) checkpoint BEFORE overwriting it —
	// this is the "previous" half of the audit record; a missing row (never
	// materialized yet) is not poisoning, so it defaults to the zero value
	// rather than erroring.
	previous, err := q.GetBalanceCheckpoint(ctx, sqlcgen.GetBalanceCheckpointParams{
		AccountHolder:    holder,
		CurrencyID:       currencyID,
		ClassificationID: classificationID,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: rebuild checkpoint: read previous: %w", err)
	}
	previousBalance := decimal.Zero
	var previousLastEntryID int64
	if err == nil {
		previousBalance, err = numericToDecimal(previous.Balance)
		if err != nil {
			return nil, fmt.Errorf("postgres: rebuild checkpoint: convert previous balance: %w", err)
		}
		previousLastEntryID = previous.LastEntryID
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

	// Durable, append-only audit record -- same transaction as the overwrite
	// above. drift matches ReconcileAccount's Detail.Drift sign convention:
	// actual (the checkpoint's claimed value) minus expected (the entries
	// truth). A non-zero row IS the forensic evidence a poisoned checkpoint
	// existed; see core.CheckpointIntegrityStore's doc comment.
	drift := previousBalance.Sub(balance)
	if err := q.InsertCheckpointRebuildAudit(ctx, sqlcgen.InsertCheckpointRebuildAuditParams{
		AccountHolder:       holder,
		CurrencyID:          currencyID,
		ClassificationID:    classificationID,
		PreviousBalance:     decimalToNumeric(previousBalance),
		PreviousLastEntryID: previousLastEntryID,
		NewBalance:          decimalToNumeric(balance),
		NewLastEntryID:      row.LastEntryID,
		Drift:               decimalToNumeric(drift),
		ActorID:             actorID,
	}); err != nil {
		return nil, fmt.Errorf("postgres: rebuild checkpoint: write audit: %w", err)
	}

	return &service.BalanceCheckpoint{
		AccountHolder:    holder,
		CurrencyID:       currencyID,
		ClassificationID: classificationID,
		Balance:          balance,
		LastEntryID:      row.LastEntryID,
		LastEntryAt:      lastEntryAt,
	}, nil
}
