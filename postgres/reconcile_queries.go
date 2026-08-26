package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
	"github.com/azex-ai/ledger/service"
)

// Compile-time assertion.
var _ service.ReconcileQuerier = (*ReconcileAdapter)(nil)

// ReconcileAdapter wraps the sqlcgen reconcile queries behind the
// service.ReconcileQuerier interface.
type ReconcileAdapter struct {
	q *sqlcgen.Queries
}

// NewReconcileAdapter creates a ReconcileAdapter backed by a connection pool.
func NewReconcileAdapter(pool *pgxpool.Pool) *ReconcileAdapter {
	return &ReconcileAdapter{q: sqlcgen.New(pool)}
}

// WithDB returns a transaction-bound clone.
func (a *ReconcileAdapter) WithDB(db DBTX) *ReconcileAdapter {
	return &ReconcileAdapter{q: sqlcgen.New(db)}
}

// OrphanEntriesCount returns the number of journal_entries whose journal_id
// does not resolve to any row in the journals table.
func (a *ReconcileAdapter) OrphanEntriesCount(ctx context.Context) (int64, error) {
	n, err := a.q.ReconcileOrphanEntriesCount(ctx)
	if err != nil {
		return 0, fmt.Errorf("postgres: reconcile: orphan entries count: %w", err)
	}
	return n, nil
}

// OrphanEntriesSample returns up to 10 (entry_id, journal_id) pairs for
// orphan entries, for use in Finding descriptions.
func (a *ReconcileAdapter) OrphanEntriesSample(ctx context.Context) ([]service.OrphanEntrySample, error) {
	rows, err := a.q.ReconcileOrphanEntriesSample(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: reconcile: orphan entries sample: %w", err)
	}
	result := make([]service.OrphanEntrySample, len(rows))
	for i, r := range rows {
		result[i] = service.OrphanEntrySample{EntryID: r.EntryID, JournalID: r.JournalID}
	}
	return result, nil
}

// AccountingEquationRows returns per-(currency_id, classification_id) debit/credit
// totals along with the classification's normal_side.
func (a *ReconcileAdapter) AccountingEquationRows(ctx context.Context) ([]service.AccountingEquationRow, error) {
	rows, err := a.q.ReconcileAccountingEquation(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: reconcile: accounting equation: %w", err)
	}
	result := make([]service.AccountingEquationRow, len(rows))
	for i, r := range rows {
		debit, err := numericToDecimal(r.TotalDebit)
		if err != nil {
			return nil, fmt.Errorf("postgres: reconcile: accounting equation: debit convert: %w", err)
		}
		credit, err := numericToDecimal(r.TotalCredit)
		if err != nil {
			return nil, fmt.Errorf("postgres: reconcile: accounting equation: credit convert: %w", err)
		}
		result[i] = service.AccountingEquationRow{
			CurrencyID:       r.CurrencyID,
			ClassificationID: r.ClassificationID,
			NormalSide:       r.NormalSide,
			TotalDebit:       debit,
			TotalCredit:      credit,
		}
	}
	return result, nil
}

// SettlementNettingViolations returns per-currency net balances for the named
// settlement classification that are non-zero outside the given time window.
func (a *ReconcileAdapter) SettlementNettingViolations(ctx context.Context, classCode string, windowMinutes int) ([]service.SettlementNettingViolation, error) {
	rows, err := a.q.ReconcileSettlementNetting(ctx, sqlcgen.ReconcileSettlementNettingParams{
		ClassificationCode: classCode,
		WindowMinutes:      int32(windowMinutes), //nolint:gosec // minutes fit in int32
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: reconcile: settlement netting: %w", err)
	}
	result := make([]service.SettlementNettingViolation, len(rows))
	for i, r := range rows {
		net, err := numericToDecimal(r.NetBalance)
		if err != nil {
			return nil, fmt.Errorf("postgres: reconcile: settlement netting: convert: %w", err)
		}
		result[i] = service.SettlementNettingViolation{CurrencyID: r.CurrencyID, NetBalance: net}
	}
	return result, nil
}

// NegativeBalanceAccounts returns user accounts (holder > 0) with a negative
// computed balance, up to pageLimit rows.
func (a *ReconcileAdapter) NegativeBalanceAccounts(ctx context.Context, pageLimit int) ([]service.NegativeBalanceAccount, error) {
	rows, err := a.q.ReconcileNonNegativeBalances(ctx, int32(pageLimit)) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("postgres: reconcile: non-negative balances: %w", err)
	}
	result := make([]service.NegativeBalanceAccount, len(rows))
	for i, r := range rows {
		debit, err := numericToDecimal(r.TotalDebit)
		if err != nil {
			return nil, fmt.Errorf("postgres: reconcile: non-negative: debit convert: %w", err)
		}
		credit, err := numericToDecimal(r.TotalCredit)
		if err != nil {
			return nil, fmt.Errorf("postgres: reconcile: non-negative: credit convert: %w", err)
		}
		// core.Delta is the sole authority for this computation (I-42). This
		// used to default to credit-normal for any r.NormalSide != "debit";
		// core.Delta refuses an unrecognized normal_side instead.
		balance, err := core.Delta(core.NormalSide(r.NormalSide), debit, credit)
		if err != nil {
			return nil, fmt.Errorf("postgres: reconcile: non-negative: classification %d: %w", r.ClassificationID, err)
		}
		result[i] = service.NegativeBalanceAccount{
			AccountHolder:    r.AccountHolder,
			CurrencyID:       r.CurrencyID,
			ClassificationID: r.ClassificationID,
			NormalSide:       r.NormalSide,
			Balance:          balance,
		}
	}
	return result, nil
}

// OrphanReservations returns reservations whose journal_id (non-zero) does not
// resolve to any journals row.
func (a *ReconcileAdapter) OrphanReservations(ctx context.Context) ([]service.OrphanReservation, error) {
	rows, err := a.q.ReconcileOrphanReservations(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: reconcile: orphan reservations: %w", err)
	}
	result := make([]service.OrphanReservation, len(rows))
	for i, r := range rows {
		result[i] = service.OrphanReservation{
			ID:            r.ID,
			UID:           pgToUID(r.Uid),
			AccountHolder: r.AccountHolder,
			CurrencyID:    r.CurrencyID,
			Status:        r.Status,
			JournalID:     r.JournalID.Int64,
		}
	}
	return result, nil
}

// StaleRollupItems returns rollup_queue items whose claimed_until lease has
// expired by more than thresholdMinutes, indicating a stuck worker.
func (a *ReconcileAdapter) StaleRollupItems(ctx context.Context, thresholdMinutes int) ([]service.StaleRollupItem, error) {
	rows, err := a.q.ReconcileStaleRollupItems(ctx, int32(thresholdMinutes)) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("postgres: reconcile: stale rollup items: %w", err)
	}
	result := make([]service.StaleRollupItem, len(rows))
	for i, r := range rows {
		var claimedUntil string
		if r.ClaimedUntil.Valid {
			claimedUntil = r.ClaimedUntil.Time.Format("2006-01-02T15:04:05Z")
		}
		result[i] = service.StaleRollupItem{
			ID:               r.ID,
			AccountHolder:    r.AccountHolder,
			CurrencyID:       r.CurrencyID,
			ClassificationID: r.ClassificationID,
			ClaimedUntil:     claimedUntil,
			FailedAttempts:   int(r.FailedAttempts),
		}
	}
	return result, nil
}

// ListCheckpointAccountsPage returns a keyset-paginated page of distinct
// (account_holder, currency_id) pairs that have at least one row in
// balance_checkpoints, ordered by (account_holder, currency_id). Pass (0, 0)
// as (afterHolder, afterCurrency) for the first page.
func (a *ReconcileAdapter) ListCheckpointAccountsPage(ctx context.Context, afterHolder, afterCurrency int64, pageLimit int) ([]service.CheckpointAccountKey, error) {
	rows, err := a.q.ReconcileListCheckpointAccountsPage(ctx, sqlcgen.ReconcileListCheckpointAccountsPageParams{
		AfterHolder:   afterHolder,
		AfterCurrency: afterCurrency,
		PageLimit:     int32(pageLimit), //nolint:gosec // page sizes are small, bounded internally
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: reconcile: list checkpoint accounts page: %w", err)
	}
	result := make([]service.CheckpointAccountKey, len(rows))
	for i, r := range rows {
		result[i] = service.CheckpointAccountKey{AccountHolder: r.AccountHolder, CurrencyID: r.CurrencyID}
	}
	return result, nil
}

// UnbalancedJournalsCount returns the number of (journal_id, currency_id)
// pairs whose entries do not net to zero -- a genuine per-journal balance
// violation (M1 fix; distinct from the global debit==credit equality check).
// See queries/integrity_balance.sql.
func (a *ReconcileAdapter) UnbalancedJournalsCount(ctx context.Context) (int64, error) {
	n, err := a.q.IntegrityUnbalancedJournalsCount(ctx)
	if err != nil {
		return 0, fmt.Errorf("postgres: reconcile: unbalanced journals count: %w", err)
	}
	return n, nil
}

// UnbalancedJournalsSample returns up to 20 (journal_id, currency_id, drift)
// rows for the journals found by UnbalancedJournalsCount, for Finding
// descriptions.
func (a *ReconcileAdapter) UnbalancedJournalsSample(ctx context.Context) ([]service.UnbalancedJournal, error) {
	rows, err := a.q.IntegrityUnbalancedJournalsSample(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: reconcile: unbalanced journals sample: %w", err)
	}
	result := make([]service.UnbalancedJournal, len(rows))
	for i, r := range rows {
		drift, err := numericToDecimal(r.Drift)
		if err != nil {
			return nil, fmt.Errorf("postgres: reconcile: unbalanced journals sample: drift convert: %w", err)
		}
		result[i] = service.UnbalancedJournal{JournalID: r.JournalID, CurrencyID: r.CurrencyID, Drift: drift}
	}
	return result, nil
}

// GetScanCursor returns the persisted resume cursor for the named check
// (C4b). Zero rows (no cursor persisted yet) is a normal "first run" state:
// it maps to (cursorStartHolder-equivalent MinInt64, MinInt64, lapDirty=false),
// matching the in-memory default check #2 always used before this table
// existed -- NOT (0, 0), which would exclude every negative (system) holder
// from the very first page (docs/bugs/2026-08-21-reconcile-coverage-blind-spots.md, B1).
func (a *ReconcileAdapter) GetScanCursor(ctx context.Context, checkName string) (afterHolder, afterCurrency int64, lapDirty bool, err error) {
	row, err := a.q.GetReconcileScanCursor(ctx, checkName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return math.MinInt64, math.MinInt64, false, nil
		}
		return 0, 0, false, fmt.Errorf("postgres: reconcile: get scan cursor: %w", err)
	}
	return row.AfterHolder, row.AfterCurrency, row.LapDirty, nil
}

// SetScanCursor persists the resume cursor and lap_dirty flag for the named
// check.
func (a *ReconcileAdapter) SetScanCursor(ctx context.Context, checkName string, afterHolder, afterCurrency int64, lapDirty bool) error {
	if err := a.q.UpsertReconcileScanCursor(ctx, sqlcgen.UpsertReconcileScanCursorParams{
		CheckName:     checkName,
		AfterHolder:   afterHolder,
		AfterCurrency: afterCurrency,
		LapDirty:      lapDirty,
	}); err != nil {
		return fmt.Errorf("postgres: reconcile: set scan cursor: %w", err)
	}
	return nil
}

// ListSystemRollupsRaw returns every system_rollups row in internal-id space,
// for the system_rollup_integrity check's entries-based comparison (M4/I-23).
func (a *ReconcileAdapter) ListSystemRollupsRaw(ctx context.Context) ([]service.SystemRollupRow, error) {
	rows, err := a.q.ListSystemRollupsRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: reconcile: list system rollups: %w", err)
	}
	result := make([]service.SystemRollupRow, len(rows))
	for i, r := range rows {
		total, err := numericToDecimal(r.TotalBalance)
		if err != nil {
			return nil, fmt.Errorf("postgres: reconcile: list system rollups: convert: %w", err)
		}
		result[i] = service.SystemRollupRow{
			CurrencyID:       r.CurrencyID,
			ClassificationID: r.ClassificationID,
			TotalBalance:     total,
		}
	}
	return result, nil
}

// LatestSnapshotDrift returns balance_snapshots rows for the most recent
// snapshot_date whose stored balance disagrees with a fresh entries-based
// recompute as of that date, up to pageLimit rows (the snapshot_integrity
// check, M4/I-23).
func (a *ReconcileAdapter) LatestSnapshotDrift(ctx context.Context, pageLimit int) ([]service.SnapshotDriftRow, error) {
	rows, err := a.q.ReconcileLatestSnapshotDrift(ctx, int32(pageLimit)) //nolint:gosec // page limits are small, bounded internally
	if err != nil {
		return nil, fmt.Errorf("postgres: reconcile: latest snapshot drift: %w", err)
	}
	result := make([]service.SnapshotDriftRow, len(rows))
	for i, r := range rows {
		stored, err := numericToDecimal(r.StoredBalance)
		if err != nil {
			return nil, fmt.Errorf("postgres: reconcile: latest snapshot drift: convert stored: %w", err)
		}
		recomputed, err := numericToDecimal(r.RecomputedBalance)
		if err != nil {
			return nil, fmt.Errorf("postgres: reconcile: latest snapshot drift: convert recomputed: %w", err)
		}
		result[i] = service.SnapshotDriftRow{
			AccountHolder:     r.AccountHolder,
			CurrencyID:        r.CurrencyID,
			ClassificationID:  r.ClassificationID,
			SnapshotDate:      r.SnapshotDate.Time,
			StoredBalance:     stored,
			RecomputedBalance: recomputed,
		}
	}
	return result, nil
}

// DuplicateIdempotencyKeys returns journals that share an idempotency_key with
// at least one other journal (should be empty given the UNIQUE index).
func (a *ReconcileAdapter) DuplicateIdempotencyKeys(ctx context.Context) ([]service.DuplicateIdempotencyKey, error) {
	rows, err := a.q.ReconcileDuplicateIdempotencyKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: reconcile: duplicate idempotency keys: %w", err)
	}
	result := make([]service.DuplicateIdempotencyKey, len(rows))
	for i, r := range rows {
		result[i] = service.DuplicateIdempotencyKey{
			IdempotencyKey: r.IdempotencyKey,
			Occurrences:    r.Occurrences,
			FirstID:        r.FirstID,
			LastID:         r.LastID,
		}
	}
	return result, nil
}
