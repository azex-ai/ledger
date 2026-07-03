package postgres

import (
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
)

// checkAmountPrecision rejects amount if it carries more decimal places than
// the currency's exponent allows. It never rounds or truncates — precision
// is the caller's explicit decision (see core/money.go's Round/ConvertAt);
// the store only refuses to silently accept an over-precise amount.
func checkAmountPrecision(amount decimal.Decimal, currency dimCurrency) error {
	if amount.Equal(amount.Truncate(currency.Exponent)) {
		return nil
	}
	return fmt.Errorf(
		"postgres: amount %s exceeds currency %s (%s) exponent %d: %w",
		amount.String(), currency.Code, currency.UID, currency.Exponent, core.ErrPrecisionExceeded,
	)
}

// validateEntriesPrecision checks every resolved entry's amount against its
// currency's exponent. Pure: the exponent was captured during uid resolution,
// so no query round-trip is needed here.
func validateEntriesPrecision(entries []resolvedEntry) error {
	for i, e := range entries {
		currency := dimCurrency{UID: e.CurrencyUID, Code: e.CurrencyUID, Exponent: e.exponent}
		if err := checkAmountPrecision(e.Amount, currency); err != nil {
			return fmt.Errorf("postgres: entry[%d]: %w", i, err)
		}
	}
	return nil
}
