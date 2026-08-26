package postgres_test

// Benchmarks for the postgres adapter. Run with:
//
//	go test ./postgres/ -bench=. -benchtime=5s -run=^$
//
// Skipped automatically when Docker is unavailable. Numbers are dependent on
// the host (CPU, IO, container overhead) — use them for relative comparison
// across changes, not as absolute SLO targets.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// BenchmarkPostJournal_SingleAccount measures end-to-end PostJournal latency
// for a 2-entry balanced journal hitting the same account dimension on every
// iteration. This is the worst case for advisory-lock / row-lock contention
// inside the rollup queue.
func BenchmarkPostJournal_SingleAccount(b *testing.B) {
	pool := setupBenchPool(b)
	store, deps := setupBenchFixture(b, pool)

	const userID int64 = 9001

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		_, err := store.PostJournal(context.Background(), core.JournalInput{
			JournalTypeUID: deps.JournalType,
			IdempotencyKey: postgrestest.UniqueKey("bench-single"),
			Source:         "bench",
			Entries: []core.EntryInput{
				{AccountHolder: userID, CurrencyUID: deps.Currency, ClassificationUID: deps.MainWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(1)},
				{AccountHolder: core.SystemAccountHolder(userID), CurrencyUID: deps.Currency, ClassificationUID: deps.Custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(1)},
			},
		})
		if err != nil {
			b.Fatal(err, i)
		}
	}
}

// BenchmarkPostJournal_FanoutAccounts spreads each iteration across a
// different user, eliminating same-account lock contention. Compared to
// _SingleAccount, the gap shows pure DB / advisory-lock overhead.
func BenchmarkPostJournal_FanoutAccounts(b *testing.B) {
	pool := setupBenchPool(b)
	store, deps := setupBenchFixture(b, pool)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		userID := int64(10_000 + i)
		_, err := store.PostJournal(context.Background(), core.JournalInput{
			JournalTypeUID: deps.JournalType,
			IdempotencyKey: postgrestest.UniqueKey(fmt.Sprintf("bench-fanout-%d", i)),
			Source:         "bench",
			Entries: []core.EntryInput{
				{AccountHolder: userID, CurrencyUID: deps.Currency, ClassificationUID: deps.MainWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(1)},
				{AccountHolder: core.SystemAccountHolder(userID), CurrencyUID: deps.Currency, ClassificationUID: deps.Custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(1)},
			},
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGetBalance_ColdCheckpoint measures the worst-case balance read
// path: an account whose checkpoint was advanced once and now has K entries
// of delta on top. Tests how the LATERAL-join delta sum scales.
func BenchmarkGetBalance_ColdCheckpoint(b *testing.B) {
	pool := setupBenchPool(b)
	store, deps := setupBenchFixture(b, pool)

	const userID int64 = 9100
	const deltaJournals = 100

	// Seed: post `deltaJournals` journals on the same account dimension.
	for i := range deltaJournals {
		_, err := store.PostJournal(context.Background(), core.JournalInput{
			JournalTypeUID: deps.JournalType,
			IdempotencyKey: postgrestest.UniqueKey(fmt.Sprintf("seed-%d", i)),
			Source:         "bench-seed",
			Entries: []core.EntryInput{
				{AccountHolder: userID, CurrencyUID: deps.Currency, ClassificationUID: deps.MainWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(1)},
				{AccountHolder: core.SystemAccountHolder(userID), CurrencyUID: deps.Currency, ClassificationUID: deps.Custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(1)},
			},
		})
		if err != nil {
			b.Fatal(err)
		}
	}

	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		_, err := store.GetBalance(context.Background(), userID, deps.Currency, deps.MainWallet)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkListComputedBalancesForHolders measures the batch balance query
// (postgres/sql/queries/checkpoints.sql's ListComputedBalancesForHolders,
// via LedgerStore.BatchGetBalances) that backs GetBalances / BatchGetBalances
// / GetBalanceBreakdown -- this is the hot path W3-sign's normal_side sign
// convergence (I-42) put behind ledger_signed_amount(). Uncheckpointed on
// purpose: every holder's balance is entirely delta (no balance_checkpoints
// row), so the SUM(ledger_signed_amount(...)) CASE-or-function-call runs
// once per journal_entries row summed, the worst case for measuring its
// per-row cost. See the migration 009 comment for why the function is
// deliberately not STRICT (a STRICT LANGUAGE SQL function is never inlined
// by the planner, which this benchmark is what caught).
func BenchmarkListComputedBalancesForHolders(b *testing.B) {
	pool := setupBenchPool(b)
	store, deps := setupBenchFixture(b, pool)

	const numHolders = 50
	const entriesPerHolder = 20
	holderIDs := make([]int64, numHolders)
	for h := range numHolders {
		holderID := int64(9300 + h)
		holderIDs[h] = holderID
		for i := range entriesPerHolder {
			_, err := store.PostJournal(context.Background(), core.JournalInput{
				JournalTypeUID: deps.JournalType,
				IdempotencyKey: postgrestest.UniqueKey(fmt.Sprintf("bench-batch-seed-%d-%d", h, i)),
				Source:         "bench-seed",
				Entries: []core.EntryInput{
					{AccountHolder: holderID, CurrencyUID: deps.Currency, ClassificationUID: deps.MainWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(1)},
					{AccountHolder: core.SystemAccountHolder(holderID), CurrencyUID: deps.Currency, ClassificationUID: deps.Custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(1)},
				},
			})
			if err != nil {
				b.Fatal(err)
			}
		}
	}

	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := store.BatchGetBalances(context.Background(), holderIDs, deps.Currency); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReserveSettle measures the per-iteration cost of a full
// reserve→settle cycle, the critical path for any reserve/settle billing
// flow. Includes advisory lock + balance check + reservation FSM transition.
func BenchmarkReserveSettle(b *testing.B) {
	pool := setupBenchPool(b)
	store, deps := setupBenchFixture(b, pool)
	reserver := postgres.NewReserverStore(pool, store, postgres.NewVerifiedBalanceStore(pool, nil))

	// Reserve only counts classifications tagged role=available (the shared
	// fixture's main_wallet is role-less on purpose), so seed a dedicated
	// available-role wallet for the reserve path.
	classStore := postgres.NewClassificationStore(pool)
	wallet, err := classStore.CreateClassification(context.Background(), core.ClassificationInput{
		Code: fmt.Sprintf("bench_avail_%d", time.Now().UnixNano()), Name: "Bench Available Wallet",
		NormalSide: core.NormalSideDebit, BalanceRole: core.BalanceRoleAvailable,
	})
	if err != nil {
		b.Fatal(err)
	}

	const userID int64 = 9200
	// Top up enough that thousands of reservations don't drain it.
	_, err = store.PostJournal(context.Background(), core.JournalInput{
		JournalTypeUID: deps.JournalType,
		IdempotencyKey: postgrestest.UniqueKey("bench-rsv-seed"),
		Source:         "bench-seed",
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyUID: deps.Currency, ClassificationUID: wallet.UID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(1_000_000)},
			{AccountHolder: core.SystemAccountHolder(userID), CurrencyUID: deps.Currency, ClassificationUID: deps.Custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(1_000_000)},
		},
	})
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		rsv, err := reserver.Reserve(context.Background(), core.ReserveInput{
			AccountHolder:  userID,
			CurrencyUID:    deps.Currency,
			Amount:         decimal.NewFromInt(1),
			IdempotencyKey: postgrestest.UniqueKey("bench-rsv"),
		})
		if err != nil {
			b.Fatal(err)
		}
		if err := reserver.Settle(context.Background(), core.SettleInput{ReservationUID: rsv.UID, Amount: decimal.NewFromInt(1), IdempotencyKey: postgrestest.UniqueKey("bench-settle")}); err != nil {
			b.Fatal(err)
		}
	}
}

func setupBenchPool(b *testing.B) *pgxpool.Pool {
	b.Helper()
	// Reuse the same fixture helper as integration tests; it skips on no-Docker.
	return postgrestest.SetupDB(b)
}

func setupBenchFixture(b *testing.B, pool *pgxpool.Pool) (*postgres.LedgerStore, invariantsFixture) {
	b.Helper()
	return setupInvariantsFixture(b, pool, context.Background())
}
