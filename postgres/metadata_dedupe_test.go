package postgres_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// TestMetadata_DedupeIsTheUniqueConstraint is J-14's pin.
//
// The eight metadata create/deactivate endpoints send no idempotency key and
// read none, while `@azex/ledger-react`'s use-metadata hooks were generating
// one per call under a comment claiming api-contract.md §9 compliance -- a
// key the server parsed away in silence, so a timed-out retry really did
// attempt a second write. Two ways to close that: build a receipt table for
// config-plane writes, or state plainly which mechanism does the
// deduplication and check that it exists.
//
// The second is the honest answer here, and this test is what makes it a
// claim rather than a hope:
//
//   - create is deduplicated by the `code` UNIQUE constraint. A retry does
//     not create a second row; it returns core.ErrConflict (HTTP 409 / code
//     10901). That is the property financial.md's idempotency rule exists to
//     protect -- no duplicate side effect -- reached by a constraint instead
//     of by a key.
//   - deactivate is idempotent by state: is_active = false twice is the same
//     state, so a retry needs no key either.
//
// docs/openapi.yaml and docs/api.md now say exactly this per endpoint. If a
// future change makes a duplicate create succeed (a dropped constraint, or a
// store that swallows the violation), this test goes red and the
// documentation stops being true at the same moment.
func TestMetadata_DedupeIsTheUniqueConstraint(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)

	t.Run("classification create is deduplicated by code", func(t *testing.T) {
		input := core.ClassificationInput{
			Code: "dedupe_class", Name: "Dedupe Class", NormalSide: core.NormalSideDebit,
			BalanceRole: core.BalanceRoleAvailable,
		}
		first, err := classStore.CreateClassification(ctx, input)
		require.NoError(t, err)

		_, err = classStore.CreateClassification(ctx, input)
		require.ErrorIs(t, err, core.ErrConflict,
			"a repeated create must conflict, not insert a second classification with the same code")

		all, err := classStore.ListClassifications(ctx, false)
		require.NoError(t, err)
		var seen int
		for _, c := range all {
			if c.Code == input.Code {
				seen++
			}
		}
		require.Equal(t, 1, seen, "exactly one row for the code -- this is the dedupe the endpoint relies on")
		require.NotEmpty(t, first.UID)
	})

	t.Run("currency create is deduplicated by code", func(t *testing.T) {
		input := core.CurrencyInput{Code: "DEDUPE", Name: "Dedupe Currency", Exponent: 2}
		_, err := currencyStore.CreateCurrency(ctx, input)
		require.NoError(t, err)
		_, err = currencyStore.CreateCurrency(ctx, input)
		require.ErrorIs(t, err, core.ErrConflict)
	})

	t.Run("journal type create is deduplicated by code", func(t *testing.T) {
		input := core.JournalTypeInput{Code: "dedupe_jt", Name: "Dedupe JT"}
		_, err := classStore.CreateJournalType(ctx, input)
		require.NoError(t, err)
		_, err = classStore.CreateJournalType(ctx, input)
		require.ErrorIs(t, err, core.ErrConflict)
	})

	t.Run("deactivate is idempotent by state", func(t *testing.T) {
		cls, err := classStore.CreateClassification(ctx, core.ClassificationInput{
			Code: "dedupe_deact", Name: "Dedupe Deactivate", NormalSide: core.NormalSideDebit,
			BalanceRole: core.BalanceRoleAvailable,
		})
		require.NoError(t, err)

		require.NoError(t, classStore.DeactivateClassification(ctx, cls.UID))
		require.NoError(t, classStore.DeactivateClassification(ctx, cls.UID),
			"deactivating twice is the same state, so the retry needs no idempotency key")
	})
}
