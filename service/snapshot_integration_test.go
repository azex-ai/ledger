package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

// TestSnapshotSparse_SameBalanceTwoDays verifies that when the balance does not
// change between two consecutive days, only one snapshot row is written.
func TestSnapshotSparse_SameBalanceTwoDays(t *testing.T) {
	pgpool := postgrestest.SetupDB(t)
	ctx := context.Background()

	extra := postgres.NewSnapshotExtraStore(pgpool)

	currencyID := postgrestest.SeedCurrency(t, pgpool, "USDT", "Tether USD")
	classID := postgrestest.SeedClassification(t, pgpool, "wallet_s", "Wallet Sparse", "debit", false)
	holderID := int64(1001)

	day1 := time.Date(2025, 1, 10, 0, 0, 0, 0, time.UTC)
	snap1 := core.BalanceSnapshot{
		AccountHolder:    holderID,
		CurrencyID:       currencyID,
		ClassificationID: classID,
		SnapshotDate:     day1,
		Balance:          decimal.NewFromInt(500),
	}
	inserted, err := extra.UpsertSnapshotSparse(ctx, snap1)
	require.NoError(t, err)
	assert.True(t, inserted, "day 1 snapshot should be inserted (no prior snapshot)")

	// Day 2: same balance — should be skipped by sparse logic.
	day2 := day1.AddDate(0, 0, 1)
	snap2 := core.BalanceSnapshot{
		AccountHolder:    holderID,
		CurrencyID:       currencyID,
		ClassificationID: classID,
		SnapshotDate:     day2,
		Balance:          decimal.NewFromInt(500),
	}
	inserted, err = extra.UpsertSnapshotSparse(ctx, snap2)
	require.NoError(t, err)
	assert.False(t, inserted, "day 2 snapshot should be skipped (balance unchanged)")

	var count int
	err = pgpool.QueryRow(ctx,
		"SELECT COUNT(*) FROM balance_snapshots WHERE account_holder=$1 AND currency_id=$2 AND classification_id=$3",
		holderID, currencyID, classID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "exactly 1 snapshot row expected")

	// Day 3: balance changes → should be inserted.
	day3 := day2.AddDate(0, 0, 1)
	snap3 := core.BalanceSnapshot{
		AccountHolder:    holderID,
		CurrencyID:       currencyID,
		ClassificationID: classID,
		SnapshotDate:     day3,
		Balance:          decimal.NewFromInt(600),
	}
	inserted, err = extra.UpsertSnapshotSparse(ctx, snap3)
	require.NoError(t, err)
	assert.True(t, inserted, "day 3 snapshot should be inserted (balance changed)")

	err = pgpool.QueryRow(ctx,
		"SELECT COUNT(*) FROM balance_snapshots WHERE account_holder=$1 AND currency_id=$2 AND classification_id=$3",
		holderID, currencyID, classID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 2, count, "2 snapshot rows expected after balance change on day 3")
}

// seedJournal inserts a balanced debit+credit journal pair at the given timestamp
// and returns the journal ID.
func seedJournal(
	t *testing.T,
	pgpool *pgxpool.Pool,
	jtID, holderID, currencyID, classID, sysClassID int64,
	amount decimal.Decimal,
	at time.Time,
	ikey string,
) int64 {
	t.Helper()
	ctx := context.Background()
	var jID int64
	err := pgpool.QueryRow(ctx,
		`INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit, actor_id, source, event_id, created_at)
		 VALUES ($1, $2, $3, $3, 0, 'test', 0, $4) RETURNING id`,
		jtID, ikey, amount.String(), at,
	).Scan(&jID)
	require.NoError(t, err)

	_, err = pgpool.Exec(ctx,
		`INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at)
		 VALUES ($1,$2,$3,$4,'debit',$5,$6)`,
		jID, holderID, currencyID, classID, amount.String(), at,
	)
	require.NoError(t, err)

	_, err = pgpool.Exec(ctx,
		`INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at)
		 VALUES ($1,$2,$3,$4,'credit',$5,$6)`,
		jID, -holderID, currencyID, sysClassID, amount.String(), at,
	)
	require.NoError(t, err)

	return jID
}

// TestBackfill_FiveDays creates journals spanning 5 days with no snapshots,
// backfills, and verifies each day with a distinct balance gets a snapshot row.
func TestBackfill_FiveDays(t *testing.T) {
	pgpool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rollup := postgres.NewRollupAdapter(pgpool)
	extra := postgres.NewSnapshotExtraStore(pgpool)
	engine := core.NewEngine()
	backfillSvc := service.NewSnapshotBackfillService(rollup, extra, extra, engine)

	currencyID := postgrestest.SeedCurrency(t, pgpool, "USDC", "USD Coin")
	classID := postgrestest.SeedClassification(t, pgpool, "wallet_bf", "Wallet Backfill", "debit", false)
	sysClassID := postgrestest.SeedClassification(t, pgpool, "custodial_bf", "Custodial Backfill", "credit", true)
	jtID := postgrestest.SeedJournalType(t, pgpool, "bf_deposit", "Backfill Deposit")
	holderID := int64(2001)

	baseDay := time.Date(2025, 2, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		at := baseDay.AddDate(0, 0, i)
		amount := decimal.NewFromInt(100 * (int64(i) + 1))
		ikey := postgrestest.UniqueKey("bf-dep")
		seedJournal(t, pgpool, jtID, holderID, currencyID, classID, sysClassID, amount, at, ikey)
	}

	// Confirm no snapshots exist yet.
	count, err := extra.CountSnapshots(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), count)

	fromDate := time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)
	toDate := time.Date(2025, 2, 5, 0, 0, 0, 0, time.UTC)
	result, err := backfillSvc.BackfillSnapshots(ctx, fromDate, toDate)
	require.NoError(t, err)
	require.Empty(t, result.Errors, "no errors expected during backfill")
	assert.Equal(t, 5, result.DaysProcessed)
	assert.Greater(t, result.SnapshotsCreated, 0, "at least one snapshot created")

	// Balance grows each day so each day should have its own snapshot row for the holder.
	var holderSnaps int
	err = pgpool.QueryRow(ctx,
		"SELECT COUNT(*) FROM balance_snapshots WHERE account_holder=$1 AND currency_id=$2 AND classification_id=$3",
		holderID, currencyID, classID,
	).Scan(&holderSnaps)
	require.NoError(t, err)
	assert.Equal(t, 5, holderSnaps, "5 snapshot rows expected (balance increases each day)")
}

// TestAdvisoryLock_SkipWhenLockHeld verifies that CreateDailySnapshot returns
// nil without writing when another connection already holds the advisory lock
// for the same date.
func TestAdvisoryLock_SkipWhenLockHeld(t *testing.T) {
	pgpool := postgrestest.SetupDB(t)
	ctx := context.Background()

	rollup := postgres.NewRollupAdapter(pgpool)
	extra := postgres.NewSnapshotExtraStore(pgpool)
	engine := core.NewEngine()

	currencyID := postgrestest.SeedCurrency(t, pgpool, "BTC", "Bitcoin")
	classID := postgrestest.SeedClassification(t, pgpool, "wallet_lock", "Wallet Lock", "debit", false)
	sysClassID := postgrestest.SeedClassification(t, pgpool, "custodial_lock", "Custodial Lock", "credit", true)
	jtID := postgrestest.SeedJournalType(t, pgpool, "lock_deposit", "Lock Deposit")
	holderID := int64(3001)

	snapshotDate := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	at := snapshotDate.Add(6 * time.Hour)

	amount := decimal.NewFromInt(250)
	seedJournal(t, pgpool, jtID, holderID, currencyID, classID, sysClassID, amount, at,
		postgrestest.UniqueKey("lock-j"))

	// Build the same advisory lock key that the service uses.
	// The service uses FNV-64a; we replicate that here.
	lockKey := advisoryLockKeyTest("snapshot:" + snapshotDate.Format("20060102"))

	// Acquire the lock from a separate connection to simulate the "other pod".
	conn, err := pgpool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()

	var acquired bool
	err = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", lockKey).Scan(&acquired)
	require.NoError(t, err)
	require.True(t, acquired, "test must be able to acquire the advisory lock first")

	// The snapshot service should see the lock held and skip without error.
	svc := service.NewSnapshotService(rollup, rollup, engine).
		WithPool(pgpool).
		WithSparseSnapshotter(extra)

	err = svc.CreateDailySnapshot(ctx, snapshotDate)
	require.NoError(t, err, "CreateDailySnapshot should return nil when lock is held by another connection")

	// No snapshots should have been written (the service skipped).
	var rowCount int
	err = pgpool.QueryRow(ctx,
		"SELECT COUNT(*) FROM balance_snapshots WHERE account_holder=$1",
		holderID,
	).Scan(&rowCount)
	require.NoError(t, err)
	assert.Equal(t, 0, rowCount, "no snapshot rows expected (service skipped due to held lock)")

	// Release the lock.
	_, err = conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockKey)
	require.NoError(t, err)

	// Now with the lock free the service should write the snapshot.
	err = svc.CreateDailySnapshot(ctx, snapshotDate)
	require.NoError(t, err)

	err = pgpool.QueryRow(ctx,
		"SELECT COUNT(*) FROM balance_snapshots WHERE account_holder=$1",
		holderID,
	).Scan(&rowCount)
	require.NoError(t, err)
	assert.Greater(t, rowCount, 0, "snapshot rows expected after lock released")
}

// advisoryLockKeyTest replicates the FNV-64a key derivation used by SnapshotService
// so the test can pre-acquire the same lock.
func advisoryLockKeyTest(name string) int64 {
	// Import hash/fnv inline to avoid importing the service package (external test).
	const offset64 = uint64(14695981039346656037)
	const prime64 = uint64(1099511628211)
	h := offset64
	for i := 0; i < len(name); i++ {
		h ^= uint64(name[i])
		h *= prime64
	}
	return int64(h)
}
