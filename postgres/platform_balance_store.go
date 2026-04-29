package postgres

import (
	"context"
	"fmt"

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

// PlatformBalanceStore reads structured platform-wide balance breakdowns.
// It reads directly from balance_checkpoints (not system_rollups) so that
// user-side (holder > 0) and system-side (holder < 0) totals can be separated
// per classification. The queries are O(C) where C is the number of distinct
// active classifications — safe for frequent reads.
type PlatformBalanceStore struct {
	q *sqlcgen.Queries
}

// NewPlatformBalanceStore creates a new PlatformBalanceStore.
func NewPlatformBalanceStore(pool *pgxpool.Pool) *PlatformBalanceStore {
	return &PlatformBalanceStore{q: sqlcgen.New(pool)}
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

// GetTotalLiabilityByAsset returns the sum of all user-side (holder > 0)
// checkpoint balances for the given currency, across all classifications.
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
// Liability = sum of user-side (holder > 0) checkpoint balances.
// Custodial = sum of system-side (holder < 0) balances for code="custodial".
// Solvent   = Custodial >= Liability.
// Margin    = Custodial - Liability (positive = surplus, negative = shortfall).
//
// This reads the in-DB picture. Comparing the custodial figure to an off-chain
// custody position is the consumer's responsibility.
func (s *PlatformBalanceStore) SolvencyCheck(ctx context.Context, currencyID int64) (*core.SolvencyReport, error) {
	liabilityRaw, err := s.q.GetTotalUserSideBalance(ctx, currencyID)
	if err != nil {
		return nil, fmt.Errorf("postgres: platform balance: solvency liability currency=%d: %w", currencyID, err)
	}
	liability, err := numericToDecimal(liabilityRaw)
	if err != nil {
		return nil, fmt.Errorf("postgres: platform balance: solvency liability convert: %w", err)
	}

	custodialRaw, err := s.q.GetSystemSideCustodialBalance(ctx, currencyID)
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
