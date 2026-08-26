package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

// TestRollupAdapter_AggregateCheckpointsByClassification_SumsAcrossHolders
// pins AggregateCheckpointsByClassification's actual GROUP BY semantics
// (postgres/sql/queries/checkpoints.sql) against real Postgres: the query
// must sum balance_checkpoints across every account_holder that shares a
// (currency_id, classification_id) pair, which is exactly what
// system_rollups (the platform-wide solvency figure) depends on.
//
// Before this test, the only real-Postgres path exercising this query
// (service/reconcile_full_integration_test.go's
// TestFullReconciliation_DetectsSystemRollupDriftFromPoisonedCheckpoint) used
// a single holder throughout, so a GROUP BY that accidentally included
// account_holder (making every row its own group instead of aggregating)
// would still pass every test in the repo -- the only test whose name
// promises multi-account coverage
// (TestSystemRollupService_MultipleAccounts) mocks
// AggregateCheckpointsByClassification itself and never runs the real SQL.
func TestRollupAdapter_AggregateCheckpointsByClassification_SumsAcrossHolders(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	adapter := postgres.NewRollupAdapter(pool)

	curUID := postgrestest.SeedCurrency(t, pool, postgrestest.UniqueKey("AGG"), "Aggregate Currency")
	otherCurUID := postgrestest.SeedCurrency(t, pool, postgrestest.UniqueKey("AGGOTHER"), "Other Currency")
	classUID := postgrestest.SeedClassification(t, pool, postgrestest.UniqueKey("agg_class"), "Aggregate Classification", "debit", true)

	var currencyID, otherCurrencyID, classificationID int64
	require.NoError(t, pool.QueryRow(ctx, "SELECT id FROM currencies WHERE uid=$1", curUID).Scan(&currencyID))
	require.NoError(t, pool.QueryRow(ctx, "SELECT id FROM currencies WHERE uid=$1", otherCurUID).Scan(&otherCurrencyID))
	require.NoError(t, pool.QueryRow(ctx, "SELECT id FROM classifications WHERE uid=$1", classUID).Scan(&classificationID))

	now := time.Now()

	// Two DIFFERENT holders, same (currency, classification) -- must be
	// summed into one aggregate row: 100 + 250 = 350.
	require.NoError(t, adapter.UpsertCheckpoint(ctx, service.BalanceCheckpoint{
		AccountHolder: 60101, CurrencyID: currencyID, ClassificationID: classificationID,
		Balance: decimal.NewFromInt(100), LastEntryID: 1, LastEntryAt: now,
	}))
	require.NoError(t, adapter.UpsertCheckpoint(ctx, service.BalanceCheckpoint{
		AccountHolder: 60102, CurrencyID: currencyID, ClassificationID: classificationID,
		Balance: decimal.NewFromInt(250), LastEntryID: 1, LastEntryAt: now,
	}))

	// A third checkpoint for one of the same holders, but under a DIFFERENT
	// currency -- must land in its own aggregate row and never bleed into
	// the sum above.
	require.NoError(t, adapter.UpsertCheckpoint(ctx, service.BalanceCheckpoint{
		AccountHolder: 60101, CurrencyID: otherCurrencyID, ClassificationID: classificationID,
		Balance: decimal.NewFromInt(9_999), LastEntryID: 1, LastEntryAt: now,
	}))

	rollups, err := adapter.AggregateCheckpointsByClassification(ctx)
	require.NoError(t, err)

	var found, foundOther *core.SystemRollup
	for i := range rollups {
		switch {
		case rollups[i].CurrencyUID == curUID && rollups[i].ClassificationUID == classUID:
			found = &rollups[i]
		case rollups[i].CurrencyUID == otherCurUID && rollups[i].ClassificationUID == classUID:
			foundOther = &rollups[i]
		}
	}

	require.NotNil(t, found, "expected an aggregate row for the seeded (currency, classification) pair")
	assert.True(t, found.TotalBalance.Equal(decimal.NewFromInt(350)),
		"two DIFFERENT holders' checkpoints (100+250) under the same (currency, classification) must sum to 350, got %s -- if this is 100 or 250 instead, the GROUP BY is accidentally keyed per-holder",
		found.TotalBalance)

	require.NotNil(t, foundOther, "expected a SEPARATE aggregate row for the other currency")
	assert.True(t, foundOther.TotalBalance.Equal(decimal.NewFromInt(9_999)),
		"the other-currency checkpoint must not be folded into the first currency's sum, got %s", foundOther.TotalBalance)
}
