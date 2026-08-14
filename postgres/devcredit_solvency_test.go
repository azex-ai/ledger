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

// TestDevCredit_SolvencyShortfallEqualsDevCreditBalance is the Postgres-backed
// proof of the property the dev-credit preset is designed around: credit issued
// with no custodied asset behind it shows up as a solvency shortfall, and that
// shortfall equals the dev_credit account's balance exactly.
//
// Booking simulated credit against its own classification (rather than
// custodial, or through the deposit templates) is what makes this hold. Were
// it booked against custodial, both sides of the report would move together
// and an insolvent platform would report a healthy margin.
func TestDevCredit_SolvencyShortfallEqualsDevCreditBalance(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	classStore := postgres.NewClassificationStore(pool)
	journalTypes := postgres.JournalTypeStoreAdapter{ClassificationStore: classStore}
	tmplStore := postgres.NewTemplateStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)
	pbStore := postgres.NewPlatformBalanceStore(pool)

	usdt, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{
		Code: "USDT-DEVC", Name: "Tether USD DevCredit", Exponent: 18,
	})
	require.NoError(t, err)

	// The deposit bundle supplies the custodial-backed path to contrast
	// against; the dev-credit bundle supplies the unbacked one.
	require.NoError(t, presets.InstallTemplateBundle(ctx, classStore, journalTypes, tmplStore, presets.DepositBundle()))
	require.NoError(t, presets.InstallDevCreditBundle(ctx, classStore, journalTypes, tmplStore))

	const holder = int64(9001)
	sys := core.SystemAccountHolder(holder)

	// --- A real deposit: DR main_wallet (holder) CR custodial (system). ---
	deposited := decimal.NewFromInt(1000)
	_, err = ledgerStore.ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
		HolderID:       holder,
		CurrencyUID:    usdt.UID,
		IdempotencyKey: postgrestest.UniqueKey("devc-real-deposit"),
		Amounts:        map[string]decimal.Decimal{"amount": deposited},
		Source:         "test",
	})
	require.NoError(t, err)

	report, err := pbStore.SolvencyCheck(ctx, usdt.UID)
	require.NoError(t, err)
	assert.True(t, report.Solvent, "custodied deposit must leave the platform solvent: %+v", report)
	assert.True(t, report.Liability.Equal(deposited), "liability=%s, got %s", deposited, report.Liability)
	assert.True(t, report.Custodial.Equal(deposited), "custodial=%s, got %s", deposited, report.Custodial)
	assert.True(t, report.Margin.IsZero(), "margin=0, got %s", report.Margin)

	// --- Simulated credit: DR main_wallet (holder) CR dev_credit (system). ---
	simulated := decimal.RequireFromString("250.75")
	journal, err := ledgerStore.ExecuteTemplate(ctx, presets.DevCreditTemplateCode, core.TemplateParams{
		HolderID:       holder,
		CurrencyUID:    usdt.UID,
		IdempotencyKey: postgrestest.UniqueKey("devc-simulated"),
		Amounts:        map[string]decimal.Decimal{"amount": simulated},
		Source:         "test",
	})
	require.NoError(t, err)
	require.NotNil(t, journal)
	assert.True(t, journal.TotalDebit.Equal(journal.TotalCredit), "journal must balance")

	report2, err := pbStore.SolvencyCheck(ctx, usdt.UID)
	require.NoError(t, err)

	// The liability grew; the custodied asset did not.
	assert.True(t, report2.Liability.Equal(deposited.Add(simulated)),
		"liability=%s, got %s", deposited.Add(simulated), report2.Liability)
	assert.True(t, report2.Custodial.Equal(deposited),
		"custodial must not move: want %s, got %s", deposited, report2.Custodial)
	assert.False(t, report2.Solvent, "unbacked credit must report insolvency: %+v", report2)

	// The core property: shortfall == dev_credit balance, to the last unit.
	devCreditClass, err := classStore.GetByCode(ctx, presets.DevCreditClassificationCode)
	require.NoError(t, err)
	devCreditBalance, err := ledgerStore.GetBalance(ctx, sys, usdt.UID, devCreditClass.UID)
	require.NoError(t, err)

	assert.True(t, devCreditBalance.Equal(simulated),
		"dev_credit balance=%s, got %s", simulated, devCreditBalance)
	assert.True(t, report2.Margin.Neg().Equal(devCreditBalance),
		"solvency shortfall (%s) must equal the dev_credit balance (%s)",
		report2.Margin.Neg(), devCreditBalance)
}

// TestDevCredit_CreditedFundsAreSpendable confirms the credited amount lands in
// the holder's available bucket rather than a pending/locked one — the whole
// point of the facility is that it drives real downstream flows.
func TestDevCredit_CreditedFundsAreSpendable(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	classStore := postgres.NewClassificationStore(pool)
	journalTypes := postgres.JournalTypeStoreAdapter{ClassificationStore: classStore}
	tmplStore := postgres.NewTemplateStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)

	usdt, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{
		Code: "USDT-DEVC2", Name: "Tether USD DevCredit 2", Exponent: 18,
	})
	require.NoError(t, err)
	require.NoError(t, presets.InstallDevCreditBundle(ctx, classStore, journalTypes, tmplStore))

	const holder = int64(9002)
	amount := decimal.NewFromInt(500)
	_, err = ledgerStore.ExecuteTemplate(ctx, presets.DevCreditTemplateCode, core.TemplateParams{
		HolderID:       holder,
		CurrencyUID:    usdt.UID,
		IdempotencyKey: postgrestest.UniqueKey("devc-spendable"),
		Amounts:        map[string]decimal.Decimal{"amount": amount},
		Source:         "test",
	})
	require.NoError(t, err)

	breakdown, err := ledgerStore.GetBalanceBreakdown(ctx, holder, usdt.UID)
	require.NoError(t, err)
	assert.True(t, breakdown.Available.Equal(amount),
		"available=%s, got %s", amount, breakdown.Available)
	assert.True(t, breakdown.Pending.IsZero(), "pending must be 0, got %s", breakdown.Pending)
	assert.True(t, breakdown.Locked.IsZero(), "locked must be 0, got %s", breakdown.Locked)
}

// TestDevCredit_InstallIsIdempotent pins that a deployment which installs the
// bundle on every boot does not accumulate duplicate metadata rows.
func TestDevCredit_InstallIsIdempotent(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	journalTypes := postgres.JournalTypeStoreAdapter{ClassificationStore: classStore}
	tmplStore := postgres.NewTemplateStore(pool)

	require.NoError(t, presets.InstallDevCreditBundle(ctx, classStore, journalTypes, tmplStore))
	require.NoError(t, presets.InstallDevCreditBundle(ctx, classStore, journalTypes, tmplStore))

	tmpl, err := tmplStore.GetTemplate(ctx, presets.DevCreditTemplateCode)
	require.NoError(t, err)
	assert.Len(t, tmpl.Lines, 2, "template must keep exactly its two legs")
}
