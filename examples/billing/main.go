// Example: SaaS-style metered billing using Reserve → metered deduction → Release.
//
// Scenario: a user tops-up their wallet, reserves a budget for an AI compute
// run, deducts the actual cost when the run completes, and releases the unused
// portion.
//
// Demonstrates:
//   - ledger.New(pool) + svc.InstallExtendedPresets(ctx)
//   - svc.Reserver().Reserve  — budget hold (TOCTOU-safe advisory lock)
//   - svc.Reserver().Settle   — actual cost capture + automatic remainder release
//   - svc.BalanceReader().GetBalance  — balance query
//   - ledger.NewIdempotencyKey  — collision-free idempotency keys
//
// Run:
//
//	export DATABASE_URL="postgres://user:pass@localhost:5432/ledger_dev?sslmode=disable"
//	go run ./examples/billing
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres"
)

const (
	userID     int64 = 1001
	currencyID int64 = 1 // assume USDT with id=1
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

	// Wire up the ledger facade.
	svc, err := ledger.New(pool)
	if err != nil {
		return fmt.Errorf("ledger.New: %w", err)
	}

	// Install the full preset suite (deposit, withdrawal, fee, transfer, …).
	// Idempotent — safe to call on every startup.
	if err := svc.InstallExtendedPresets(ctx); err != nil {
		return fmt.Errorf("install presets: %w", err)
	}

	// -----------------------------------------------------------------------
	// Step 1: top-up the user's main_wallet via a deposit booking.
	// In production this would go through the full EVM deposit lifecycle;
	// here we post a journal directly to seed the balance.
	// -----------------------------------------------------------------------
	topupKey := ledger.NewIdempotencyKey("topup")
	jw := svc.JournalWriter()

	_, err = jw.ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
		HolderID:       userID,
		CurrencyID:     currencyID,
		IdempotencyKey: topupKey,
		Amounts:        map[string]decimal.Decimal{"amount": decimal.RequireFromString("100.00")},
		Source:         "billing-example",
	})
	if err != nil {
		return fmt.Errorf("top-up: %w", err)
	}
	fmt.Println("topped up: 100.00 USDT")

	// -----------------------------------------------------------------------
	// Step 2: reserve a budget for the compute run (e.g. up to $20.00).
	// Reserve acquires a per-(holder, currency) advisory lock and checks
	// available = totalBalance − SUM(active reservations) before locking funds.
	// -----------------------------------------------------------------------
	reserveKey := ledger.NewIdempotencyKey("reserve")
	rsv, err := svc.Reserver().Reserve(ctx, core.ReserveInput{
		AccountHolder:  userID,
		CurrencyID:     currencyID,
		Amount:         decimal.RequireFromString("20.00"),
		IdempotencyKey: reserveKey,
		ExpiresIn:      time.Hour, // 1-hour budget window
	})
	if err != nil {
		return fmt.Errorf("reserve: %w", err)
	}
	fmt.Printf("reserved: id=%d amount=%s status=%s\n", rsv.ID, rsv.ReservedAmount, rsv.Status)

	// -----------------------------------------------------------------------
	// Step 3: compute run finishes — actual cost was $15.75.
	// Settle debits the actual amount and automatically releases the $4.25
	// remainder. Both operations happen atomically inside the adapter.
	// -----------------------------------------------------------------------
	actualCost := decimal.RequireFromString("15.75")
	if err := svc.Reserver().Settle(ctx, rsv.ID, actualCost); err != nil {
		return fmt.Errorf("settle: %w", err)
	}
	fmt.Printf("settled: actual_cost=%s (remainder released automatically)\n", actualCost)

	// -----------------------------------------------------------------------
	// Step 4: read back the user's main_wallet balance.
	// Note: classificationID for main_wallet is looked up from the preset.
	// -----------------------------------------------------------------------
	cls, err := svc.Classifications().GetByCode(ctx, "main_wallet")
	if err != nil {
		return fmt.Errorf("get classification: %w", err)
	}

	balance, err := svc.BalanceReader().GetBalance(ctx, userID, currencyID, cls.ID)
	if err != nil {
		return fmt.Errorf("get balance: %w", err)
	}

	// Expected: 100.00 - 15.75 = 84.25 USDT
	fmt.Printf("final balance: %s USDT (expected 84.25)\n", balance)
	return nil
}
