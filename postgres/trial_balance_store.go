package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

// trialBalanceQueryTimeout bounds the wall-clock time spent on the
// full-table aggregation query, mirroring the timeout-protection pattern used
// by the reconcile suite's check #2 (service.FullReconciliationConfig.Check2Timeout).
// A large ledger without daily-snapshot acceleration (see design doc §7
// backlog) could otherwise run an unbounded scan.
const trialBalanceQueryTimeout = 30 * time.Second

var _ core.TrialBalanceReader = (*TrialBalanceStore)(nil)

// TrialBalanceStore implements core.TrialBalanceReader using PostgreSQL.
type TrialBalanceStore struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
	dims *dimCache
}

// NewTrialBalanceStore creates a new TrialBalanceStore.
func NewTrialBalanceStore(pool *pgxpool.Pool) *TrialBalanceStore {
	return &TrialBalanceStore{
		pool: pool,
		q:    sqlcgen.New(pool),
		dims: dimCacheFor(pool),
	}
}

// WithDB returns a clone of the TrialBalanceStore bound to an existing transaction.
func (s *TrialBalanceStore) WithDB(db DBTX) *TrialBalanceStore {
	return &TrialBalanceStore{
		pool: nil, // tx mode
		q:    sqlcgen.New(db),
		dims: s.dims,
	}
}

// TrialBalance aggregates per-classification debit/credit totals for
// currencyID as of asOf (inclusive), and reports whether the ledger balances
// globally for that currency and cutoff.
func (s *TrialBalanceStore) TrialBalance(ctx context.Context, currencyUID string, asOf time.Time) (*core.TrialBalanceReport, error) {
	ctx, cancel := context.WithTimeout(ctx, trialBalanceQueryTimeout)
	defer cancel()

	cur, err := s.dims.currencyByUIDOrErr(ctx, s.q, currencyUID)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.TrialBalanceRows(ctx, sqlcgen.TrialBalanceRowsParams{
		CurrencyID:  cur.ID,
		EffectiveAt: asOf,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: trial balance: %w", err)
	}

	report := &core.TrialBalanceReport{
		CurrencyUID: currencyUID,
		AsOf:        asOf,
		Rows:        make([]core.TrialBalanceRow, 0, len(rows)),
	}

	for _, row := range rows {
		debit := mustNumericToDecimal(row.TotalDebit)
		credit := mustNumericToDecimal(row.TotalCredit)
		normalSide := core.NormalSide(row.NormalSide)

		net := debit.Sub(credit)
		if normalSide == core.NormalSideCredit {
			net = credit.Sub(debit)
		}

		cls, err := s.dims.classByIDOrErr(ctx, s.q, row.ClassificationID)
		if err != nil {
			return nil, err
		}
		report.Rows = append(report.Rows, core.TrialBalanceRow{
			ClassificationUID:  cls.UID,
			ClassificationCode: row.ClassificationCode,
			ClassificationName: row.ClassificationName,
			NormalSide:         normalSide,
			TotalDebit:         debit,
			TotalCredit:        credit,
			Net:                net,
		})
		report.TotalDebit = report.TotalDebit.Add(debit)
		report.TotalCredit = report.TotalCredit.Add(credit)
	}
	report.Balanced = report.TotalDebit.Equal(report.TotalCredit)

	return report, nil
}
