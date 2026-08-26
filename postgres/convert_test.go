package postgres

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// TestAnyToDecimal_Float64_Rejected pins a Minor from the 2026-08-25
// financial-engineering audit (lead-financial-spotchecks.md "抽查 2"): the
// float64 branch of anyToDecimal used to warn-and-continue
// (decimal.NewFromFloat) instead of rejecting. That branch is confirmed
// unreachable through pgx v5 today (SUM(numeric) never surfaces as Go
// float64), but financial.md's "never float64" rule is absolute, and a
// defensive branch guarding an absolute rule must fail loud if it is ever
// exercised -- not silently and irreversibly lose precision. This pin
// exercises the branch directly (bypassing pgx) so the behavior is asserted
// even though the path is otherwise unreachable in production.
func TestAnyToDecimal_Float64_Rejected(t *testing.T) {
	_, err := anyToDecimal(float64(0.1))
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrPrecisionExceeded)
}

// TestAnyToDecimal_Nil_IsZero pins the (unaffected) nil-input path so the
// float64 fix above can't accidentally regress the "no rows" case that every
// other anyToDecimal caller relies on (COALESCE(SUM(...), 0) over an empty
// set arrives here as untyped nil, not 0).
func TestAnyToDecimal_Nil_IsZero(t *testing.T) {
	d, err := anyToDecimal(nil)
	require.NoError(t, err)
	assert.True(t, d.IsZero())
}

// TestAnyToDecimal_UnsupportedType_Rejected pins the pre-existing default
// branch alongside the float64 fix: an exotic type that isn't numeric,
// int64, string, or *big.Int must still be a hard error, not silently
// coerced.
func TestAnyToDecimal_UnsupportedType_Rejected(t *testing.T) {
	_, err := anyToDecimal(struct{}{})
	require.Error(t, err)
	assert.False(t, errors.Is(err, core.ErrPrecisionExceeded), "unsupported-type error must stay distinct from the float64/precision-loss error")
}
