package postgres_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres"
)

func TestClassificationStore_CRUD(t *testing.T) {
	pool := setupTestDB(t)
	store := postgres.NewClassificationStore(pool)
	ctx := context.Background()

	cls, err := store.CreateClassification(ctx, core.ClassificationInput{
		Code:       "main_wallet",
		Name:       "Main Wallet",
		NormalSide: core.NormalSideDebit,
		IsSystem:   false,
	})
	require.NoError(t, err)
	assert.Equal(t, "main_wallet", cls.Code)
	assert.True(t, cls.IsActive)

	// List active only
	list, err := store.ListClassifications(ctx, true)
	require.NoError(t, err)
	assert.Len(t, list, 1)

	// Deactivate
	err = store.DeactivateClassification(ctx, cls.ID)
	require.NoError(t, err)

	// Active-only should be empty now
	list, err = store.ListClassifications(ctx, true)
	require.NoError(t, err)
	assert.Len(t, list, 0)

	// Include inactive should still show it
	list, err = store.ListClassifications(ctx, false)
	require.NoError(t, err)
	assert.Len(t, list, 1)
	assert.False(t, list[0].IsActive)
}

func TestJournalTypeStore_CRUD(t *testing.T) {
	pool := setupTestDB(t)
	store := postgres.NewClassificationStore(pool)
	ctx := context.Background()

	jt, err := store.CreateJournalType(ctx, core.JournalTypeInput{
		Code: "deposit",
		Name: "Deposit",
	})
	require.NoError(t, err)
	assert.Equal(t, "deposit", jt.Code)
	assert.True(t, jt.IsActive)

	list, err := store.ListJournalTypes(ctx, true)
	require.NoError(t, err)
	assert.Len(t, list, 1)

	err = store.DeactivateJournalType(ctx, jt.ID)
	require.NoError(t, err)

	list, err = store.ListJournalTypes(ctx, true)
	require.NoError(t, err)
	assert.Len(t, list, 0)

	list, err = store.ListJournalTypes(ctx, false)
	require.NoError(t, err)
	assert.Len(t, list, 1)
}

func TestCurrencyStore_CRUD(t *testing.T) {
	pool := setupTestDB(t)
	store := postgres.NewCurrencyStore(pool)
	ctx := context.Background()

	cur, err := store.CreateCurrency(ctx, core.CurrencyInput{
		Code: "USDT",
		Name: "Tether USD",
	})
	require.NoError(t, err)
	assert.Equal(t, "USDT", cur.Code)

	got, err := store.GetCurrency(ctx, cur.ID)
	require.NoError(t, err)
	assert.Equal(t, cur.ID, got.ID)

	list, err := store.ListCurrencies(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 1)
}

func TestTemplateStore_CRUD(t *testing.T) {
	pool := setupTestDB(t)
	tmplStore := postgres.NewTemplateStore(pool)
	ctx := context.Background()

	jtID := seedJournalType(t, pool, "deposit", "Deposit")
	clsID := seedClassification(t, pool, "wallet", "Wallet", "debit", false)

	tmpl, err := tmplStore.CreateTemplate(ctx, core.TemplateInput{
		Code:          "deposit_confirm",
		Name:          "Deposit Confirm",
		JournalTypeID: jtID,
		Lines: []core.TemplateLineInput{
			{
				ClassificationID: clsID,
				EntryType:        core.EntryTypeDebit,
				HolderRole:       core.HolderRoleUser,
				AmountKey:        "amount",
				SortOrder:        1,
			},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "deposit_confirm", tmpl.Code)
	assert.Len(t, tmpl.Lines, 1)

	got, err := tmplStore.GetTemplate(ctx, "deposit_confirm")
	require.NoError(t, err)
	assert.Equal(t, tmpl.ID, got.ID)
	assert.Len(t, got.Lines, 1)

	list, err := tmplStore.ListTemplates(ctx, true)
	require.NoError(t, err)
	assert.Len(t, list, 1)

	err = tmplStore.DeactivateTemplate(ctx, tmpl.ID)
	require.NoError(t, err)

	list, err = tmplStore.ListTemplates(ctx, true)
	require.NoError(t, err)
	assert.Len(t, list, 0)
}
