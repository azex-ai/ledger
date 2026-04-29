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
		CurrencyID:     1,
		IdempotencyKey: "fee-42",
		Amounts:        map[string]decimal.Decimal{"amount": amount},
	}

	journal, err := tmpl.Render(params)
	require.NoError(t, err)
	assertBalanced(t, journal.Entries)

	// DR entry should be user, CR entry should be system
	assert.Equal(t, core.EntryTypeDebit, journal.Entries[0].EntryType)
	assert.Equal(t, int64(42), journal.Entries[0].AccountHolder) // user
	assert.Equal(t, core.EntryTypeCredit, journal.Entries[1].EntryType)
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
