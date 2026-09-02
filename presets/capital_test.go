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

	// equity is DEBIT-normal. It reads backwards against standard
	// accounting's A = L + E on purpose: a capital injection increases equity
	// AND custodial, custodial is credit-normal, and two accounts that
	// increase in the same journal must carry opposite normal_sides for it to
	// balance. See presets/capital.go's header.
	equity, err := cs.GetByCode(ctx, "equity")
	require.NoError(t, err)
	assert.Equal(t, core.NormalSideDebit, equity.NormalSide)
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
		CurrencyUID:    "cur-1",
		IdempotencyKey: "cap-inj-1",
		Amounts:        map[string]decimal.Decimal{"amount": amount},
	}

	// This assertion is the inverse of what it used to be, and the reason is
	// worth stating where the test lives. It used to read
	//
	//	assert.Equal(t, core.EntryTypeDebit, injJournal.Entries[0].EntryType) // custodial DR
	//
	// three lines below an assertion that custodial is credit-normal -- so
	// the test knew the polarity and still demanded the leg that contradicts
	// it. Injecting 1000 of platform capital drove the custody figure DOWN by
	// 1000 and pinned SolvencyCheck at solvent=false permanently (2026-09-02
	// audit A-C1). The pin is inverted rather than deleted so the disproof
	// stays in the record.
	//
	// capital_injection: custodial +amount, equity +amount.
	injTmpl, err := ts.GetTemplate(ctx, "capital_injection")
	require.NoError(t, err)
	injJournal, err := injTmpl.Render(params)
	require.NoError(t, err)
	assertJournalEffect(t, cs, params.HolderID, injJournal.Entries, map[string]string{
		"custodial/system": amount.String(),
		"equity/system":    amount.String(),
	})

	// capital_withdraw: the exact inverse -- custodial -amount, equity -amount.
	params.IdempotencyKey = "cap-wd-1"
	wdTmpl, err := ts.GetTemplate(ctx, "capital_withdraw")
	require.NoError(t, err)
	wdJournal, err := wdTmpl.Render(params)
	require.NoError(t, err)
	assertJournalEffect(t, cs, params.HolderID, wdJournal.Entries, map[string]string{
		"custodial/system": amount.Neg().String(),
		"equity/system":    amount.Neg().String(),
	})
}

func TestCapitalBundle_Idempotent(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallCapitalBundle(ctx, cs, jts, ts))
	require.NoError(t, InstallCapitalBundle(ctx, cs, jts, ts))
}
