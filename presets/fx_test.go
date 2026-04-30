package presets

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

func TestFXBundle_Classifications(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallFXBundle(ctx, cs, jts, ts))

	settlement, err := cs.GetByCode(ctx, "settlement")
	require.NoError(t, err)
	assert.Equal(t, core.NormalSideCredit, settlement.NormalSide)
	assert.True(t, settlement.IsSystem)

	mw, err := cs.GetByCode(ctx, "main_wallet")
	require.NoError(t, err)
	assert.Equal(t, core.NormalSideDebit, mw.NormalSide)
	assert.False(t, mw.IsSystem)
}

func TestFXBundle_JournalTypes(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallFXBundle(ctx, cs, jts, ts))

	for _, code := range []string{"fx_sell", "fx_buy"} {
		jt, err := jts.GetJournalTypeByCode(ctx, code)
		require.NoError(t, err, "journal type %q must be installed", code)
		assert.True(t, jt.IsActive)
	}
}

// Each leg balances independently (per-currency invariant). FX is by
// definition multi-currency, so each leg must be self-contained.
func TestFXBundle_Templates_Balance(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallFXBundle(ctx, cs, jts, ts))

	sell, err := ts.GetTemplate(ctx, "fx_sell")
	require.NoError(t, err)
	buy, err := ts.GetTemplate(ctx, "fx_buy")
	require.NoError(t, err)

	// User sells 100 USD (currency 1)
	sellInput, err := sell.Render(core.TemplateParams{
		HolderID:       42,
		CurrencyID:     1,
		IdempotencyKey: "fx-quote-7-sell",
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(100)},
		Metadata:       map[string]string{"fx_rate": "0.92", "quote_id": "q-7", "side": "sell"},
	})
	require.NoError(t, err)
	assertBalanced(t, sellInput.Entries)
	assert.Equal(t, "0.92", sellInput.Metadata["fx_rate"])

	// User buys 92 EUR (currency 2)
	buyInput, err := buy.Render(core.TemplateParams{
		HolderID:       42,
		CurrencyID:     2,
		IdempotencyKey: "fx-quote-7-buy",
		Amounts:        map[string]decimal.Decimal{"amount": decimal.RequireFromString("92")},
		Metadata:       map[string]string{"fx_rate": "0.92", "quote_id": "q-7", "side": "buy"},
	})
	require.NoError(t, err)
	assertBalanced(t, buyInput.Entries)
	assert.Equal(t, "0.92", buyInput.Metadata["fx_rate"])
}

// fx_sell is the user-side debit-out: main_wallet (user) credit, settlement
// (system) debit. Pin the direction so an accidental flip is caught.
func TestFXBundle_FXSell_RoutesUserOutToSettlement(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallFXBundle(ctx, cs, jts, ts))

	sell, err := ts.GetTemplate(ctx, "fx_sell")
	require.NoError(t, err)

	got, err := sell.Render(core.TemplateParams{
		HolderID:       42,
		CurrencyID:     1,
		IdempotencyKey: "fx-sell-direction",
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(100)},
	})
	require.NoError(t, err)
	require.Len(t, got.Entries, 2)

	mw := findEntryByHolder(got.Entries, 42)
	require.NotNil(t, mw, "main_wallet entry on user holder must exist")
	assert.Equal(t, core.EntryTypeCredit, mw.EntryType, "user main_wallet must be credited (funds leaving user)")

	settle := findEntryByHolder(got.Entries, core.SystemAccountHolder(42))
	require.NotNil(t, settle, "settlement entry on system holder must exist")
	assert.Equal(t, core.EntryTypeDebit, settle.EntryType, "system settlement must be debited (platform absorbs CCY-A)")
}

// fx_buy is the mirror: settlement (system) credit, main_wallet (user) debit.
func TestFXBundle_FXBuy_RoutesSettlementToUser(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallFXBundle(ctx, cs, jts, ts))

	buy, err := ts.GetTemplate(ctx, "fx_buy")
	require.NoError(t, err)

	got, err := buy.Render(core.TemplateParams{
		HolderID:       42,
		CurrencyID:     2,
		IdempotencyKey: "fx-buy-direction",
		Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(92)},
	})
	require.NoError(t, err)

	mw := findEntryByHolder(got.Entries, 42)
	require.NotNil(t, mw)
	assert.Equal(t, core.EntryTypeDebit, mw.EntryType, "user main_wallet must be debited (funds arriving)")

	settle := findEntryByHolder(got.Entries, core.SystemAccountHolder(42))
	require.NotNil(t, settle)
	assert.Equal(t, core.EntryTypeCredit, settle.EntryType, "system settlement must be credited (platform releases CCY-B)")
}

func TestFXBundle_Idempotent(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallFXBundle(ctx, cs, jts, ts))
	require.NoError(t, InstallFXBundle(ctx, cs, jts, ts))
}

// FXBundle composes cleanly with TransferBundle since both reference
// settlement. Installing transfer first must not break FX install.
func TestFXBundle_ComposesWithTransfer(t *testing.T) {
	ctx := context.Background()
	cs := newFakeClassificationStore()
	jts := newFakeJournalTypeStore()
	ts := newFakeTemplateStore()

	require.NoError(t, InstallTransferBundle(ctx, cs, jts, ts))
	require.NoError(t, InstallFXBundle(ctx, cs, jts, ts))

	// settlement classification must still match (system + credit-normal).
	settle, err := cs.GetByCode(ctx, "settlement")
	require.NoError(t, err)
	assert.True(t, settle.IsSystem)
	assert.Equal(t, core.NormalSideCredit, settle.NormalSide)
}

func findEntryByHolder(entries []core.EntryInput, holder int64) *core.EntryInput {
	for i := range entries {
		if entries[i].AccountHolder == holder {
			return &entries[i]
		}
	}
	return nil
}
