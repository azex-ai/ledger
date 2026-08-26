package postgres

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
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
// currency's exponent. The exponent was already captured during uid
// resolution, but the currency's short code was not carried onto
// resolvedEntry -- only its internal id and uid -- so the code is resolved
// here via the (cache-backed, cheap after the first miss) dims lookup by id.
// Without this, the error message quoted the uid twice under a "code" label,
// which silently dropped the one piece of information (the short code) an
// operator actually needs to identify the currency at a glance.
func validateEntriesPrecision(ctx context.Context, dims *dimCache, q *sqlcgen.Queries, entries []resolvedEntry) error {
	for i, e := range entries {
		cur, err := dims.currencyByIDOrErr(ctx, q, e.currencyID)
		if err != nil {
			return fmt.Errorf("postgres: entry[%d]: %w", i, err)
		}
		if err := checkAmountPrecision(e.Amount, cur); err != nil {
			return fmt.Errorf("postgres: entry[%d]: %w", i, err)
		}
	}
	return nil
}
