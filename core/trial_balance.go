package core

import (
	"time"

	"github.com/shopspring/decimal"
)

// TrialBalanceRow is one classification's aggregated debit/credit totals as
// of a given point in time.
type TrialBalanceRow struct {
	ClassificationID   int64           `json:"classification_id"`
	ClassificationCode string          `json:"classification_code"`
	ClassificationName string          `json:"classification_name"`
	NormalSide         NormalSide      `json:"normal_side"`
	TotalDebit         decimal.Decimal `json:"total_debit"`
	TotalCredit        decimal.Decimal `json:"total_credit"`
	// Net is the balance in the classification's normal-side direction:
	// debit-normal -> total_debit - total_credit, credit-normal -> the reverse.
	Net decimal.Decimal `json:"net"`
}

// TrialBalanceReport is a full trial balance for one currency as of one
// point in time, with the global debit=credit check.
type TrialBalanceReport struct {
	CurrencyID  int64             `json:"currency_id"`
	AsOf        time.Time         `json:"as_of"`
	Rows        []TrialBalanceRow `json:"rows"`
	TotalDebit  decimal.Decimal   `json:"total_debit"`
	TotalCredit decimal.Decimal   `json:"total_credit"`
	// Balanced reports whether TotalDebit == TotalCredit — the invariant a
	// trial balance exists to verify.
	Balanced bool `json:"balanced"`
}
