package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

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
		var balance decimal.Decimal
		if r.NormalSide == "debit" {
			balance = debit.Sub(credit)
		} else {
			balance = credit.Sub(debit)
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
			AccountHolder: r.AccountHolder,
			CurrencyID:    r.CurrencyID,
			Status:        r.Status,
			JournalID:     r.JournalID,
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
