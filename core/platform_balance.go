package core

import (
	"context"

	"github.com/shopspring/decimal"
)

// PlatformBalance is a structured per-currency breakdown of system-wide balances
// read in real time from the ledger.
//
// UserSide contains totals for accounts with positive holder IDs (holder > 0),
// keyed by classification code.
//
// SystemSide contains totals for accounts with negative holder IDs (holder < 0),
// keyed by classification code.
//
// Amounts are computed as `checkpoint.balance + delta`, where delta is the net
// of journal_entries posted after each account checkpoint's last_entry_id.
// Reads therefore reflect every committed journal immediately, without waiting
// for the rollup worker.
type PlatformBalance struct {
	CurrencyUID string                     `json:"currency_uid"`
	UserSide    map[string]decimal.Decimal `json:"user_side"`   // classification code → total
	SystemSide  map[string]decimal.Decimal `json:"system_side"` // classification code → total
}

// SolvencyReport is the result of a solvency check for a single currency.
//
// Liability is the sum of user-side balances (holder > 0) across every
// active classification tagged with a non-empty BalanceRole (Available,
// Pending, or Locked) for the given currency — the same basis
// BalanceReader.GetBalanceBreakdown uses for a holder's spendable-money
// view. This represents what the platform owes to users in aggregate.
// Role-less classifications (fee_expense and similar debit-normal cost/memo
// accounts booked to a user's holder id for per-user reporting) are
// excluded: they are not liabilities, and summing them in previously turned
// every dollar of cumulative fee revenue into a phantom dollar of
// insolvency (docs/audits/2026-08-25-financial-engineering/financial-correctness.md
// Major #1).
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
	CurrencyUID string          `json:"currency_uid"`
	Liability   decimal.Decimal `json:"liability"`
	Custodial   decimal.Decimal `json:"custodial"`
	Solvent     bool            `json:"solvent"`
	Margin      decimal.Decimal `json:"margin"`
}

// PlatformBalanceReader reads structured platform-wide balance breakdowns from
// the ledger in real time.
type PlatformBalanceReader interface {
	// GetPlatformBalances returns a per-classification breakdown for the given
	// currency. Both UserSide and SystemSide maps are keyed by classification
	// code; missing classifications have zero balance.
	GetPlatformBalances(ctx context.Context, currencyUID string) (*PlatformBalance, error)

	// GetTotalLiabilityByAsset returns the sum of user-side (holder > 0)
	// balances for classifications with a non-empty BalanceRole for the
	// given currency (see SolvencyReport.Liability).
	GetTotalLiabilityByAsset(ctx context.Context, currencyUID string) (decimal.Decimal, error)
}

// SolvencyChecker computes a solvency report for a single currency.
type SolvencyChecker interface {
	// SolvencyCheck returns a SolvencyReport for the given currency.
	// Custodial is the total of system-side "custodial" classification balances.
	// Liability is the total of user-side balances with a non-empty
	// BalanceRole (see SolvencyReport.Liability).
	// Implementations should ensure the custodial and liability figures describe
	// the same point in time.
	SolvencyCheck(ctx context.Context, currencyUID string) (*SolvencyReport, error)
}
