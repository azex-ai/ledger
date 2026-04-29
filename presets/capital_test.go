package presets

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

func TestCapitalBundle_Classifications(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallCapitalBundle(ctx, cs, jts, ts))

	equity, err := cs.GetByCode(ctx, "equity")
	require.NoError(t, err)
	assert.Equal(t, core.NormalSideCredit, equity.NormalSide)
	assert.True(t, equity.IsSystem)

	// custodial must also be present (shared)
	custodial, err := cs.GetByCode(ctx, "custodial")
	require.NoError(t, err)
	assert.Equal(t, core.NormalSideCredit, custodial.NormalSide)
	assert.True(t, custodial.IsSystem)
}

func TestCapitalBundle_JournalTypes(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallCapitalBundle(ctx, cs, jts, ts))

	inj, err := jts.GetJournalTypeByCode(ctx, "capital_injection")
	require.NoError(t, err)
	assert.Equal(t, "Capital Injection", inj.Name)

	wd, err := jts.GetJournalTypeByCode(ctx, "capital_withdraw")
	require.NoError(t, err)
	assert.Equal(t, "Capital Withdrawal", wd.Name)
}

func TestCapitalBundle_Templates_Balance(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallCapitalBundle(ctx, cs, jts, ts))

	amount := decimal.NewFromInt(1_000_000)
	params := core.TemplateParams{
		HolderID:       7, // ops workspace
		CurrencyID:     1,
		IdempotencyKey: "cap-inj-1",
		Amounts:        map[string]decimal.Decimal{"amount": amount},
	}

	// capital_injection: DR custodial CR equity
	injTmpl, err := ts.GetTemplate(ctx, "capital_injection")
	require.NoError(t, err)
	injJournal, err := injTmpl.Render(params)
	require.NoError(t, err)
	assertBalanced(t, injJournal.Entries)
	assert.Equal(t, core.EntryTypeDebit, injJournal.Entries[0].EntryType)   // custodial DR
	assert.Equal(t, core.EntryTypeCredit, injJournal.Entries[1].EntryType)  // equity CR

	// capital_withdraw: DR equity CR custodial
	params.IdempotencyKey = "cap-wd-1"
	wdTmpl, err := ts.GetTemplate(ctx, "capital_withdraw")
	require.NoError(t, err)
	wdJournal, err := wdTmpl.Render(params)
	require.NoError(t, err)
	assertBalanced(t, wdJournal.Entries)
	assert.Equal(t, core.EntryTypeDebit, wdJournal.Entries[0].EntryType)   // equity DR
	assert.Equal(t, core.EntryTypeCredit, wdJournal.Entries[1].EntryType)  // custodial CR
}

func TestCapitalBundle_Idempotent(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallCapitalBundle(ctx, cs, jts, ts))
	require.NoError(t, InstallCapitalBundle(ctx, cs, jts, ts))
}
