// Example: SaaS-style metered billing using Reserve → metered deduction → Release.
//
// Scenario: a user tops-up their wallet, reserves a budget for an AI compute
// run, deducts the actual cost when the run completes, and releases the unused
// portion.
//
// Demonstrates:
//   - ledger.New(pool) + svc.InstallExtendedPresets(ctx)
//   - svc.Reserver().Reserve  — budget hold (TOCTOU-safe advisory lock)
//   - svc.Reserver().Settle   — closes the hold at the actual amount
//   - svc.RunInTx             — settling and charging in one transaction
//   - svc.BalanceReader().GetBalance  — balance query
//   - ledger.NewIdempotencyKey  — collision-free idempotency keys
//
// One thing this example is careful about, because an earlier version of it
// was not: Settle does not move money. It closes the hold, which releases the
// reserved amount back into the holder's available balance -- the charge is a
// journal the caller posts. Settling without posting it releases the hold and
// bills nobody, and the ledger reports success, because from its side nothing
// went wrong: it did what it was asked. The earlier version printed a final
// balance of 100 under a line that said it expected 84.25.
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

const userID int64 = 1001

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

	currencyUID, err := ensureCurrency(ctx, svc, "USDT", "Tether USD")
	if err != nil {
		return err
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
		CurrencyUID:    currencyUID,
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
		CurrencyUID:    currencyUID,
		Amount:         decimal.RequireFromString("20.00"),
		IdempotencyKey: reserveKey,
		ExpiresIn:      time.Hour, // 1-hour budget window
	})
	if err != nil {
		return fmt.Errorf("reserve: %w", err)
	}
	fmt.Printf("reserved: uid=%s amount=%s status=%s\n", rsv.UID, rsv.ReservedAmount, rsv.Status)

	// -----------------------------------------------------------------------
	// Step 3: compute run finishes — actual cost was $15.75.
	//
	// Two things have to happen, and they have to happen together: the hold
	// closes at the actual amount, and the user is charged that amount.
	//
	// Settle only does the first. It is a state transition on the reservation
	// -- the ledger records that the hold ended and how much of it was used --
	// and the held amount stops counting against the holder's available
	// balance. No entries are written, so no money moves. That split is
	// deliberate: only the caller knows which accounts a charge belongs
	// between, and the ledger will not guess.
	//
	// RunInTx puts both in one transaction. Crash between them and neither
	// lands; without it, a crash after Settle frees the hold and bills nobody.
	// The *ledger.Service handed to the callback is a short-lived clone bound
	// to that transaction -- use it for everything inside, and do not keep it.
	// -----------------------------------------------------------------------
	actualCost := decimal.RequireFromString("15.75")
	chargeKey := ledger.NewIdempotencyKey("charge")
	if err := svc.RunInTx(ctx, func(tx *ledger.Service) error {
		if err := tx.Reserver().Settle(ctx, core.SettleInput{ReservationUID: rsv.UID, Amount: actualCost}); err != nil {
			return fmt.Errorf("settle: %w", err)
		}
		if _, err := tx.JournalWriter().ExecuteTemplate(ctx, "fee_charge", core.TemplateParams{
			HolderID:       userID,
			CurrencyUID:    currencyUID,
			IdempotencyKey: chargeKey,
			Amounts:        map[string]decimal.Decimal{"amount": actualCost},
			Source:         "billing-example",
		}); err != nil {
			return fmt.Errorf("charge: %w", err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("settle and charge: %w", err)
	}
	fmt.Printf("settled and charged: actual_cost=%s (the unused 4.25 goes back to available)\n", actualCost)

	// -----------------------------------------------------------------------
	// Step 4: read back the user's main_wallet balance.
	// Note: classificationID for main_wallet is looked up from the preset.
	// -----------------------------------------------------------------------
	cls, err := svc.Classifications().GetByCode(ctx, "main_wallet")
	if err != nil {
		return fmt.Errorf("get classification: %w", err)
	}

	balance, err := svc.BalanceReader().GetBalance(ctx, userID, currencyUID, cls.UID)
	if err != nil {
		return fmt.Errorf("get balance: %w", err)
	}

	// 100.00 topped up, 15.75 charged.
	expected := decimal.RequireFromString("84.25")
	fmt.Printf("final balance: %s USDT (expected %s)\n", balance, expected)
	if !balance.Equal(expected) {
		// An example that prints a number next to the number it expected, and
		// exits 0 when they differ, is documentation for a bug. This one fails.
		return fmt.Errorf("balance is %s, expected %s -- the charge did not land", balance, expected)
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
