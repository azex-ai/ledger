package postgres_test

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/presets"
)

// TestSolvencyCheck_IgnoresTamperedCheckpoints is W3-M2's pin (2026-09-02
// adversarial re-review, w3-review/money-path.md M-2).
//
// I-49 established that balance_checkpoints is an UNTRUSTED cache: it is a
// derived table ledger_app may INSERT into freely, and nothing about a
// checkpoint row is signed. That conclusion landed on exactly one consumer,
// the withdrawal gate. SolvencyCheck -- the only alarm in the library that
// says "credit was issued with nothing behind it" -- kept reading
// `COALESCE(bc.balance,0) + delta` on BOTH sides, so one INSERT flipped it:
//
//	SOLVENCY before tamper: liability=1250.75 custodial=1000 solvent=false
//	-- INSERT INTO balance_checkpoints (custodial system holder, balance=1e6)
//	SOLVENCY after tamper:  solvent=true
//
// Both figures are now recomputed from journal_entries alone (the same
// entries-only basis the gate's verified balance uses). It is a periodic
// report, so the cost of not using the cache is acceptable; being wrong in
// the direction of "no alarm" is not.
func TestSolvencyCheck_IgnoresTamperedCheckpoints(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	classStore := postgres.NewClassificationStore(pool)
	journalTypes := postgres.JournalTypeStoreAdapter{ClassificationStore: classStore}
	tmplStore := postgres.NewTemplateStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)
	pbStore := postgres.NewPlatformBalanceStore(pool)

	usdt, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{
		Code: "USDT-SOLVTAMPER", Name: "Tether USD Solvency Tamper", Exponent: 18,
	})
	require.NoError(t, err)
	require.NoError(t, presets.InstallTemplateBundle(ctx, classStore, journalTypes, tmplStore, presets.DepositBundle()))
	require.NoError(t, presets.InstallDevCreditBundle(ctx, classStore, journalTypes, tmplStore))

	const holder = int64(9401)

	// A backed deposit (so the custodial dimension has real entries, which is
	// what the tampered checkpoint attaches to) ...
	_, err = ledgerStore.ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
		HolderID:       holder,
		CurrencyUID:    usdt.UID,
		IdempotencyKey: postgrestest.UniqueKey("solvtamper-deposit"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(1000)},
		Source:         "test",
	})
	require.NoError(t, err)
	// ... and unbacked credit on top, which is the shortfall this alarm exists
	// to expose.
	_, err = ledgerStore.ExecuteTemplate(ctx, presets.DevCreditTemplateCode, core.TemplateParams{
		HolderID:       holder,
		CurrencyUID:    usdt.UID,
		IdempotencyKey: postgrestest.UniqueKey("solvtamper-devcredit"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(250)},
		Source:         "test",
	})
	require.NoError(t, err)

	before, err := pbStore.SolvencyCheck(ctx, usdt.UID)
	require.NoError(t, err)
	require.False(t, before.Solvent, "baseline: 1250 of claims against 1000 of custody: %+v", before)
	require.True(t, before.Custodial.Equal(decimal.NewFromInt(1000)), "custodial=%s", before.Custodial)
	require.True(t, before.Liability.Equal(decimal.NewFromInt(1250)), "liability=%s", before.Liability)

	// The attack: one INSERT, with nothing more than the app's own
	// credentials, on the custodial dimension the deposit created.
	var currencyID, custodialClassID int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM currencies WHERE uid = $1`, usdt.UID).Scan(&currencyID))
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM classifications WHERE code = 'custodial'`).Scan(&custodialClassID))
	_, err = pool.Exec(ctx, `
		INSERT INTO balance_checkpoints (account_holder, currency_id, classification_id, balance, last_entry_id)
		VALUES ($1, $2, $3, 1000000, 0)
	`, core.SystemAccountHolder(holder), currencyID, custodialClassID)
	require.NoError(t, err)

	after, err := pbStore.SolvencyCheck(ctx, usdt.UID)
	require.NoError(t, err)
	assert.False(t, after.Solvent,
		"a forged checkpoint row must not be able to silence the only unbacked-issuance alarm in the library: %+v", after)
	assert.True(t, after.Custodial.Equal(before.Custodial),
		"custodial must be recomputed from entries alone; before=%s after=%s", before.Custodial, after.Custodial)
	assert.True(t, after.Liability.Equal(before.Liability),
		"liability must be recomputed from entries alone; before=%s after=%s", before.Liability, after.Liability)
}
