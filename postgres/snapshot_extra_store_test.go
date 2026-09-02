package postgres_test

// F-M3 (2026-09-02 audit): SnapshotExtraStore.MergeWithLive had zero test
// coverage anywhere in the repo -- it is a pure consumer-facing API (no
// production code inside this library calls it), so its whole body could be
// swapped for `return nil, nil` and `go test ./...` stayed green.

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

// TestSnapshotExtraStore_MergeWithLive_SplicesTodayFromCheckpoint pins
// F-M3: given one historic snapshot day and today's live checkpoint balance,
// MergeWithLive over [historicDay, today] must return both -- the historic
// row as stored, and today synthesised from the live checkpoint (not the
// snapshot table, which has no row for today at all). A no-op
// implementation (`return nil, nil`) returns neither and goes red on the
// length assertion alone.
func TestSnapshotExtraStore_MergeWithLive_SplicesTodayFromCheckpoint(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	_, deps := setupInvariantsFixture(t, pool, ctx)
	rollup := postgres.NewRollupAdapter(pool)
	extra := postgres.NewSnapshotExtraStore(pool)

	holder := int64(9401)
	historicDay := time.Date(time.Now().UTC().Year(), time.Now().UTC().Month(), time.Now().UTC().Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -2)
	today := time.Date(time.Now().UTC().Year(), time.Now().UTC().Month(), time.Now().UTC().Day(), 0, 0, 0, 0, time.UTC)

	// A historic snapshot row, written the normal way.
	require.NoError(t, rollup.UpsertSnapshot(ctx, core.BalanceSnapshot{
		AccountHolder:     holder,
		CurrencyUID:       deps.Currency,
		ClassificationUID: deps.MainWallet,
		SnapshotDate:      historicDay,
		Balance:           decimal.NewFromInt(100),
	}))

	// Today's checkpoint, written directly (as the rollup worker would),
	// deliberately a value no journal entry in this test produces -- proves
	// MergeWithLive reads the checkpoint, not derives a number independently.
	curInternal := postgrestest.InternalID(t, pool, "currencies", deps.Currency)
	clsInternal := postgrestest.InternalID(t, pool, "classifications", deps.MainWallet)
	require.NoError(t, rollup.UpsertCheckpoint(ctx, service.BalanceCheckpoint{
		AccountHolder:    holder,
		CurrencyID:       curInternal,
		ClassificationID: clsInternal,
		Balance:          decimal.NewFromInt(250),
		LastEntryID:      0,
		LastEntryAt:      time.Now().UTC(),
	}))

	got, err := extra.MergeWithLive(ctx, holder, deps.Currency, historicDay, today)
	require.NoError(t, err)
	require.Len(t, got, 2, "must return the historic day and today's live-checkpoint row")

	byDate := map[string]core.BalanceSnapshot{}
	for _, s := range got {
		byDate[s.SnapshotDate.Format("2006-01-02")] = s
	}
	require.Contains(t, byDate, historicDay.Format("2006-01-02"))
	require.Contains(t, byDate, today.Format("2006-01-02"))
	assert.True(t, byDate[historicDay.Format("2006-01-02")].Balance.Equal(decimal.NewFromInt(100)),
		"historic day must come from the snapshot table unchanged")
	assert.True(t, byDate[today.Format("2006-01-02")].Balance.Equal(decimal.NewFromInt(250)),
		"today must be synthesised from the live checkpoint, not the snapshot table")
}
