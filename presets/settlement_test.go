package presets

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

func TestSettlementBundle_Classifications(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallSettlementBundle(ctx, cs, jts, ts))

	settlement, err := cs.GetByCode(ctx, "settlement")
	require.NoError(t, err)
	assert.Equal(t, core.NormalSideCredit, settlement.NormalSide)
	assert.True(t, settlement.IsSystem)

	fees, err := cs.GetByCode(ctx, "fees")
	require.NoError(t, err)
	assert.Equal(t, core.NormalSideCredit, fees.NormalSide)
	assert.True(t, fees.IsSystem)
}

func TestSettlementBundle_JournalType(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallSettlementBundle(ctx, cs, jts, ts))

	jt, err := jts.GetJournalTypeByCode(ctx, "checkout_settlement")
	require.NoError(t, err)
	assert.Equal(t, "Checkout Settlement", jt.Name)
}

func TestSettlementBundle_GrossTemplate_Balance(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallSettlementBundle(ctx, cs, jts, ts))

	gross := decimal.NewFromInt(500)
	params := core.TemplateParams{
		HolderID:       10, // merchant
		CurrencyID:     1,
		IdempotencyKey: "settle-gross-1",
		Amounts:        map[string]decimal.Decimal{"gross_amount": gross},
	}

	tmpl, err := ts.GetTemplate(ctx, "checkout_settlement_gross")
	require.NoError(t, err)
	require.Len(t, tmpl.Lines, 2)

	journal, err := tmpl.Render(params)
	require.NoError(t, err)
	assertBalanced(t, journal.Entries)

	// DR custodial (system) CR main_wallet (user/merchant)
	assert.Equal(t, core.EntryTypeDebit, journal.Entries[0].EntryType)
	assert.Equal(t, int64(-10), journal.Entries[0].AccountHolder) // system
	assert.Equal(t, core.EntryTypeCredit, journal.Entries[1].EntryType)
	assert.Equal(t, int64(10), journal.Entries[1].AccountHolder) // merchant
}

func TestSettlementBundle_NetTemplate_Balance(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallSettlementBundle(ctx, cs, jts, ts))

	gross := decimal.NewFromInt(500)
	net := decimal.NewFromInt(490)
	fee := decimal.NewFromInt(10)
	// invariant: gross == net + fee
	require.True(t, gross.Equal(net.Add(fee)))

	params := core.TemplateParams{
		HolderID:       10, // merchant
		CurrencyID:     1,
		IdempotencyKey: "settle-net-1",
		Amounts: map[string]decimal.Decimal{
			"gross_amount": gross,
			"net_amount":   net,
			"fee_amount":   fee,
		},
	}

	tmpl, err := ts.GetTemplate(ctx, "checkout_settlement_net")
	require.NoError(t, err)
	require.Len(t, tmpl.Lines, 3)

	journal, err := tmpl.Render(params)
	require.NoError(t, err)
	assertBalanced(t, journal.Entries)

	// DR custodial(system, gross) | CR main_wallet(merchant, net) + CR fees(system, fee)
	assert.Equal(t, core.EntryTypeDebit, journal.Entries[0].EntryType)
	assert.True(t, journal.Entries[0].Amount.Equal(gross))
	assert.Equal(t, core.EntryTypeCredit, journal.Entries[1].EntryType)
	assert.True(t, journal.Entries[1].Amount.Equal(net))
	assert.Equal(t, core.EntryTypeCredit, journal.Entries[2].EntryType)
	assert.True(t, journal.Entries[2].Amount.Equal(fee))
}

func TestSettlementBundle_Idempotent(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallSettlementBundle(ctx, cs, jts, ts))
	require.NoError(t, InstallSettlementBundle(ctx, cs, jts, ts))
}
