package presets

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

func TestTransferBundle_Classifications(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallTransferBundle(ctx, cs, jts, ts))

	// settlement must be credit-normal system account
	settlement, err := cs.GetByCode(ctx, "settlement")
	require.NoError(t, err)
	assert.Equal(t, core.NormalSideCredit, settlement.NormalSide)
	assert.True(t, settlement.IsSystem)

	// main_wallet pulled in via shared
	mw, err := cs.GetByCode(ctx, "main_wallet")
	require.NoError(t, err)
	assert.Equal(t, core.NormalSideDebit, mw.NormalSide)
	assert.False(t, mw.IsSystem)
}

func TestTransferBundle_JournalType(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallTransferBundle(ctx, cs, jts, ts))

	jt, err := jts.GetJournalTypeByCode(ctx, "transfer")
	require.NoError(t, err)
	assert.Equal(t, "User-to-user Transfer", jt.Name)
}

func TestTransferBundle_Templates_Balance(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallTransferBundle(ctx, cs, jts, ts))

	amount := decimal.NewFromInt(100)
	params := core.TemplateParams{
		HolderID:       1,
		CurrencyID:     1,
		IdempotencyKey: "tx-1",
		Amounts:        map[string]decimal.Decimal{"amount": amount},
	}

	// transfer_out: DR main_wallet CR settlement — must balance
	tmplOut, err := ts.GetTemplate(ctx, "transfer_out")
	require.NoError(t, err)
	journalOut, err := tmplOut.Render(params)
	require.NoError(t, err)
	assertBalanced(t, journalOut.Entries)

	// transfer_in: DR settlement CR main_wallet — must balance
	params.IdempotencyKey = "tx-2"
	tmplIn, err := ts.GetTemplate(ctx, "transfer_in")
	require.NoError(t, err)
	journalIn, err := tmplIn.Render(params)
	require.NoError(t, err)
	assertBalanced(t, journalIn.Entries)
}

func TestTransferBundle_Idempotent(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallTransferBundle(ctx, cs, jts, ts))
	require.NoError(t, InstallTransferBundle(ctx, cs, jts, ts))
}

// assertBalanced verifies total debits equal total credits.
func assertBalanced(t *testing.T, entries []core.EntryInput) {
	t.Helper()
	var totalDebit, totalCredit decimal.Decimal
	for _, e := range entries {
		switch e.EntryType {
		case core.EntryTypeDebit:
			totalDebit = totalDebit.Add(e.Amount)
		case core.EntryTypeCredit:
			totalCredit = totalCredit.Add(e.Amount)
		}
	}
	assert.True(t, totalDebit.Equal(totalCredit),
		"journal not balanced: DR=%s CR=%s", totalDebit, totalCredit)
}
