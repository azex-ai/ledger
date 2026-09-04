package core_test

// W2-T3 (bus board #17, only-read measurement task,
// docs/plans/2026-08-21-integrity-hardening-contracts.md W2-2): the pure-CPU
// cost of core.VerifyJournalAuth itself -- canonical digest recompute +
// ed25519 signature verify, no DB involved at all. This is the per-journal
// unit cost that postgres/verify_balance_bench_test.go's naive
// verify-balance path multiplies by N (the account's entry count).
//
// Run:
//
//	go test ./core/ -bench=VerifyJournalAuth -benchmem -run=^$ -benchtime=3s -count=5
//
// No Docker/Postgres needed for this file.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/authdev"
	"github.com/azex-ai/ledger/core"
)

func newVerifyBenchKey(b *testing.B) (*authdev.LocalAttestor, *authdev.LocalVerifier) {
	b.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		b.Fatal(err)
	}
	attestor, verifier, err := authdev.NewLocalAttestor(priv.Seed(), "verify-bench-key")
	if err != nil {
		b.Fatal(err)
	}
	return attestor, verifier
}

// verifyBenchInput builds a balanced JournalInput with numEntries entries
// (numEntries/2 debit/credit pairs across distinct synthetic accounts, all
// in one currency/classification pair) -- shape only, no DB round trip.
func verifyBenchInput(numEntries int) core.JournalInput {
	entries := make([]core.EntryInput, 0, numEntries)
	for i := 0; i < numEntries; i += 2 {
		amt := decimal.NewFromInt(int64(i + 1))
		entries = append(entries,
			core.EntryInput{AccountHolder: int64(1000 + i), CurrencyUID: "bench-currency-uid", ClassificationUID: "bench-wallet-uid", EntryType: core.EntryTypeDebit, Amount: amt},
			core.EntryInput{AccountHolder: int64(-(1000 + i)), CurrencyUID: "bench-currency-uid", ClassificationUID: "bench-custodial-uid", EntryType: core.EntryTypeCredit, Amount: amt},
		)
	}
	return core.JournalInput{
		JournalTypeUID: "bench-journal-type-uid",
		IdempotencyKey: "bench-verify-idem-key",
		ActorID:        42,
		Source:         "verify-bench",
		Entries:        entries,
	}
}

func runVerifyJournalAuthBench(b *testing.B, numEntries int) {
	attestor, verifier := newVerifyBenchKey(b)
	input := verifyBenchInput(numEntries)
	effectiveAt := time.Now().UTC()

	digest, err := core.CanonicalJournalDigest(input, effectiveAt)
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	signature, keyID, err := attestor.Sign(ctx, digest)
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		if err := core.VerifyJournalAuth(ctx, verifier, input, effectiveAt, digest, signature, keyID); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkVerifyJournalAuth_2Entries is the common case: a plain
// debit/credit posting (deposit/withdrawal/transfer shape).
func BenchmarkVerifyJournalAuth_2Entries(b *testing.B) { runVerifyJournalAuthBench(b, 2) }

// BenchmarkVerifyJournalAuth_10Entries stands in for a multi-entry journal
// (e.g. a fee split or multi-party settlement template).
func BenchmarkVerifyJournalAuth_10Entries(b *testing.B) { runVerifyJournalAuthBench(b, 10) }

// BenchmarkVerifyJournalAuth_50Entries stands in for a large batch/template
// journal -- upper end of what the design docs discuss for
// ExecuteTemplateBatch-shaped postings.
func BenchmarkVerifyJournalAuth_50Entries(b *testing.B) { runVerifyJournalAuthBench(b, 50) }
