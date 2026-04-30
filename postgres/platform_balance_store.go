package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

// Compile-time interface assertions.
var (
	_ core.PlatformBalanceReader = (*PlatformBalanceStore)(nil)
	_ core.SolvencyChecker       = (*PlatformBalanceStore)(nil)
)

// PlatformBalanceStore reads structured platform-wide balance breakdowns in
// real time. Every query computes `checkpoint.balance + delta` where delta is
// the net of journal_entries past the checkpoint's last_entry_id, so reads
// reflect every committed write immediately — no waiting for the rollup
// worker.
//
// Single-statement queries (GetPlatformBalances, GetTotalLiabilityByAsset)
// rely on PostgreSQL statement-level snapshot consistency. Multi-statement
// reads (SolvencyCheck) wrap in REPEATABLE READ to keep the liability and
// custodial figures from drifting against each other.
type PlatformBalanceStore struct {
	pool *pgxpool.Pool
	db   DBTX
	q    *sqlcgen.Queries
}

// NewPlatformBalanceStore creates a new PlatformBalanceStore bound to a pool.
func NewPlatformBalanceStore(pool *pgxpool.Pool) *PlatformBalanceStore {
	return &PlatformBalanceStore{
		pool: pool,
		db:   pool,
		q:    sqlcgen.New(pool),
	}
}

// WithDB returns a clone bound to db (a *pgxpool.Pool or pgx.Tx). When passed
// a tx the store reads inside the caller's transaction and SolvencyCheck
// skips its own REPEATABLE READ wrap (the caller's isolation applies).
func (s *PlatformBalanceStore) WithDB(db DBTX) *PlatformBalanceStore {
	return &PlatformBalanceStore{
		pool: nil, // tx mode — disables inner BeginTx
		db:   db,
		q:    sqlcgen.New(db),
	}
}

// GetPlatformBalances returns a structured per-classification balance breakdown
// for the given currency. UserSide and SystemSide maps are keyed by
// classification code. Classifications with no checkpoints are absent from the
// maps (not present with a zero value).
func (s *PlatformBalanceStore) GetPlatformBalances(ctx context.Context, currencyID int64) (*core.PlatformBalance, error) {
	rows, err := s.q.GetPlatformBalancesByHolder(ctx, currencyID)
	if err != nil {
		return nil, fmt.Errorf("postgres: platform balance: get by holder: %w", err)
	}

	pb := &core.PlatformBalance{
		CurrencyID: currencyID,
		UserSide:   make(map[string]decimal.Decimal),
		SystemSide: make(map[string]decimal.Decimal),
	}

	for _, row := range rows {
		bal, err := numericToDecimal(row.TotalBalance)
		if err != nil {
			return nil, fmt.Errorf("postgres: platform balance: convert %s/%s: %w",
				row.ClassificationCode, row.HolderSide, err)
		}
		switch row.HolderSide {
		case "user":
			pb.UserSide[row.ClassificationCode] = bal
		case "system":
			pb.SystemSide[row.ClassificationCode] = bal
		}
	}

	return pb, nil
}

// GetTotalLiabilityByAsset returns the realtime sum of all user-side
// (holder > 0) balances for the given currency, across all classifications.
// This is the aggregate liability — what the platform owes users in total.
func (s *PlatformBalanceStore) GetTotalLiabilityByAsset(ctx context.Context, currencyID int64) (decimal.Decimal, error) {
	raw, err := s.q.GetTotalUserSideBalance(ctx, currencyID)
	if err != nil {
		return decimal.Zero, fmt.Errorf("postgres: platform balance: total liability currency=%d: %w", currencyID, err)
	}
	total, err := numericToDecimal(raw)
	if err != nil {
		return decimal.Zero, fmt.Errorf("postgres: platform balance: total liability convert: %w", err)
	}
	return total, nil
}

// SolvencyCheck computes a solvency report for the given currency.
//
// Liability = realtime sum of user-side (holder > 0) balances.
// Custodial = realtime sum of system-side (holder < 0) balances for code="custodial".
// Solvent   = Custodial >= Liability.
// Margin    = Custodial - Liability (positive = surplus, negative = shortfall).
//
// Both figures come from one REPEATABLE READ transaction so they describe a
// single point in time. Comparing the custodial figure to an off-chain custody
// position is the consumer's responsibility.
func (s *PlatformBalanceStore) SolvencyCheck(ctx context.Context, currencyID int64) (*core.SolvencyReport, error) {
	if s.pool == nil {
		// Tx mode: caller's transaction provides isolation; query directly.
		return s.solvencyCheckWithQueries(ctx, s.q, currencyID)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: platform balance: solvency: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlcgen.New(tx)
	report, err := s.solvencyCheckWithQueries(ctx, q, currencyID)
	if err != nil {
		return nil, err
	}
	return report, nil
}

func (s *PlatformBalanceStore) solvencyCheckWithQueries(ctx context.Context, q *sqlcgen.Queries, currencyID int64) (*core.SolvencyReport, error) {
	liabilityRaw, err := q.GetTotalUserSideBalance(ctx, currencyID)
	if err != nil {
		return nil, fmt.Errorf("postgres: platform balance: solvency liability currency=%d: %w", currencyID, err)
	}
	liability, err := numericToDecimal(liabilityRaw)
	if err != nil {
		return nil, fmt.Errorf("postgres: platform balance: solvency liability convert: %w", err)
	}

	custodialRaw, err := q.GetSystemSideCustodialBalance(ctx, currencyID)
	if err != nil {
		return nil, fmt.Errorf("postgres: platform balance: solvency custodial currency=%d: %w", currencyID, err)
	}
	custodial, err := numericToDecimal(custodialRaw)
	if err != nil {
		return nil, fmt.Errorf("postgres: platform balance: solvency custodial convert: %w", err)
	}

	margin := custodial.Sub(liability)
	return &core.SolvencyReport{
		CurrencyID: currencyID,
		Liability:  liability,
		Custodial:  custodial,
		Solvent:    custodial.GreaterThanOrEqual(liability),
		Margin:     margin,
	}, nil
}
