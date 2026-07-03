package server

import (
	"strings"

	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/pkg/httpx"
)

// parseWireAmount parses a JSON wire amount. The wire format is a plain
// decimal string ("123.456", api-contract §4) — scientific notation ("1e10")
// is technically a valid decimal but never a legitimate client-sent amount,
// so it is rejected to keep the wire format strict.
func parseWireAmount(raw, field string) (decimal.Decimal, error) {
	if strings.ContainsAny(raw, "eE") {
		return decimal.Decimal{}, httpx.ErrBadRequest(field + " must be a plain decimal string (scientific notation is not accepted)")
	}
	d, err := decimal.NewFromString(raw)
	if err != nil {
		return decimal.Decimal{}, httpx.ErrBadRequest(field + " is not a valid decimal")
	}
	return d, nil
}
