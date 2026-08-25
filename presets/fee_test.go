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
	require.Len(t, tmpl.Lines, 2)

	amount := decimal.NewFromFloat(2.50)
	params := core.TemplateParams{
		HolderID:       42,
		CurrencyUID:    "cur-1",
		IdempotencyKey: "fee-42",
		Amounts:        map[string]decimal.Decimal{"amount": amount},
	}

	journal, err := tmpl.Render(params)
	require.NoError(t, err)
	assertBalanced(t, journal.Entries)

	// The holder leg credits, because main_wallet is declared NormalSideDebit
	// and a fee is money leaving the holder. This assertion used to require a
	// debit, which is what let the inverted template ship: the test did not
	// miss the bug, it certified it.
	//
	// assertBalanced above cannot catch this and never could -- both lines
	// draw on the same amount key, so total debits equal total credits
	// whichever side each classification lands on. The direction is only
	// observable in the balance the entries produce, which is pinned against
	// real Postgres by TestPresetDirection_MovesMoneyTheRightWay in the root
	// package.
	assert.Equal(t, core.EntryTypeCredit, journal.Entries[0].EntryType, "the holder pays the fee, so the holder leg credits")
	assert.Equal(t, int64(42), journal.Entries[0].AccountHolder) // user
	assert.Equal(t, core.EntryTypeDebit, journal.Entries[1].EntryType)
	assert.Equal(t, int64(-42), journal.Entries[1].AccountHolder) // system
}

func TestFeeBundle_Idempotent(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallFeeBundle(ctx, cs, jts, ts))
	require.NoError(t, InstallFeeBundle(ctx, cs, jts, ts))
}
