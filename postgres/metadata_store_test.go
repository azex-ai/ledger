package postgres_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

func TestClassificationStore_CRUD(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	store := postgres.NewClassificationStore(pool)
	ctx := context.Background()

	cls, err := store.CreateClassification(ctx, core.ClassificationInput{
		Code:        "main_wallet",
		Name:        "Main Wallet",
		NormalSide:  core.NormalSideDebit,
		IsSystem:    false,
		BalanceRole: core.BalanceRoleAvailable,
	})
	require.NoError(t, err)
	assert.Equal(t, "main_wallet", cls.Code)
	assert.True(t, cls.IsActive)

	// List active only
	list, err := store.ListClassifications(ctx, true)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(list), 1)
	assert.Contains(t, classificationCodes(list), cls.Code)

	// Deactivate
	err = store.DeactivateClassification(ctx, cls.UID)
	require.NoError(t, err)

	// Active-only should be empty now
	list, err = store.ListClassifications(ctx, true)
	require.NoError(t, err)
	assert.NotContains(t, classificationCodes(list), cls.Code)

	// Include inactive should still show it
	list, err = store.ListClassifications(ctx, false)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(list), 1)
	found := false
	for _, item := range list {
		if item.Code == cls.Code {
			found = true
			assert.False(t, item.IsActive)
		}
	}
	assert.True(t, found)
}

func TestClassificationStore_CreateWithLifecycle(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	store := postgres.NewClassificationStore(pool)
	ctx := context.Background()

	lifecycle := &core.Lifecycle{
		Initial:  "pending",
		Terminal: []core.Status{"confirmed", "expired"},
		Transitions: map[core.Status][]core.Status{
			"pending": {"confirmed", "expired"},
		},
	}

	cls, err := store.CreateClassification(ctx, core.ClassificationInput{
		Code:       "deposit_test",
		Name:       "Deposit Test",
		NormalSide: core.NormalSideCredit,
		IsSystem:   true,
		Lifecycle:  lifecycle,
	})
	require.NoError(t, err)
	require.NotNil(t, cls.Lifecycle)
	assert.Equal(t, lifecycle.Initial, cls.Lifecycle.Initial)
}

func TestJournalTypeStore_CRUD(t *testing.T) {
	pool := postgrestest.SetupDB(t)
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

	err = store.DeactivateJournalType(ctx, jt.UID)
	require.NoError(t, err)

	list, err = store.ListJournalTypes(ctx, true)
	require.NoError(t, err)
	assert.Len(t, list, 0)

	list, err = store.ListJournalTypes(ctx, false)
	require.NoError(t, err)
	assert.Len(t, list, 1)
}

// TestJournalTypeStore_HolderKind pins the M-7 fix (docs/INVARIANTS.md
// I-44): HolderTxKindNone is tolerated at creation (unlike
// ClassificationInput.BalanceRole, refused for non-system classifications
// since the M-4 fix — see core.HolderTxKindNone's doc comment for why the
// two fields are validated differently), a garbage string is refused, an
// explicit vocabulary value round-trips, and SetHolderKind can retag a
// journal type off HolderTxKindNone (not restricted to that direction only,
// unlike SetBalanceRole).
func TestJournalTypeStore_HolderKind(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	store := postgres.NewClassificationStore(pool)
	ctx := context.Background()

	// Untagged creation is accepted — no financial computation depends on
	// this field, unlike balance_role.
	untagged, err := store.CreateJournalType(ctx, core.JournalTypeInput{Code: "ht_none", Name: "Untagged"})
	require.NoError(t, err)
	assert.Equal(t, core.HolderTxKindNone, untagged.HolderKind)

	// A recognized value round-trips.
	tagged, err := store.CreateJournalType(ctx, core.JournalTypeInput{
		Code: "ht_fee", Name: "Fee", HolderKind: core.HolderTxKindFee,
	})
	require.NoError(t, err)
	assert.Equal(t, core.HolderTxKindFee, tagged.HolderKind)

	// A garbage string is refused — the CHECK constraint and the Go-side
	// IsValid() gate agree on the same closed vocabulary.
	_, err = store.CreateJournalType(ctx, core.JournalTypeInput{
		Code: "ht_bad", Name: "Bad", HolderKind: core.HolderTxKind("not_a_real_kind"),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)

	// SetHolderKind retags an untagged row — not restricted to '' -> <kind>
	// only, unlike SetBalanceRole's expand-only upgrade guard.
	require.NoError(t, store.SetHolderKind(ctx, untagged.UID, core.HolderTxKindAdjustment))
	got, err := store.GetJournalTypeByCode(ctx, "ht_none")
	require.NoError(t, err)
	assert.Equal(t, core.HolderTxKindAdjustment, got.HolderKind)

	// And it can retag an already-tagged row too.
	require.NoError(t, store.SetHolderKind(ctx, got.UID, core.HolderTxKindWithdrawal))
	got2, err := store.GetJournalTypeByCode(ctx, "ht_none")
	require.NoError(t, err)
	assert.Equal(t, core.HolderTxKindWithdrawal, got2.HolderKind)

	// SetHolderKind refuses a garbage value the same way creation does.
	err = store.SetHolderKind(ctx, got2.UID, core.HolderTxKind("still_not_real"))
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)
}

func TestCurrencyStore_CRUD(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	store := postgres.NewCurrencyStore(pool)
	ctx := context.Background()

	cur, err := store.CreateCurrency(ctx, core.CurrencyInput{
		Code: "USDT",
		Name: "Tether USD", Exponent: 18,
	})
	require.NoError(t, err)
	assert.Equal(t, "USDT", cur.Code)
	assert.True(t, cur.IsActive)
	assert.Equal(t, int32(18), cur.Exponent)

	got, err := store.GetCurrency(ctx, cur.UID)
	require.NoError(t, err)
	assert.Equal(t, cur.UID, got.UID)
	assert.True(t, got.IsActive)
	assert.Equal(t, int32(18), got.Exponent)

	// Active-only listing shows the new currency.
	list, err := store.ListCurrencies(ctx, true)
	require.NoError(t, err)
	assert.Len(t, list, 1)

	// Deactivate (soft delete).
	err = store.DeactivateCurrency(ctx, cur.UID)
	require.NoError(t, err)

	// Active-only listing now hides it.
	list, err = store.ListCurrencies(ctx, true)
	require.NoError(t, err)
	assert.Empty(t, list)

	// Including inactive still returns it, flagged inactive.
	list, err = store.ListCurrencies(ctx, false)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.False(t, list[0].IsActive)
}

// TestCurrencyStore_CreateCurrency_RejectsInvalidExponent pins the exponent
// bound (I-16): CurrencyInput.Validate rejects anything outside [0, 18]
// before a query is even issued.
func TestCurrencyStore_CreateCurrency_RejectsInvalidExponent(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	store := postgres.NewCurrencyStore(pool)
	ctx := context.Background()

	_, err := store.CreateCurrency(ctx, core.CurrencyInput{Code: "NEG", Name: "Negative Exponent", Exponent: -1})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)

	_, err = store.CreateCurrency(ctx, core.CurrencyInput{Code: "TOOBIG", Name: "Too Big Exponent", Exponent: 19})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)
}

// TestCurrencyStore_CreateCurrency_ExponentZero pins that exponent=0 (JPY) is
// a legitimate, distinct value from "not specified" — not silently coerced
// to the loosest default.
func TestCurrencyStore_CreateCurrency_ExponentZero(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	store := postgres.NewCurrencyStore(pool)
	ctx := context.Background()

	cur, err := store.CreateCurrency(ctx, core.CurrencyInput{Code: "JPY-ZERO", Name: "Yen", Exponent: 0})
	require.NoError(t, err)
	assert.Equal(t, int32(0), cur.Exponent)
}

func TestTemplateStore_CRUD(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	tmplStore := postgres.NewTemplateStore(pool)
	ctx := context.Background()

	jtID := postgrestest.SeedJournalType(t, pool, "deposit", "Deposit")
	clsID := postgrestest.SeedClassification(t, pool, "wallet", "Wallet", "debit", false)

	tmpl, err := tmplStore.CreateTemplate(ctx, core.TemplateInput{
		Code:           "deposit_confirm",
		Name:           "Deposit Confirm",
		JournalTypeUID: jtID,
		Lines: []core.TemplateLineInput{
			{
				ClassificationUID: clsID,
				EntryType:         core.EntryTypeDebit,
				HolderRole:        core.HolderRoleUser,
				AmountKey:         "amount",
				SortOrder:         1,
			},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "deposit_confirm", tmpl.Code)
	assert.Len(t, tmpl.Lines, 1)

	got, err := tmplStore.GetTemplate(ctx, "deposit_confirm")
	require.NoError(t, err)
	assert.Equal(t, tmpl.UID, got.UID)
	assert.Len(t, got.Lines, 1)

	list, err := tmplStore.ListTemplates(ctx, true)
	require.NoError(t, err)
	assert.Len(t, list, 1)

	err = tmplStore.DeactivateTemplate(ctx, tmpl.UID)
	require.NoError(t, err)

	list, err = tmplStore.ListTemplates(ctx, true)
	require.NoError(t, err)
	assert.Len(t, list, 0)
}

func TestTemplateStore_RejectsEmptyLines(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	tmplStore := postgres.NewTemplateStore(pool)
	ctx := context.Background()

	jtID := postgrestest.SeedJournalType(t, pool, "deposit", "Deposit")

	_, err := tmplStore.CreateTemplate(ctx, core.TemplateInput{
		Code:           "broken",
		Name:           "Broken",
		JournalTypeUID: jtID,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)
}

func classificationCodes(list []core.Classification) []string {
	codes := make([]string, 0, len(list))
	for _, item := range list {
		codes = append(codes, item.Code)
	}
	return codes
}
