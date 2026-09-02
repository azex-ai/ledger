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
//
// Failures come back through httpx.ErrField, so the response's
// message.fields names the offending field (J-8). This function is the
// single choke point for wire amounts across every handler, which is why it
// is the highest-value place to attach that.
func parseWireAmount(raw, field string) (decimal.Decimal, error) {
	if strings.ContainsAny(raw, "eE") {
		return decimal.Decimal{}, httpx.ErrField(field, "must be a plain decimal string; scientific notation is not accepted")
	}
	d, err := decimal.NewFromString(raw)
	if err != nil {
		return decimal.Decimal{}, httpx.ErrField(field, "is not a valid decimal amount")
	}
	return d, nil
}
