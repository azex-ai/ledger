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
//     on the same pgx.Tx via tx.DBTX() — both writes commit or roll back
//     together
//   - Rollback on error: the journal is never committed when the side-effect fails
//
// NOTE: To stay self-contained, this example creates a tiny `demo_orders`
// table on startup and uses it as the caller-side write. Adapt to your own
// schema before integrating into a real application.
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

	currencyUID, err := ensureCurrency(ctx, svc, "USDT", "Tether USD")
	if err != nil {
		return err
	}

	// Demo-only table that the in-tx side-effect will write to. Created via
	// the facade's DBTX() so the example needs no migration files of its own.
	if _, err := svc.DBTX().Exec(ctx, `
		CREATE TABLE IF NOT EXISTS demo_orders (
			id          BIGSERIAL PRIMARY KEY,
			holder_id   BIGINT NOT NULL,
			currency_id BIGINT NOT NULL,
			amount      NUMERIC(30,18) NOT NULL,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create demo_orders: %w", err)
	}

	// Seed a balance so the lock_funds template has something to work with.
	_, err = svc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
		HolderID:       3001,
		CurrencyUID:    currencyUID,
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
			CurrencyUID:    currencyUID,
			IdempotencyKey: ikey,
			Amounts:        map[string]decimal.Decimal{"amount": decimal.RequireFromString("100.00")},
			Source:         "tx-compose-example",
		})
		if err != nil {
			return fmt.Errorf("lock_funds template: %w", err)
		}

		// 2. Insert an order row on the SAME transaction.
		// tx.DBTX() returns the active pgx.Tx inside a RunInTx callback, so
		// this Exec lands on the same connection as the journal write above —
		// the two writes commit or roll back together.
		//
		// (tx.Pool() exists too, but it returns the underlying pool and would
		// commit independently of the surrounding transaction. Use DBTX inside
		// RunInTx; reserve Pool for code that runs outside the callback.)
		if _, err := tx.DBTX().Exec(ctx,
			`INSERT INTO demo_orders (holder_id, currency_id, amount) VALUES ($1, $2, $3)`,
			3001, 1, "100.00",
		); err != nil {
			return fmt.Errorf("insert demo_orders: %w", err)
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
			CurrencyUID:    currencyUID,
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

func ensureCurrency(ctx context.Context, svc *ledger.Service, code, name string) (string, error) {
	list, err := svc.Currencies().ListCurrencies(ctx, false)
	if err != nil {
		return "", fmt.Errorf("list currencies: %w", err)
	}
	for _, c := range list {
		if c.Code == code {
			return c.UID, nil
		}
	}
	created, err := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{Code: code, Name: name, Exponent: 18})
	if err != nil {
		return "", fmt.Errorf("create currency: %w", err)
	}
	return created.UID, nil
}
