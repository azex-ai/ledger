package presets

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

func TestFeeBundle_Classifications(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallFeeBundle(ctx, cs, jts, ts))

	fees, err := cs.GetByCode(ctx, "fees")
	require.NoError(t, err)
	assert.Equal(t, core.NormalSideCredit, fees.NormalSide)
	assert.True(t, fees.IsSystem)
}

func TestFeeBundle_JournalType(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallFeeBundle(ctx, cs, jts, ts))

	jt, err := jts.GetJournalTypeByCode(ctx, "fee")
	require.NoError(t, err)
	assert.Equal(t, "Fee Charge", jt.Name)
}

func TestFeeBundle_Template_Balance(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallFeeBundle(ctx, cs, jts, ts))

	tmpl, err := ts.GetTemplate(ctx, "fee_charge")
	require.NoError(t, err)
	// Four lines, not two (2026-09-02 audit A-M4). `fees` is credit-normal, so
	// platform revenue accumulates on CR -- which is what
	// checkout_settlement_net already did. This template debited it, so two
	// 30-unit fees collected through the two paths summed to zero. Crediting
	// fees needs a second debit, and the pair that supplies it is the same
	// one withdraw_fee uses: the payer's memo cost tracker and the custody
	// pool the earned fee leaves.
	require.Len(t, tmpl.Lines, 4)

	amount := decimal.NewFromFloat(2.50)
	params := core.TemplateParams{
		HolderID:       42,
		CurrencyUID:    "cur-1",
		IdempotencyKey: "fee-42",
		Amounts:        map[string]decimal.Decimal{"amount": amount},
	}

	journal, err := tmpl.Render(params)
	require.NoError(t, err)

	// The holder entry credits, because main_wallet is declared NormalSideDebit
	// and a fee is money leaving the holder. That assertion used to require a
	// debit, which is what let the inverted template ship: the test did not
	// miss the bug, it certified it.
	//
	// assertBalanced alone cannot catch this and never could -- the entries draw
	// on the same amount key, so total debits equal total credits whichever
	// side each classification lands on. assertJournalEffect states the
	// EFFECT instead, which is the thing a reader can check against the
	// product meaning of "charge a fee".
	assertJournalEffect(t, cs, params.HolderID, journal.Entries, map[string]string{
		"main_wallet/user": amount.Neg().String(), // the holder pays
		"fee_expense/user": amount.String(),       // ... and it is visible as their cost
		"custodial/system": amount.Neg().String(), // the fee leaves the custody pool
		"fees/system":      amount.String(),       // ... and lands in platform revenue
	})
}

func TestFeeBundle_Idempotent(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallFeeBundle(ctx, cs, jts, ts))
	require.NoError(t, InstallFeeBundle(ctx, cs, jts, ts))
}
