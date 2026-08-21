package postgres_test

// Pins for I-1/I-24 (P3, C1 + M1 fixes):
//   - the DB-layer deferred constraint trigger restored by migration 044
//     (docs/plans/2026-08-21-tamper-evident-ledger-design.md §2 C1, §5), and
//   - the fleet-wide "journal_dr_cr" per-journal reconcile scan that
//     complements it (§2 M1).
//
// Both tests drive golang-migrate directly (postgrestest.SetupRawDB +
// postgres.NewMigrationSource, the same pattern as
// migrate_populated_test.go's TestMigrate_PopulatedDatabase) so they can
// demonstrate the exact "before 044 this succeeds, after 044 this fails"
// contrast that a fixed-version SetupDB cannot show.

import (
	"context"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// preTriggerVersion is the last migration version before 044 restored the
// DB-layer per-journal balance trigger. Chosen as the highest version that
// predates this task's own migrations (041) rather than the logically
// preceding one (043, owned by a parallel task landing separately) so this
// test works whether run against this branch in isolation or against a
// fully merged main -- it only cares that 044 has not run yet.
const preTriggerVersion = 41

// TestJournalBalanceTrigger_RejectsDirectSQLImbalance pins I-1/I-24: before
// migration 044, a direct SQL INSERT that leaves an existing journal
// unbalanced by currency succeeds with nothing in the database to stop it
// (C1). After 044, the identical attack against a fresh journal is rejected
// at commit.
func TestJournalBalanceTrigger_RejectsDirectSQLImbalance(t *testing.T) {
	ctx := context.Background()
	connStr := postgrestest.SetupRawDB(t)
	migrateURL := strings.Replace(connStr, "postgres://", "pgx5://", 1)

	source, err := postgres.NewMigrationSource()
	require.NoError(t, err)
	m, err := migrate.NewWithSourceInstance("iofs", source, migrateURL)
	require.NoError(t, err)

	require.NoError(t, m.Migrate(preTriggerVersion))

	pool, err := pgxpool.New(ctx, connStr)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	store, deps := setupInvariantsFixture(t, pool, ctx)

	// A legitimate, balanced journal, inserted directly against the
	// schema-v41 shape rather than through store.PostJournal: the current
	// binary's InsertJournal sends event_id as NULL (migration 045 made the
	// column a nullable FK-target-exception), which schema v41's
	// NOT-NULL-DEFAULT-0 event_id column rejects. This test's subject is the
	// journal_entries balance trigger, not journal creation, so a direct
	// insert matching the historical schema is the correct fixture here.
	journalAID := insertLegacyBalancedJournal(t, pool, ctx, deps.JournalType, 4201, deps.Currency, deps.MainWallet, deps.Custodial, decimal.NewFromInt(100), postgrestest.UniqueKey("bal-trig-pre"))

	currencyID := postgrestest.InternalID(t, pool, "currencies", deps.Currency)
	classID := postgrestest.InternalID(t, pool, "classifications", deps.MainWallet)

	// Pre-044: a direct SQL insert that unbalances journalA (an extra debit
	// leg with no matching credit) succeeds -- nothing in the DB stops it.
	_, err = pool.Exec(ctx,
		`INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
		 VALUES ($1, $2, $3, $4, 'debit', $5)`,
		journalAID, 4201, currencyID, classID, decimal.NewFromInt(50),
	)
	require.NoError(t, err, "before migration 044, a direct SQL insert could unbalance a journal with nothing in the DB to stop it (C1)")

	// Bring the schema up to date -- this is what restores the trigger. It
	// is NOT retroactive: journalA above stays corrupted (that gap is
	// exactly why the fleet-wide check in the sibling test exists).
	require.NoError(t, m.Up())

	// Post-044: a second, independent balanced journal.
	journalB, err := store.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: deps.JournalType,
		IdempotencyKey: postgrestest.UniqueKey("bal-trig-post"),
		Source:         "trigger-test",
		Entries: []core.EntryInput{
			{AccountHolder: 4202, CurrencyUID: deps.Currency, ClassificationUID: deps.MainWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(200)},
			{AccountHolder: -4202, CurrencyUID: deps.Currency, ClassificationUID: deps.Custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(200)},
		},
	})
	require.NoError(t, err)
	journalBID := postgrestest.InternalID(t, pool, "journals", journalB.UID)

	// The identical direct-SQL attack against journalB must now be rejected
	// at commit by the restored deferred constraint trigger.
	_, err = pool.Exec(ctx,
		`INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
		 VALUES ($1, $2, $3, $4, 'debit', $5)`,
		journalBID, 4202, currencyID, classID, decimal.NewFromInt(50),
	)
	require.Error(t, err, "after migration 044, a direct SQL insert that unbalances a journal by currency must be rejected by the DB layer")
	assert.Contains(t, err.Error(), "unbalanced entries", "the trigger's error message should name the failure")
}

// TestUnbalancedJournalsFleetScan_CatchesWhatGlobalEqualityMisses pins I-24 /
// M1: the fleet-wide "journal_dr_cr" scan (IntegrityUnbalancedJournalsCount)
// catches two individually-unbalanced journals that a GLOBAL debit==credit
// equality check cannot see, because summed together debit still equals
// credit. The corrupted journals are crafted pre-044 (direct SQL, schema
// v41) -- exactly the historical-gap scenario the fleet scan exists for,
// since migration 044's trigger is not retroactive.
func TestUnbalancedJournalsFleetScan_CatchesWhatGlobalEqualityMisses(t *testing.T) {
	ctx := context.Background()
	connStr := postgrestest.SetupRawDB(t)
	migrateURL := strings.Replace(connStr, "postgres://", "pgx5://", 1)

	source, err := postgres.NewMigrationSource()
	require.NoError(t, err)
	m, err := migrate.NewWithSourceInstance("iofs", source, migrateURL)
	require.NoError(t, err)

	require.NoError(t, m.Migrate(preTriggerVersion))

	pool, err := pgxpool.New(ctx, connStr)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, deps := setupInvariantsFixture(t, pool, ctx)

	// Journal A and B: legitimate 2-leg posts (150 debit / 150 credit each),
	// inserted directly against the schema-v41 shape rather than through
	// store.PostJournal -- see TestJournalBalanceTrigger_RejectsDirectSQLImbalance's
	// comment on insertLegacyBalancedJournal for why. A direct-SQL extra
	// debit/credit leg is added to each below to unbalance them individually.
	journalAID := insertLegacyBalancedJournal(t, pool, ctx, deps.JournalType, 4301, deps.Currency, deps.MainWallet, deps.Custodial, decimal.NewFromInt(150), postgrestest.UniqueKey("fleet-a"))
	journalBID := insertLegacyBalancedJournal(t, pool, ctx, deps.JournalType, 4302, deps.Currency, deps.MainWallet, deps.Custodial, decimal.NewFromInt(150), postgrestest.UniqueKey("fleet-b"))

	currencyID := postgrestest.InternalID(t, pool, "currencies", deps.Currency)
	classID := postgrestest.InternalID(t, pool, "classifications", deps.MainWallet)
	sysClassID := postgrestest.InternalID(t, pool, "classifications", deps.Custodial)

	_, err = pool.Exec(ctx,
		`INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
		 VALUES ($1, $2, $3, $4, 'debit', $5)`,
		journalAID, 4301, currencyID, classID, decimal.NewFromInt(50),
	)
	require.NoError(t, err, "pre-044: crafting the corrupted fixture must itself succeed")

	_, err = pool.Exec(ctx,
		`INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
		 VALUES ($1, $2, $3, $4, 'credit', $5)`,
		journalBID, -4302, currencyID, sysClassID, decimal.NewFromInt(50),
	)
	require.NoError(t, err, "pre-044: crafting the corrupted fixture must itself succeed")

	// Bring the schema up to date. Not retroactive -- journalA/journalB stay
	// corrupted, which is the point: this is the historical-gap scenario.
	require.NoError(t, m.Up())

	// The GLOBAL debit==credit equality check is blind to this: summed
	// across both journals, debit == credit still holds.
	var totalDebit, totalCredit decimal.Decimal
	err = pool.QueryRow(ctx, `
		SELECT
		  COALESCE(SUM(CASE WHEN entry_type='debit' THEN amount END), 0),
		  COALESCE(SUM(CASE WHEN entry_type='credit' THEN amount END), 0)
		FROM journal_entries
		WHERE journal_id IN ($1, $2)
	`, journalAID, journalBID).Scan(&totalDebit, &totalCredit)
	require.NoError(t, err)
	require.True(t, totalDebit.Equal(totalCredit),
		"the crafted fixture must be globally balanced (debit=%s credit=%s) -- that's the whole point of M1", totalDebit, totalCredit)

	// The fleet-wide per-journal scan must catch both individually.
	adapter := postgres.NewReconcileAdapter(pool)
	count, err := adapter.UnbalancedJournalsCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), count, "both journal A and journal B must be flagged individually, even though they net to zero globally")

	samples, err := adapter.UnbalancedJournalsSample(ctx)
	require.NoError(t, err)
	var sawA, sawB bool
	for _, s := range samples {
		switch s.JournalID {
		case journalAID:
			sawA = true
			assert.True(t, s.Drift.Equal(decimal.NewFromInt(50)), "journal A should drift +50, got %s", s.Drift)
		case journalBID:
			sawB = true
			assert.True(t, s.Drift.Equal(decimal.NewFromInt(-50)), "journal B should drift -50, got %s", s.Drift)
		}
	}
	assert.True(t, sawA, "expected journal A in the sample")
	assert.True(t, sawB, "expected journal B in the sample")
}

// insertLegacyBalancedJournal inserts a balanced 2-leg journal directly,
// matching the journals/journal_entries shape as of schema v41 (event_id
// still NOT NULL DEFAULT 0, no FK) rather than going through
// postgres.LedgerStore.PostJournal. The current binary's InsertJournal
// always sends event_id as NULL when unset (migration 045 converted the
// column to a nullable FK-target exception with a real FK to events(id)),
// which a NOT-NULL event_id column rejects -- so the current binary's
// PostJournal cannot run against a database still at v41. Both callers in
// this file need a legitimate journal fixture at exactly that pre-044
// schema state, which is what this helper is for.
func insertLegacyBalancedJournal(
	t *testing.T,
	pool *pgxpool.Pool,
	ctx context.Context,
	journalTypeUID string,
	holder int64,
	currencyUID, walletUID, custodialUID string,
	amount decimal.Decimal,
	idempotencyKey string,
) int64 {
	t.Helper()

	var journalID int64
	err := pool.QueryRow(ctx,
		`INSERT INTO journals (uid, journal_type_id, idempotency_key, total_debit, total_credit, actor_id, source, event_id)
		 VALUES (gen_random_uuid(), (SELECT id FROM journal_types WHERE uid=$1::uuid), $2, $3, $3, 0, 'test', 0)
		 RETURNING id`,
		journalTypeUID, idempotencyKey, amount.String(),
	).Scan(&journalID)
	require.NoError(t, err)

	_, err = pool.Exec(ctx,
		`INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
		 VALUES ($1, $2, (SELECT id FROM currencies WHERE uid=$3::uuid), (SELECT id FROM classifications WHERE uid=$4::uuid), 'debit', $5)`,
		journalID, holder, currencyUID, walletUID, amount.String(),
	)
	require.NoError(t, err)

	_, err = pool.Exec(ctx,
		`INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
		 VALUES ($1, $2, (SELECT id FROM currencies WHERE uid=$3::uuid), (SELECT id FROM classifications WHERE uid=$4::uuid), 'credit', $5)`,
		journalID, -holder, currencyUID, custodialUID, amount.String(),
	)
	require.NoError(t, err)

	return journalID
}
