package presets

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

func TestDevCreditBundle_Classifications(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallDevCreditBundle(ctx, cs, jts, ts))

	dc, err := cs.GetByCode(ctx, DevCreditClassificationCode)
	require.NoError(t, err)
	assert.Equal(t, core.NormalSideCredit, dc.NormalSide)
	assert.True(t, dc.IsSystem)
	// Not part of any holder's spendable-money view: it is the system-side
	// counterparty, never a holder-facing bucket.
	assert.Equal(t, core.BalanceRoleNone, dc.BalanceRole)

	// main_wallet rides along (shared) — it is the credited leg.
	mw, err := cs.GetByCode(ctx, "main_wallet")
	require.NoError(t, err)
	assert.Equal(t, core.BalanceRoleAvailable, mw.BalanceRole)
}

// TestDevCreditBundle_NotCustodial pins the solvency contract: simulated
// credit must never be booked against "custodial", because
// GetSystemSideCustodialBalance keys on exactly that code. Were these equal,
// every simulated top-up would inflate the asset side by the amount it added
// to liabilities and /platform/solvency would report a healthy margin over
// balance that nothing backs.
func TestDevCreditBundle_NotCustodial(t *testing.T) {
	assert.NotEqual(t, "custodial", DevCreditClassificationCode)

	for _, tmpl := range devCreditTemplates {
		for _, line := range tmpl.Lines {
			assert.NotEqual(t, "custodial", line.ClassificationCode,
				"template %q must not touch the custodial account", tmpl.Code)
		}
	}
}

func TestDevCreditBundle_Template_Balance(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallDevCreditBundle(ctx, cs, jts, ts))

	const holder = int64(42)
	amount := decimal.RequireFromString("125.5")
	params := core.TemplateParams{
		HolderID:       holder,
		CurrencyUID:    "cur-1",
		IdempotencyKey: "devcredit-1",
		Amounts:        map[string]decimal.Decimal{"amount": amount},
	}

	tmpl, err := ts.GetTemplate(ctx, DevCreditTemplateCode)
	require.NoError(t, err)
	journal, err := tmpl.Render(params)
	require.NoError(t, err)

	assertBalanced(t, journal.Entries)
	require.Len(t, journal.Entries, 2)

	// DR main_wallet (user side) — spendable balance lands on the holder.
	assert.Equal(t, core.EntryTypeDebit, journal.Entries[0].EntryType)
	assert.Equal(t, holder, journal.Entries[0].AccountHolder)
	assert.True(t, journal.Entries[0].Amount.Equal(amount))

	// CR dev_credit (system side) — the unbacked counterparty.
	assert.Equal(t, core.EntryTypeCredit, journal.Entries[1].EntryType)
	assert.Equal(t, core.SystemAccountHolder(holder), journal.Entries[1].AccountHolder)
	assert.True(t, journal.Entries[1].Amount.Equal(amount))
}

func TestDevCreditBundle_JournalType(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallDevCreditBundle(ctx, cs, jts, ts))

	jt, err := jts.GetJournalTypeByCode(ctx, DevCreditJournalTypeCode)
	require.NoError(t, err)
	assert.Equal(t, "Developer Credit", jt.Name)
	// Holder-facing wording must not narrate how the balance was produced.
	assert.NotContains(t, jt.DisplayLabel, "Developer")
	assert.NotContains(t, jt.DisplayLabel, "dev")
}

// TestDevCreditBundle_ExcludedFromExtendedPresets pins the opt-in property:
// installing the full preset suite must not hand a deployment the ability to
// mint balance out of nothing. Only an explicit InstallDevCreditBundle call
// does that.
func TestDevCreditBundle_ExcludedFromExtendedPresets(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallExtendedPresets(ctx, cs, jts, ts))

	_, err := cs.GetByCode(ctx, DevCreditClassificationCode)
	require.Error(t, err, "extended presets must not install the dev-credit classification")

	_, err = ts.GetTemplate(ctx, DevCreditTemplateCode)
	require.Error(t, err, "extended presets must not install the dev-credit template")
}

func TestDevCreditBundle_Idempotent(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallDevCreditBundle(ctx, cs, jts, ts))
	require.NoError(t, InstallDevCreditBundle(ctx, cs, jts, ts))
}
