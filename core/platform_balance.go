package core

import (
	"context"

	"github.com/shopspring/decimal"
)

// PlatformBalance is a structured per-currency breakdown of system-wide balances
// read from the system_rollups materialized view.
//
// UserSide contains totals for accounts with positive holder IDs (holder > 0),
// keyed by classification code.
//
// SystemSide contains totals for accounts with negative holder IDs (holder < 0),
// keyed by classification code.
//
// Amounts are the sum of balance_checkpoints for each (currency, classification)
// group, as last refreshed by RefreshSystemRollups.
//
// Note: system_rollups does NOT distinguish holder sign — it aggregates all
// holders for a given (currency_id, classification_id). The split into UserSide
// vs SystemSide is performed at query time using separate SQL predicates.
type PlatformBalance struct {
	CurrencyID int64                      `json:"currency_id"`
	UserSide   map[string]decimal.Decimal `json:"user_side"`   // classification code → total
	SystemSide map[string]decimal.Decimal `json:"system_side"` // classification code → total
}

// SolvencyReport is the result of a solvency check for a single currency.
//
// Liability is the sum of all user-side balances (holder > 0) across every
// active classification for the given currency. This represents what the
// platform owes to users in aggregate.
//
// Custodial is the sum of system-side balances for classifications whose code
// is "custodial". This represents funds the platform holds in custody on behalf
// of users.
//
// Solvent is true when Custodial >= Liability (platform can cover user claims).
//
// Margin is Custodial - Liability. Positive means surplus; negative means
// the platform is under-collateralised in the ledger picture. Comparing this
// figure to an off-chain custody position is the consumer's responsibility.
type SolvencyReport struct {
	CurrencyID int64           `json:"currency_id"`
	Liability  decimal.Decimal `json:"liability"`
	Custodial  decimal.Decimal `json:"custodial"`
	Solvent    bool            `json:"solvent"`
	Margin     decimal.Decimal `json:"margin"`
}

// PlatformBalanceReader reads structured platform-wide balance breakdowns from
// the system_rollups table.
type PlatformBalanceReader interface {
	// GetPlatformBalances returns a per-classification breakdown for the given
	// currency. Both UserSide and SystemSide maps are keyed by classification
	// code; missing classifications have zero balance.
	GetPlatformBalances(ctx context.Context, currencyID int64) (*PlatformBalance, error)

	// GetTotalLiabilityByAsset returns the sum of all user-side balances
	// (holder > 0) across all classifications for the given currency.
	GetTotalLiabilityByAsset(ctx context.Context, currencyID int64) (decimal.Decimal, error)
}

// SolvencyChecker computes a solvency report for a single currency.
type SolvencyChecker interface {
	// SolvencyCheck returns a SolvencyReport for the given currency.
	// Custodial is the total of system-side "custodial" classification balances.
	// Liability is the total of all user-side balances.
	// The check is O(1) — it reads from the system_rollups materialized table.
	SolvencyCheck(ctx context.Context, currencyID int64) (*SolvencyReport, error)
}
