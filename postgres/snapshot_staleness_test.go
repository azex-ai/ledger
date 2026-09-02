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

// TestRollupAdapter_GetSnapshotBalances_BackdatedEntryInvalidatesCache pins a
// Major from the 2026-08-25 financial-engineering audit
// (postgres/sql/queries/checkpoints.sql:166-186, ListBalancesAt;
// financial-correctness.md): a balance_snapshots row is written once, from
// whatever entries existed at that moment. Nothing re-triggers it when a
// later write retroactively backdates (effective_at earlier than the
// snapshot's own creation) into an already-snapshotted business date — the
// snapshot silently keeps reporting the stale total forever, even though the
// live effective_at-keyed balance (ListBalancesAt) is correct.
//
// This is the exact minimal repro from the report: snapshot day D at 100,
// then a journal effective on day D lands (written) after the snapshot was
// taken. GetSnapshotBalances(D) must reflect the true 150, not the cached
// 100.
func TestRollupAdapter_GetSnapshotBalances_BackdatedEntryInvalidatesCache(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ledgerStore := postgres.NewLedgerStore(pool)
	adapter := postgres.NewRollupAdapter(pool)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT-SNAP", "Tether USD Snapshot")
	jtID := postgrestest.SeedJournalType(t, pool, "transfer_snap", "Transfer Snap")
	wallet := postgrestest.SeedClassification(t, pool, "wallet_snap", "Wallet Snap", "debit", false)
	custodial := postgrestest.SeedClassification(t, pool, "custodial_snap", "Custodial Snap", "credit", true)

	holder := int64(9201)
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -3)

	// 1. Post a journal effective within day D.
	_, err := ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("snap-stale-first"),
		EffectiveAt:    day.Add(6 * time.Hour),
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: wallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: curID, ClassificationUID: custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	})
	require.NoError(t, err)

	// Sanity: the live as-of balance for day D is 100 before the snapshot.
	cutoff := day.AddDate(0, 0, 1)
	live, err := adapter.ListBalancesAt(ctx, cutoff)
	require.NoError(t, err)
	assert.True(t, findBalance(live, holder, curID, wallet).Equal(decimal.NewFromInt(100)))

	// 2. Snapshot day D — as service/snapshot.go's CreateDailySnapshot would,
	// but written directly here to control ordering precisely.
	require.NoError(t, adapter.UpsertSnapshot(ctx, core.BalanceSnapshot{
		AccountHolder:     holder,
		CurrencyUID:       curID,
		ClassificationUID: wallet,
		SnapshotDate:      day,
		Balance:           decimal.NewFromInt(100),
	}))

	// Confirm the naive read (no backdated entry yet) returns the snapshot as
	// written -- staleness detection must not fire when there is nothing
	// stale.
	before, err := adapter.GetSnapshotBalances(ctx, holder, curID, day)
	require.NoError(t, err)
	assert.True(t, findBalance(before, holder, curID, wallet).Equal(decimal.NewFromInt(100)))

	// 3. A retroactive posting lands AFTER the snapshot was taken, but
	// attributed (effective_at) to day D — already inside the snapshotted
	// window.
	_, err = ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("snap-stale-backdated"),
		EffectiveAt:    day.Add(18 * time.Hour),
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: wallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(50)},
			{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: curID, ClassificationUID: custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(50)},
		},
	})
	require.NoError(t, err)

	// The true as-of balance for day D is now 150.
	live, err = adapter.ListBalancesAt(ctx, cutoff)
	require.NoError(t, err)
	assert.True(t, findBalance(live, holder, curID, wallet).Equal(decimal.NewFromInt(150)))

	// 4. The snapshot read must reflect the true 150, not the cached 100 --
	// this is the assertion that was false before the fix.
	after, err := adapter.GetSnapshotBalances(ctx, holder, curID, day)
	require.NoError(t, err)
	got := findBalance(after, holder, curID, wallet)
	assert.True(t, got.Equal(decimal.NewFromInt(150)), "snapshot read must self-heal on a backdated entry: got %s, want 150", got)
}

func findBalance(balances []core.Balance, holder int64, currencyUID, classificationUID string) decimal.Decimal {
	for _, b := range balances {
		if b.AccountHolder == holder && b.CurrencyUID == currencyUID && b.ClassificationUID == classificationUID {
			return b.Balance
		}
	}
	return decimal.Zero
}

// TestSnapshotSelfHeal_AllAsOfReadersAgree pins the 2026-09-02 audit's A-M5:
// the self-healing the test above proves for GetSnapshotBalances was the ONLY
// self-healing there was, and it was row-driven.
//
// Two holes, one test:
//
//  1. GetBalanceTrends reads balance_snapshots through its own SQL and never
//     touched RollupAdapter, so /holders/{h}/trends -- a user-facing endpoint
//     -- kept serving pre-backdating values for every day except today.
//     Measured: GetSnapshotBalances 150, GetBalanceTrends 100, ground truth
//  150. Two reads of the same business date, disagreeing.
//  2. The self-heal looped over the CACHED ROWS, so a dimension the backdated
//     write itself introduced was invisible to it: a pending position of 70
//     opened by the backdated journal was simply absent from the output.
//
// The assertion is deliberately a three-way agreement rather than three
// separate expected numbers: any reader of a business date must return what
// journal_entries actually says as of that date, and the way to keep a fourth
// reader from reopening this is to state the property, not the values.
func TestSnapshotSelfHeal_AllAsOfReadersAgree(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	adapter := postgres.NewRollupAdapter(pool)
	trends := postgres.NewBalanceTrendsStore(pool, ledgerStore)

	curID := postgrestest.SeedCurrency(t, pool, "USDT-HEAL", "Tether USD Heal")
	jtID := postgrestest.SeedJournalType(t, pool, "transfer_heal", "Transfer Heal")
	wallet := postgrestest.SeedClassificationWithRole(t, pool, "wallet_heal", "Wallet Heal", "debit", false, "available")
	pending := postgrestest.SeedClassificationWithRole(t, pool, "pending_heal", "Pending Heal", "credit", false, "pending")
	custodial := postgrestest.SeedClassification(t, pool, "custodial_heal", "Custodial Heal", "credit", true)

	holder := int64(9301)
	sys := core.SystemAccountHolder(holder)
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -2)
	cutoff := day.AddDate(0, 0, 1)

	// A journal effective within day D, written now.
	_, err := ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("heal-first"),
		EffectiveAt:    day.Add(6 * time.Hour),
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: wallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: sys, CurrencyUID: curID, ClassificationUID: custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	})
	require.NoError(t, err)

	// Snapshot day D from what exists right now.
	balances, err := adapter.ListBalancesAt(ctx, cutoff)
	require.NoError(t, err)
	for _, b := range balances {
		if b.AccountHolder != holder {
			continue
		}
		require.NoError(t, adapter.UpsertSnapshot(ctx, core.BalanceSnapshot{
			AccountHolder:     b.AccountHolder,
			CurrencyUID:       b.CurrencyUID,
			ClassificationUID: b.ClassificationUID,
			SnapshotDate:      day,
			Balance:           b.Balance,
		}))
	}

	// Now backdate INTO the already-snapshotted day: +50 on the existing
	// wallet dimension, and +70 on a pending dimension that had no snapshot
	// row at all because it had no entries when the snapshot ran.
	time.Sleep(10 * time.Millisecond) // created_at must be strictly later
	_, err = ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtID,
		IdempotencyKey: postgrestest.UniqueKey("heal-backdated"),
		EffectiveAt:    day.Add(8 * time.Hour),
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: wallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(50)},
			{AccountHolder: sys, CurrencyUID: curID, ClassificationUID: custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(50)},
			{AccountHolder: holder, CurrencyUID: curID, ClassificationUID: pending, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(70)},
			{AccountHolder: sys, CurrencyUID: curID, ClassificationUID: custodial, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(70)},
		},
	})
	require.NoError(t, err)

	// --- Ground truth: recomputed from journal_entries, no cache involved. ---
	truth := map[string]decimal.Decimal{}
	live, err := adapter.ListBalancesAt(ctx, cutoff)
	require.NoError(t, err)
	for _, b := range live {
		if b.AccountHolder == holder {
			truth[b.ClassificationUID] = b.Balance
		}
	}
	require.Len(t, truth, 2, "fixture must produce two holder dimensions as of day D")
	assert.True(t, truth[wallet].Equal(decimal.NewFromInt(150)))
	assert.True(t, truth[pending].Equal(decimal.NewFromInt(70)))

	// --- Reader 1: GetSnapshotBalances. Must include the dimension the
	// backdated write created, not only the ones already cached. ---
	snap, err := adapter.GetSnapshotBalances(ctx, holder, curID, day)
	require.NoError(t, err)
	got := map[string]decimal.Decimal{}
	for _, b := range snap {
		got[b.ClassificationUID] = b.Balance
	}
	assert.Len(t, got, len(truth), "GetSnapshotBalances lost a dimension: %v vs truth %v", got, truth)
	for uid, want := range truth {
		assert.Truef(t, got[uid].Equal(want),
			"GetSnapshotBalances[%s]: want %s got %s", uid, want, got[uid])
	}

	// --- Reader 2: GetBalanceTrends, per dimension. ---
	for uid, want := range truth {
		points, err := trends.GetBalanceTrends(ctx, core.BalanceTrendFilter{
			AccountHolder:     holder,
			CurrencyUID:       curID,
			ClassificationUID: uid,
			From:              day,
			Until:             day,
		})
		require.NoError(t, err)
		require.Len(t, points, 1)
		assert.Truef(t, points[0].Balance.Equal(want),
			"GetBalanceTrends[%s] on day D: want %s got %s -- the trends endpoint reads balance_snapshots too and must self-heal identically",
			uid, want, points[0].Balance)
	}
}
