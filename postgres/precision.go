package postgres

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

// fetchCurrencyExponents batch-loads the currency rows for the given IDs and
// returns them keyed by ID. IDs that don't exist in currencies are simply
// absent from the result — callers rely on the currency_id foreign key to
// reject genuinely nonexistent currencies at insert time; this function only
// needs the exponent (and code, for error messages) of currencies that do
// exist.
func fetchCurrencyExponents(ctx context.Context, q *sqlcgen.Queries, currencyIDs []int64) (map[int64]sqlcgen.Currency, error) {
	if len(currencyIDs) == 0 {
		return nil, nil
	}
	rows, err := q.GetCurrenciesByIDs(ctx, currencyIDs)
	if err != nil {
		return nil, fmt.Errorf("postgres: get currency exponents: %w", err)
	}
	out := make(map[int64]sqlcgen.Currency, len(rows))
	for _, r := range rows {
		out[r.ID] = r
	}
	return out, nil
}

// checkAmountPrecision rejects amount if it carries more decimal places than
// the currency's exponent allows. It never rounds or truncates — precision
// is the caller's explicit decision (see core/money.go's Round/ConvertAt);
// the store only refuses to silently accept an over-precise amount.
func checkAmountPrecision(amount decimal.Decimal, currency sqlcgen.Currency) error {
	exponent := int32(currency.Exponent)
	if amount.Equal(amount.Truncate(exponent)) {
		return nil
	}
	return fmt.Errorf(
		"postgres: amount %s exceeds currency %s (id=%d) exponent %d: %w",
		amount.String(), currency.Code, currency.ID, exponent, core.ErrPrecisionExceeded,
	)
}

// uniqueCurrencyIDs returns the deduplicated set of currency IDs referenced
// by entries, in first-seen order (order doesn't matter for the batch fetch,
// but keeping it deterministic makes test failures easier to read).
func uniqueCurrencyIDs(entries []core.EntryInput) []int64 {
	seen := make(map[int64]struct{}, len(entries))
	ids := make([]int64, 0, len(entries))
	for _, e := range entries {
		if _, ok := seen[e.CurrencyID]; ok {
			continue
		}
		seen[e.CurrencyID] = struct{}{}
		ids = append(ids, e.CurrencyID)
	}
	return ids
}

// validateEntriesPrecision checks every entry's amount against its
// currency's exponent. Entries whose currency_id doesn't exist are skipped —
// the subsequent insert's foreign key constraint rejects those with a clear
// core.ErrInvalidInput, and duplicating that check here would require an
// extra sentinel for no real benefit.
func validateEntriesPrecision(ctx context.Context, q *sqlcgen.Queries, entries []core.EntryInput) error {
	exponents, err := fetchCurrencyExponents(ctx, q, uniqueCurrencyIDs(entries))
	if err != nil {
		return err
	}
	for i, e := range entries {
		currency, ok := exponents[e.CurrencyID]
		if !ok {
			continue
		}
		if err := checkAmountPrecision(e.Amount, currency); err != nil {
			return fmt.Errorf("postgres: entry[%d]: %w", i, err)
		}
	}
	return nil
}

// validateSingleAmountPrecision checks one amount against its currency's
// exponent. Used by write paths that post a single amount rather than a
// batch of journal entries (e.g. Reserve).
func validateSingleAmountPrecision(ctx context.Context, q *sqlcgen.Queries, currencyID int64, amount decimal.Decimal) error {
	exponents, err := fetchCurrencyExponents(ctx, q, []int64{currencyID})
	if err != nil {
		return err
	}
	currency, ok := exponents[currencyID]
	if !ok {
		return nil // FK constraint on insert rejects a nonexistent currency.
	}
	return checkAmountPrecision(amount, currency)
}
