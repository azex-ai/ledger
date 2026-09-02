package ledger_test

// Pins for B-X1: Deactivate* must actually deactivate. Driven through
// ledger.New(pool) -- the facade a consumer holds -- because the whole
// finding is that the three Deactivate* methods were reachable, documented,
// and connected to nothing on the write path.

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
)

// TestDeactivatedCurrency_RefusesNewJournals pins the currency arm.
func TestDeactivatedCurrency_RefusesNewJournals(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	svc, err := ledger.New(pool)
	require.NoError(t, err)
	dims := seedBarrierDims(t, pool)
	ctx := context.Background()

	// Works before deactivation.
	_, err = svc.JournalWriter().PostJournal(ctx, dims.journal(postgrestest.UniqueKey("cur-before"), time.Time{}))
	require.NoError(t, err)

	require.NoError(t, svc.Currencies().DeactivateCurrency(ctx, dims.currencyUID))

	_, err = svc.JournalWriter().PostJournal(ctx, dims.journal(postgrestest.UniqueKey("cur-after"), time.Time{}))
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)
	assert.Contains(t, err.Error(), "deactivated")

	// History still reads: a soft delete keeps the books, it only closes the
	// write path.
	bal, err := svc.BalanceReader().GetBalance(ctx, 1, dims.currencyUID, dims.userClsUID)
	require.NoError(t, err)
	assert.True(t, bal.Equal(decimal.NewFromInt(10)), "got %s", bal)
}

// TestDeactivatedClassification_RefusesNewJournals pins the classification
// arm -- including a system-side classification, which is where the money
// actually sits.
func TestDeactivatedClassification_RefusesNewJournals(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	svc, err := ledger.New(pool)
	require.NoError(t, err)
	dims := seedBarrierDims(t, pool)
	ctx := context.Background()

	require.NoError(t, svc.Classifications().DeactivateClassification(ctx, dims.systemClsUID))

	_, err = svc.JournalWriter().PostJournal(ctx, dims.journal(postgrestest.UniqueKey("cls-after"), time.Time{}))
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)
	assert.Contains(t, err.Error(), "deactivated")
}

// TestDeactivatedJournalType_RefusesNewJournals pins the journal-type arm.
func TestDeactivatedJournalType_RefusesNewJournals(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	svc, err := ledger.New(pool)
	require.NoError(t, err)
	dims := seedBarrierDims(t, pool)
	ctx := context.Background()

	require.NoError(t, svc.JournalTypes().DeactivateJournalType(ctx, dims.journalTypeUID))

	_, err = svc.JournalWriter().PostJournal(ctx, dims.journal(postgrestest.UniqueKey("jt-after"), time.Time{}))
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)
	assert.Contains(t, err.Error(), "deactivated")
}

// TestDeactivate_UnknownUIDIsNotFound pins the other half of the silent
// no-op: deactivating something that does not exist used to return nil.
func TestDeactivate_UnknownUIDIsNotFound(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	svc, err := ledger.New(pool)
	require.NoError(t, err)
	ctx := context.Background()

	const ghost = "11111111-1111-1111-1111-111111111111"

	assert.ErrorIs(t, svc.Currencies().DeactivateCurrency(ctx, ghost), core.ErrNotFound)
	assert.ErrorIs(t, svc.Classifications().DeactivateClassification(ctx, ghost), core.ErrNotFound)
	assert.ErrorIs(t, svc.JournalTypes().DeactivateJournalType(ctx, ghost), core.ErrNotFound)
	assert.ErrorIs(t, svc.Templates().DeactivateTemplate(ctx, ghost), core.ErrNotFound)
}
