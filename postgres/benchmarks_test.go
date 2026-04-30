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
			JournalTypeID:  deps.JournalType,
			IdempotencyKey: postgrestest.UniqueKey("bench-single"),
			Source:         "bench",
			Entries: []core.EntryInput{
				{AccountHolder: userID, CurrencyID: deps.Currency, ClassificationID: deps.MainWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(1)},
				{AccountHolder: core.SystemAccountHolder(userID), CurrencyID: deps.Currency, ClassificationID: deps.Custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(1)},
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
			JournalTypeID:  deps.JournalType,
			IdempotencyKey: postgrestest.UniqueKey(fmt.Sprintf("bench-fanout-%d", i)),
			Source:         "bench",
			Entries: []core.EntryInput{
				{AccountHolder: userID, CurrencyID: deps.Currency, ClassificationID: deps.MainWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(1)},
				{AccountHolder: core.SystemAccountHolder(userID), CurrencyID: deps.Currency, ClassificationID: deps.Custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(1)},
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
			JournalTypeID:  deps.JournalType,
			IdempotencyKey: postgrestest.UniqueKey(fmt.Sprintf("seed-%d", i)),
			Source:         "bench-seed",
			Entries: []core.EntryInput{
				{AccountHolder: userID, CurrencyID: deps.Currency, ClassificationID: deps.MainWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(1)},
				{AccountHolder: core.SystemAccountHolder(userID), CurrencyID: deps.Currency, ClassificationID: deps.Custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(1)},
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

// BenchmarkReserveSettle measures the per-iteration cost of a full
// reserve→settle cycle, the critical path for any reserve/settle billing
// flow. Includes advisory lock + balance check + reservation FSM transition.
func BenchmarkReserveSettle(b *testing.B) {
	pool := setupBenchPool(b)
	store, deps := setupBenchFixture(b, pool)
	reserver := postgres.NewReserverStore(pool, store)

	const userID int64 = 9200
	// Top up enough that thousands of reservations don't drain it.
	_, err := store.PostJournal(context.Background(), core.JournalInput{
		JournalTypeID:  deps.JournalType,
		IdempotencyKey: postgrestest.UniqueKey("bench-rsv-seed"),
		Source:         "bench-seed",
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyID: deps.Currency, ClassificationID: deps.MainWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(1_000_000)},
			{AccountHolder: core.SystemAccountHolder(userID), CurrencyID: deps.Currency, ClassificationID: deps.Custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(1_000_000)},
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
			CurrencyID:     deps.Currency,
			Amount:         decimal.NewFromInt(1),
			IdempotencyKey: postgrestest.UniqueKey("bench-rsv"),
		})
		if err != nil {
			b.Fatal(err)
		}
		if err := reserver.Settle(context.Background(), rsv.ID, decimal.NewFromInt(1)); err != nil {
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
