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
		CurrencyUID:    "cur-1",
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
//
// ⚠️ On its own this says almost nothing about a template. Two legs sharing
// one amount key are balanced whichever way round their entry types are, so
// every preset whose only guard was this helper had its DIRECTION unchecked --
// which is how three shipped presets went out with the sign reversed and how
// both audits that looked at them found green tests over wrong accounting
// (2026-08-25 C7, 2026-09-02 A-C1 / A-M2 / A-M4). Use assertJournalEffect
// instead wherever the effect on each account is what matters, and keep this
// one only for "does it balance at all".
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

// assertJournalEffect is the direction-aware assertion assertBalanced cannot
// make: it resolves every leg's classification, applies the ledger's single
// sign authority (core.SignedAmount, docs/INVARIANTS.md I-43) and compares the
// resulting per-account effect against what the caller says the template is
// supposed to DO.
//
// want is keyed "<classification code>/<user|system>" and valued as a decimal
// string, signed: positive means "this account's balance goes up by that
// much". Every leg of the journal must be accounted for and every declared
// key must be produced, so adding or dropping a leg fails the test too.
//
// Writing expectations this way is deliberate: a reviewer reads "custodial
// increases by 1000" rather than "entries[0].EntryType == credit", and the
// second form is exactly what let the pins certify the reversed directions.
func assertJournalEffect(
	t *testing.T,
	cs *fakeClassificationStore,
	holderID int64,
	entries []core.EntryInput,
	want map[string]string,
) {
	t.Helper()
	assertBalanced(t, entries)

	byUID := make(map[string]*core.Classification, len(cs.classifications))
	for _, c := range cs.classifications {
		byUID[c.UID] = c
	}

	got := make(map[string]decimal.Decimal, len(want))
	for i, e := range entries {
		cls, ok := byUID[e.ClassificationUID]
		if !ok {
			t.Fatalf("entry[%d]: unknown classification uid %q", i, e.ClassificationUID)
		}
		signed, err := core.SignedAmount(cls.NormalSide, e.EntryType, e.Amount)
		require.NoErrorf(t, err, "entry[%d] (%s)", i, cls.Code)

		side := "system"
		if e.AccountHolder == holderID {
			side = "user"
		}
		key := cls.Code + "/" + side
		got[key] = got[key].Add(signed)
	}

	for key, raw := range want {
		expected := decimal.RequireFromString(raw)
		actual, ok := got[key]
		if !ok {
			t.Errorf("no leg touched %s (want effect %s); journal produced %v", key, expected, effectStrings(got))
			continue
		}
		assert.Truef(t, expected.Equal(actual),
			"%s: want effect %s, got %s (whole journal: %v)", key, expected, actual, effectStrings(got))
		delete(got, key)
	}
	for key, actual := range got {
		t.Errorf("unexpected leg: %s moved by %s and the test did not declare it", key, actual)
	}
}

func effectStrings(m map[string]decimal.Decimal) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v.String()
	}
	return out
}
