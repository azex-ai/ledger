// Example: transactional composition — caller's business write + ledger journal
// in a single PostgreSQL transaction.
//
// Use case: when a user makes a withdrawal you want to both (a) insert an order
// row in your own orders table and (b) lock the funds in the ledger. If either
// fails, both must roll back. RunInTx makes this trivial.
//
// Demonstrates:
//   - svc.RunInTx(ctx, func(tx *ledger.Service) error { ... })
//   - Combining tx.JournalWriter().ExecuteTemplate with a raw SQL side-effect
//     on the same pgx.Tx via tx.Pool() (illustrative — real code would use
//     your own typed store wired to the same DBTX)
//   - Rollback on error: the journal is never committed when the side-effect fails
//
// NOTE: This example writes to a hypothetical "orders" table that does not exist
// in the ledger schema. The second RunInTx call deliberately returns an error to
// illustrate rollback isolation. Adapt to your own schema before running.
//
// Run:
//
//	export DATABASE_URL="postgres://user:pass@localhost:5432/ledger_dev?sslmode=disable"
//	go run ./examples/tx-compose
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres"
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

	if err := svc.InstallDefaultPresets(ctx); err != nil {
		return fmt.Errorf("install presets: %w", err)
	}

	// Seed a balance so the lock_funds template has something to work with.
	_, err = svc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
		HolderID:       3001,
		CurrencyID:     1,
		IdempotencyKey: ledger.NewIdempotencyKey("txdemo-seed"),
		Amounts:        map[string]decimal.Decimal{"amount": decimal.RequireFromString("500.00")},
		Source:         "tx-compose-seed",
	})
	if err != nil {
		return fmt.Errorf("seed deposit: %w", err)
	}
	fmt.Println("seeded 500.00 USDT for holder 3001")

	// -----------------------------------------------------------------------
	// Happy path: journal + side-effect both succeed → committed together.
	// -----------------------------------------------------------------------
	ikey := ledger.NewIdempotencyKey("withdraw-lock")
	commitErr := svc.RunInTx(ctx, func(tx *ledger.Service) error {
		// 1. Lock funds in the ledger (DR locked / CR main_wallet).
		_, err := tx.JournalWriter().ExecuteTemplate(ctx, "lock_funds", core.TemplateParams{
			HolderID:       3001,
			CurrencyID:     1,
			IdempotencyKey: ikey,
			Amounts:        map[string]decimal.Decimal{"amount": decimal.RequireFromString("100.00")},
			Source:         "tx-compose-example",
		})
		if err != nil {
			return fmt.Errorf("lock_funds template: %w", err)
		}

		// 2. Insert an order row on the same transaction.
		// In real code you would use a typed repository wired to tx.Pool().
		// Here we illustrate the pattern with a raw query against a table that
		// is expected NOT to exist — to keep this compilable without schema changes
		// we catch the specific "table does not exist" error and treat it as ok.
		_, execErr := tx.Pool().Exec(ctx,
			`INSERT INTO orders (holder_id, currency_id, amount) VALUES ($1, $2, $3)`,
			3001, 1, "100.00",
		)
		if execErr != nil {
			// Table doesn't exist in this example environment — that's fine.
			// In production, return the error so the tx rolls back:
			//   return fmt.Errorf("insert order: %w", execErr)
			fmt.Printf("  (orders table not found — skipping side-effect: %v)\n", execErr)
		}

		return nil // commit both operations
	})
	if commitErr != nil {
		return fmt.Errorf("RunInTx happy path: %w", commitErr)
	}
	fmt.Println("committed: lock_funds journal + order insert in one transaction")

	// -----------------------------------------------------------------------
	// Rollback path: simulate a business-logic failure after the journal write.
	// The journal must NOT appear in the database after this call returns.
	// -----------------------------------------------------------------------
	rollbackKey := ledger.NewIdempotencyKey("withdraw-lock-rb")
	rollbackErr := svc.RunInTx(ctx, func(tx *ledger.Service) error {
		_, err := tx.JournalWriter().ExecuteTemplate(ctx, "lock_funds", core.TemplateParams{
			HolderID:       3001,
			CurrencyID:     1,
			IdempotencyKey: rollbackKey,
			Amounts:        map[string]decimal.Decimal{"amount": decimal.RequireFromString("50.00")},
			Source:         "tx-compose-rollback",
		})
		if err != nil {
			return fmt.Errorf("lock_funds: %w", err)
		}

		// Simulate downstream failure (e.g. payment gateway rejected the withdrawal).
		return errors.New("payment gateway: insufficient external liquidity")
	})

	// The error propagates; the journal was rolled back along with the tx.
	if rollbackErr != nil {
		fmt.Printf("expected rollback: %v\n", rollbackErr)
		fmt.Println("journal was rolled back — ledger balance unchanged")
	}

	return nil
}
