package postgres_test

// Partition-management tests (I-13: partition coverage is total, now with
// active monthly partitions — migration 037 + PartitionStore).

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// After migrations, the monthly partition for the current month must exist
// (created by 037's horizon) and the default partition must be empty.
func TestPartitions_MigrationCreatesHorizon(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	now := time.Now().UTC()
	currentPart := fmt.Sprintf("journal_entries_y%04dm%02d", now.Year(), int(now.Month()))

	var exists bool
	require.NoError(t, pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", currentPart).Scan(&exists))
	assert.True(t, exists, "migration 037 must pre-create the current month partition %s", currentPart)

	var hasRows bool
	require.NoError(t, pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM journal_entries_default)").Scan(&hasRows))
	assert.False(t, hasRows, "default partition must be empty after 037")
}

// EnsureMonthlyPartitions is idempotent and extends the horizon; entries
// posted inside the horizon land in monthly partitions, not the default.
func TestPartitions_EnsureMonthlyPartitions(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	store := postgres.NewPartitionStore(pool)

	// Far-future anchor so we exercise creation regardless of 037's horizon.
	anchor := time.Now().UTC().AddDate(1, 0, 0)
	created, err := store.EnsureMonthlyPartitions(ctx, anchor, 2)
	require.NoError(t, err)
	assert.Len(t, created, 3, "anchor month + 2 ahead")

	// Second run: nothing new.
	created, err = store.EnsureMonthlyPartitions(ctx, anchor, 2)
	require.NoError(t, err)
	assert.Empty(t, created)

	// A normally posted journal (created_at = now, inside 037's horizon)
	// must route to a monthly partition, leaving the default empty.
	ledgerStore, deps := setupInvariantsFixture(t, pool, ctx)
	_, err = ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: deps.JournalType,
		IdempotencyKey: postgrestest.UniqueKey("part-route"),
		Source:         "partition-test",
		Entries: []core.EntryInput{
			{AccountHolder: 8101, CurrencyUID: deps.Currency, ClassificationUID: deps.MainWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(10)},
			{AccountHolder: core.SystemAccountHolder(8101), CurrencyUID: deps.Currency, ClassificationUID: deps.Custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(10)},
		},
	})
	require.NoError(t, err)

	hasRows, err := store.DefaultPartitionHasRows(ctx)
	require.NoError(t, err)
	assert.False(t, hasRows, "in-horizon entries must land in a monthly partition")
}

// Rows stranded in the default partition (horizon lapsed) are rebalanced
// into monthly partitions by EnsureMonthlyPartitions' fallback path.
func TestPartitions_RebalanceStrandedDefaultRows(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	store := postgres.NewPartitionStore(pool)

	ledgerStore, deps := setupInvariantsFixture(t, pool, ctx)

	// Post a normal, balanced journal to copy from.
	orig, err := ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: deps.JournalType,
		IdempotencyKey: postgrestest.UniqueKey("part-orig"),
		Source:         "partition-test",
		Entries: []core.EntryInput{
			{AccountHolder: 8102, CurrencyUID: deps.Currency, ClassificationUID: deps.MainWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(25)},
			{AccountHolder: core.SystemAccountHolder(8102), CurrencyUID: deps.Currency, ClassificationUID: deps.Custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(25)},
		},
	})
	require.NoError(t, err)

	// Simulate a lapsed horizon: clone the journal with created_at in a
	// far-future month that has no partition, so its entries land in
	// journal_entries_default. The clone copies journals generically (temp
	// table) to stay robust against column additions; entries are re-pointed
	// and re-dated. Balance is preserved, so the deferred currency-balance
	// trigger passes at commit.
	stranded := time.Date(time.Now().UTC().Year()+3, 6, 15, 12, 0, 0, 0, time.UTC)
	require.NoError(t, pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var origID int64
		if err := tx.QueryRow(ctx, "SELECT id FROM journals WHERE uid = $1", orig.UID).Scan(&origID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "CREATE TEMP TABLE j_copy ON COMMIT DROP AS SELECT * FROM journals WHERE id = $1", origID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE j_copy SET id = nextval(pg_get_serial_sequence('journals','id')),
			                   uid = gen_random_uuid(),
			                   idempotency_key = idempotency_key || '-stranded',
			                   created_at = $1`, stranded); err != nil {
			return err
		}
		var newID int64
		if err := tx.QueryRow(ctx, "WITH ins AS (INSERT INTO journals SELECT * FROM j_copy RETURNING id) SELECT id FROM ins").Scan(&newID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at, effective_at)
			 SELECT $1, account_holder, currency_id, classification_id, entry_type, amount, $2, effective_at
			   FROM journal_entries WHERE journal_id = $3`, newID, stranded, origID)
		return err
	}))

	hasRows, err := store.DefaultPartitionHasRows(ctx)
	require.NoError(t, err)
	require.True(t, hasRows, "rows dated %s must land in default (no partition covers it)", stranded)

	// Ensure for that month — direct create fails on overlap, fallback
	// rebalance must move the stranded rows into the new monthly partition.
	created, err := store.EnsureMonthlyPartitions(ctx, stranded, 1)
	require.NoError(t, err)
	assert.NotEmpty(t, created)

	hasRows, err = store.DefaultPartitionHasRows(ctx)
	require.NoError(t, err)
	assert.False(t, hasRows, "stranded rows must be rebalanced out of default")

	strandedPart := fmt.Sprintf("journal_entries_y%04dm%02d", stranded.Year(), int(stranded.Month()))
	var n int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM "+strandedPart).Scan(&n))
	assert.Equal(t, 2, n, "both entries of the stranded journal must live in %s", strandedPart)
}
