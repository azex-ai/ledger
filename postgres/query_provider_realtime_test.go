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
)

func TestQueryStore_GetSystemRollups_RealtimeReflectsUnrolledJournal(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	classStore := postgres.NewClassificationStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)
	queryStore := postgres.NewQueryStore(pool)

	usdt, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{
		Code: "USDT-QS-RT", Name: "Tether QueryStore RT",
	})
	require.NoError(t, err)

	custodial, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "custodial", Name: "Custodial QueryStore RT", NormalSide: core.NormalSideDebit, IsSystem: true,
	})
	require.NoError(t, err)

	mainWallet, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: "main_wallet_qs_rt", Name: "Main Wallet QueryStore RT", NormalSide: core.NormalSideCredit,
	})
	require.NoError(t, err)

	jt, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{
		Code: "deposit_qs_rt", Name: "Deposit QueryStore RT",
	})
	require.NoError(t, err)

	userID := int64(9101)
	sysID := core.SystemAccountHolder(userID)

	_, err = ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jt.ID,
		IdempotencyKey: postgrestest.UniqueKey("qs-rt"),
		Entries: []core.EntryInput{
			{AccountHolder: sysID, CurrencyID: usdt.ID, ClassificationID: custodial.ID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(500)},
			{AccountHolder: userID, CurrencyID: usdt.ID, ClassificationID: mainWallet.ID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(500)},
		},
		Source: "test",
	})
	require.NoError(t, err)

	var checkpointCount int
	require.NoError(t,
		pool.QueryRow(ctx,
			"SELECT count(*) FROM balance_checkpoints WHERE currency_id = $1 AND account_holder IN ($2, $3)",
			usdt.ID, userID, sysID,
		).Scan(&checkpointCount),
	)
	require.Zero(t, checkpointCount, "test invalid: checkpoints should not exist before rollup runs")

	rollups, err := queryStore.GetSystemRollups(ctx)
	require.NoError(t, err)
	require.Len(t, rollups, 2, "realtime system balances should include both classifications immediately")

	byClass := make(map[int64]core.SystemRollup, len(rollups))
	for _, r := range rollups {
		byClass[r.ClassificationID] = r
		assert.Equal(t, usdt.ID, r.CurrencyID)
		assert.False(t, r.UpdatedAt.IsZero())
		assert.WithinDuration(t, time.Now(), r.UpdatedAt, 5*time.Second)
	}

	assert.True(t, byClass[custodial.ID].TotalBalance.Equal(decimal.NewFromInt(500)))
	assert.True(t, byClass[mainWallet.ID].TotalBalance.Equal(decimal.NewFromInt(500)))
}
