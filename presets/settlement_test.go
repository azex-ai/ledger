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
		CurrencyUID:    "cur-1",
		IdempotencyKey: "settle-gross-1",
		Amounts:        map[string]decimal.Decimal{"gross_amount": gross},
	}

	tmpl, err := ts.GetTemplate(ctx, "checkout_settlement_gross")
	require.NoError(t, err)
	require.Len(t, tmpl.Lines, 2)

	journal, err := tmpl.Render(params)
	require.NoError(t, err)

	// Inverted pin (2026-09-02 audit A-M2). It used to assert
	//
	//	assert.Equal(t, core.EntryTypeCredit, journal.Entries[1].EntryType) // merchant
	//
	// with a `// merchant` comment naming the very leg that was taking money
	// OFF the merchant: the journal type declares HolderTxKindDeposit and the
	// wire showed direction=out on a 97-unit "Payment". A merchant being
	// settled RECEIVES; main_wallet is debit-normal; therefore the merchant
	// leg debits.
	assertJournalEffect(t, cs, params.HolderID, journal.Entries, map[string]string{
		"main_wallet/user": gross.String(),
		"custodial/system": gross.String(),
	})
	assert.Equal(t, int64(-10), journal.Entries[1].AccountHolder) // system counterpart
	assert.Equal(t, int64(10), journal.Entries[0].AccountHolder)  // merchant
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
		CurrencyUID:    "cur-1",
		IdempotencyKey: "settle-net-1",
		Amounts: map[string]decimal.Decimal{
			"gross_amount": gross,
			"net_amount":   net,
			"fee_amount":   fee,
		},
	}

	tmpl, err := ts.GetTemplate(ctx, "checkout_settlement_net")
	require.NoError(t, err)
	// Four legs, not three: crediting revenue to credit-normal `fees` needs a
	// matching debit, and no three-leg arrangement of these classifications
	// can raise the merchant, the custody pool and the revenue account at
	// once (presets/settlement.go explains the arithmetic).
	require.Len(t, tmpl.Lines, 4)

	journal, err := tmpl.Render(params)
	require.NoError(t, err)

	// Inverted pin (2026-09-02 audit A-M2): the old assertions demanded
	// DR custodial(gross) + CR main_wallet(net), which drained the custody
	// pool by gross while only reducing the liability by net -- a fresh
	// -fee of phantom insolvency on every single settlement.
	assertJournalEffect(t, cs, params.HolderID, journal.Entries, map[string]string{
		"main_wallet/user": net.String(),
		"fee_expense/user": fee.String(),
		"custodial/system": net.String(),
		"fees/system":      fee.String(),
	})
	// gross is not a leg; it is net + fee, and the ledger's balance rule is
	// what enforces the relationship now.
	require.True(t, gross.Equal(net.Add(fee)))
}

func TestSettlementBundle_Idempotent(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallSettlementBundle(ctx, cs, jts, ts))
	require.NoError(t, InstallSettlementBundle(ctx, cs, jts, ts))
}
