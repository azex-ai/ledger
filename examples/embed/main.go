// Example: minimum-viable embed.
//
// The shortest path from `import "github.com/azex-ai/ledger"` to a posted
// journal and a queried balance — with no template, no booking, no preset
// installation. Useful as the "hello world" to compare your integration
// against, and as a documentation aid for the dual-mode story (library +
// HTTP service) advertised in CLAUDE.md.
//
// Demonstrates:
//   - ledger.New(pool)            — single facade construction
//   - svc.JournalWriter().PostJournal — bypass templates entirely
//   - svc.BalanceReader().GetBalance  — checkpoint+delta read
//
// What it does NOT cover (see other examples):
//   - Reserve/Settle (see examples/billing)
//   - Booking lifecycle (see examples/crypto-deposit)
//   - Event delivery / webhooks (see examples/event-subscribe)
//   - Transaction composition (see examples/tx-compose)
//
// Run:
//
//	export DATABASE_URL="postgres://user:pass@localhost:5432/ledger_dev?sslmode=disable"
//	go run ./examples/embed
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres"
)

const (
	userID     int64 = 9001
	currencyID int64 = 1 // assume USDT id=1 already exists; if not, see svc.Currencies().CreateCurrency
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	if err := postgres.Migrate(dbURL); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("pgxpool: %w", err)
	}
	defer pool.Close()

	svc, err := ledger.New(pool)
	if err != nil {
		return fmt.Errorf("ledger.New: %w", err)
	}

	// Make sure a journal type and the two classifications we'll cite exist.
	// In production you'd install a preset bundle; here we wire the bare
	// minimum by hand to keep the example self-contained.
	jt, err := ensureJournalType(ctx, svc, "manual_credit", "Manual Credit")
	if err != nil {
		return err
	}
	main, err := ensureClassification(ctx, svc, "main_wallet", "Main Wallet", core.NormalSideDebit, false)
	if err != nil {
		return err
	}
	custody, err := ensureClassification(ctx, svc, "custodial", "Custodial", core.NormalSideCredit, true)
	if err != nil {
		return err
	}

	// -----------------------------------------------------------------------
	// Post a journal directly. This is the lowest-level write path the
	// library exposes — no templates, no bookings, just a balanced set of
	// entries with an idempotency key.
	//
	//	DR main_wallet (user)   $50.00
	//	CR custodial (system)   $50.00
	// -----------------------------------------------------------------------
	amount := decimal.RequireFromString("50.00")
	input := core.JournalInput{
		JournalTypeID:  jt.ID,
		IdempotencyKey: ledger.NewIdempotencyKey("embed-demo"),
		Source:         "embed-example",
		Entries: []core.EntryInput{
			{
				AccountHolder:    userID,
				CurrencyID:       currencyID,
				ClassificationID: main.ID,
				EntryType:        core.EntryTypeDebit,
				Amount:           amount,
			},
			{
				AccountHolder:    core.SystemAccountHolder(userID),
				CurrencyID:       currencyID,
				ClassificationID: custody.ID,
				EntryType:        core.EntryTypeCredit,
				Amount:           amount,
			},
		},
	}

	journal, err := svc.JournalWriter().PostJournal(ctx, input)
	if err != nil {
		return fmt.Errorf("post journal: %w", err)
	}
	fmt.Printf("posted journal id=%d (debit=%s credit=%s)\n", journal.ID, journal.TotalDebit, journal.TotalCredit)

	// -----------------------------------------------------------------------
	// Read the balance back. Uses the checkpoint+delta path internally so the
	// new journal is reflected immediately, even though the rollup worker
	// hasn't advanced the checkpoint yet.
	// -----------------------------------------------------------------------
	balance, err := svc.BalanceReader().GetBalance(ctx, userID, currencyID, main.ID)
	if err != nil {
		return fmt.Errorf("get balance: %w", err)
	}
	fmt.Printf("user %d main_wallet (currency %d): %s\n", userID, currencyID, balance)

	return nil
}

func ensureJournalType(ctx context.Context, svc *ledger.Service, code, name string) (*core.JournalType, error) {
	jt, err := svc.JournalTypes().GetJournalTypeByCode(ctx, code)
	if err == nil {
		return jt, nil
	}
	return svc.JournalTypes().CreateJournalType(ctx, core.JournalTypeInput{Code: code, Name: name})
}

func ensureClassification(ctx context.Context, svc *ledger.Service, code, name string, side core.NormalSide, system bool) (*core.Classification, error) {
	c, err := svc.Classifications().GetByCode(ctx, code)
	if err == nil {
		return c, nil
	}
	return svc.Classifications().CreateClassification(ctx, core.ClassificationInput{
		Code:       code,
		Name:       name,
		NormalSide: side,
		IsSystem:   system,
	})
}
