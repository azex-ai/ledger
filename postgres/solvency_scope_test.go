package postgres_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/presets"
)

// TestSolvencyCheck_RejectsAScopeCodeThatDoesNotExist is m-1's pin (2026-09-02
// adversarial re-review, w3-review/money-path.md m-1). The fail-loud check
// §7.3 introduced only asked "did ANY code match": a scope with one typo in it
// -- WithCustodialClassCodes("custodial", "setlement") -- matched one, passed,
// and left the whole settlement position (FX inventory, transit) silently out
// of the asset side. Multi-code scopes are exactly what §7.3 introduced, so
// "matched some" was the case that needed catching.
func TestSolvencyCheck_RejectsAScopeCodeThatDoesNotExist(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	journalTypes := postgres.JournalTypeStoreAdapter{ClassificationStore: classStore}
	tmplStore := postgres.NewTemplateStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)

	usdt, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{
		Code: "USDT-SCOPE1", Name: "Tether USD Scope 1", Exponent: 18,
	})
	require.NoError(t, err)
	require.NoError(t, presets.InstallTemplateBundle(ctx, classStore, journalTypes, tmplStore, presets.DepositBundle()))

	_, err = postgres.NewPlatformBalanceStore(pool).
		WithCustodialClassCodes("custodial", "setlement").
		SolvencyCheck(ctx, usdt.UID)
	require.Error(t, err, "one matching code must not vouch for the rest of the scope")
	assert.ErrorIs(t, err, core.ErrInvalidInput)
	assert.Contains(t, err.Error(), "setlement", "the error has to name the code that matched nothing")
}

// TestSolvencyCheck_RejectsANonAssetClassificationInScope is m-2's pin. The
// reasoning for which classifications are custodied assets ("equity", "fees",
// "spread" are the platform's own money; "dev_credit" is by design UNBACKED)
// lived only in DefaultCustodialClassCodes' doc comment, so one line of
// consumer configuration -- WithCustodialClassCodes("custodial", "dev_credit")
// -- could move the unbacked issuance this alarm exists to expose onto the
// asset side and make the shortfall disappear. §7.3 moved "what counts as a
// custodied asset" from a hardcoded code to a classification property; the
// property now actually gates it.
func TestSolvencyCheck_RejectsANonAssetClassificationInScope(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	journalTypes := postgres.JournalTypeStoreAdapter{ClassificationStore: classStore}
	tmplStore := postgres.NewTemplateStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)

	usdt, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{
		Code: "USDT-SCOPE2", Name: "Tether USD Scope 2", Exponent: 18,
	})
	require.NoError(t, err)
	require.NoError(t, presets.InstallTemplateBundle(ctx, classStore, journalTypes, tmplStore, presets.DepositBundle()))
	require.NoError(t, presets.InstallDevCreditBundle(ctx, classStore, journalTypes, tmplStore))

	// dev_credit exists and is a system classification, but it is the
	// counterparty of unbacked credit, not an asset backing holder claims.
	_, err = postgres.NewPlatformBalanceStore(pool).
		WithCustodialClassCodes("custodial", "dev_credit").
		SolvencyCheck(ctx, usdt.UID)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)
	assert.Contains(t, err.Error(), "dev_credit")

	// A holder-facing, role-bearing classification is refused for the same
	// reason from the other direction: main_wallet is a LIABILITY, and
	// counting it as an asset would net the report to zero by construction.
	_, err = postgres.NewPlatformBalanceStore(pool).
		WithCustodialClassCodes("custodial", "main_wallet").
		SolvencyCheck(ctx, usdt.UID)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)
	assert.Contains(t, err.Error(), "main_wallet")

	// Control: the shipped default scope still resolves.
	_, err = postgres.NewPlatformBalanceStore(pool).SolvencyCheck(ctx, usdt.UID)
	require.NoError(t, err, "the default scope must keep working: %v", err)
}

// TestSolvencyCheck_RejectsAnEmptyScope closes the third shape of the same
// hole: WithCustodialClassCodes() with no arguments matched no classification,
// so "did any code match" could not fire (there were no codes to match) and
// Custodial was a permanent, silent zero.
func TestSolvencyCheck_RejectsAnEmptyScope(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	journalTypes := postgres.JournalTypeStoreAdapter{ClassificationStore: classStore}
	tmplStore := postgres.NewTemplateStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)

	usdt, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{
		Code: "USDT-SCOPE3", Name: "Tether USD Scope 3", Exponent: 18,
	})
	require.NoError(t, err)
	require.NoError(t, presets.InstallTemplateBundle(ctx, classStore, journalTypes, tmplStore, presets.DepositBundle()))

	_, err = postgres.NewPlatformBalanceStore(pool).
		WithCustodialClassCodes().
		SolvencyCheck(ctx, usdt.UID)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)
}
