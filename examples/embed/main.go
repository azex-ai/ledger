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
//	# optional: migrations on their own credential (see docs/RUNBOOK.md "Database roles")
//	export MIGRATE_DATABASE_URL="postgres://ledger_owner:pass@localhost:5432/ledger_dev?sslmode=disable"
//	go run ./examples/embed
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

const userID int64 = 9001

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
	// Migrations run on their own credential. Migrate switches its own
	// connection to ledger_owner rather than granting the credential
	// ledger_owner's privileges, so this pool inherits nothing while a
	// migration run is in flight -- but a credential that can reach
	// ledger_owner at all is still not one to serve traffic on: any session
	// holding it can SET ROLE to that role deliberately. See docs/RUNBOOK.md
	// "Database roles".
	migrateURL := os.Getenv("MIGRATE_DATABASE_URL")
	if migrateURL == "" {
		migrateURL = dbURL
		log.Printf("warning: MIGRATE_DATABASE_URL is unset, so migrations run on DATABASE_URL. " +
			"That credential can act as ledger_owner -- able to drop the append-only guards -- which is not something " +
			"a serving pool should be able to do; Migrate refuses outright if any other session is already connected " +
			"as it. Acceptable for a local example, which migrates before it opens a pool. Not for production.")
	}
	if err := postgres.Migrate(migrateURL); err != nil {
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

	currencyUID, err := ensureCurrency(ctx, svc, "USDT", "Tether USD")
	if err != nil {
		return err
	}

	// Make sure a journal type and the two classifications we'll cite exist.
	// In production you'd install a preset bundle; here we wire the bare
	// minimum by hand to keep the example self-contained.
	jt, err := ensureJournalType(ctx, svc, "manual_credit", "Manual Credit")
	if err != nil {
		return err
	}
	main, err := ensureClassification(ctx, svc, "main_wallet", "Main Wallet", core.NormalSideDebit, false, core.BalanceRoleAvailable)
	if err != nil {
		return err
	}
	custody, err := ensureClassification(ctx, svc, "custodial", "Custodial", core.NormalSideCredit, true, core.BalanceRoleNone)
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
		JournalTypeUID: jt.UID,
		IdempotencyKey: ledger.NewIdempotencyKey("embed-demo"),
		Source:         "embed-example",
		Entries: []core.EntryInput{
			{
				AccountHolder:     userID,
				CurrencyUID:       currencyUID,
				ClassificationUID: main.UID,
				EntryType:         core.EntryTypeDebit,
				Amount:            amount,
			},
			{
				AccountHolder:     core.SystemAccountHolder(userID),
				CurrencyUID:       currencyUID,
				ClassificationUID: custody.UID,
				EntryType:         core.EntryTypeCredit,
				Amount:            amount,
			},
		},
	}

	journal, err := svc.JournalWriter().PostJournal(ctx, input)
	if err != nil {
		return fmt.Errorf("post journal: %w", err)
	}
	fmt.Printf("posted journal uid=%s (debit=%s credit=%s)\n", journal.UID, journal.TotalDebit, journal.TotalCredit)

	// -----------------------------------------------------------------------
	// Read the balance back. Uses the checkpoint+delta path internally so the
	// new journal is reflected immediately, even though the rollup worker
	// hasn't advanced the checkpoint yet.
	// -----------------------------------------------------------------------
	balance, err := svc.BalanceReader().GetBalance(ctx, userID, currencyUID, main.UID)
	if err != nil {
		return fmt.Errorf("get balance: %w", err)
	}
	fmt.Printf("user %d main_wallet (currency %s): %s\n", userID, currencyUID, balance)

	return nil
}

func ensureJournalType(ctx context.Context, svc *ledger.Service, code, name string) (*core.JournalType, error) {
	jt, err := svc.JournalTypes().GetJournalTypeByCode(ctx, code)
	if err == nil {
		return jt, nil
	}
	if !errors.Is(err, core.ErrNotFound) {
		// A transient error (connection drop, timeout) here is not "the
		// journal type doesn't exist" -- falling through to Create on any
		// error, instead of specifically ErrNotFound, would race a second
		// create against the one that actually exists.
		return nil, fmt.Errorf("get journal type %s: %w", code, err)
	}
	return svc.JournalTypes().CreateJournalType(ctx, core.JournalTypeInput{Code: code, Name: name})
}

func ensureCurrency(ctx context.Context, svc *ledger.Service, code, name string) (string, error) {
	list, err := svc.Currencies().ListCurrencies(ctx, false)
	if err != nil {
		return "", fmt.Errorf("list currencies: %w", err)
	}
	const exponent = int32(6)
	for _, c := range list {
		if c.Code != code {
			continue
		}
		if c.Exponent != exponent {
			return "", fmt.Errorf("currency %s already exists with exponent %d, this example expects %d", code, c.Exponent, exponent)
		}
		return c.UID, nil
	}
	created, err := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{Code: code, Name: name, Exponent: exponent})
	if err != nil {
		return "", fmt.Errorf("create currency: %w", err)
	}
	return created.UID, nil
}

func ensureClassification(ctx context.Context, svc *ledger.Service, code, name string, side core.NormalSide, system bool, role core.BalanceRole) (*core.Classification, error) {
	c, err := svc.Classifications().GetByCode(ctx, code)
	if err == nil {
		return c, nil
	}
	if !errors.Is(err, core.ErrNotFound) {
		return nil, fmt.Errorf("get classification %s: %w", code, err)
	}
	return svc.Classifications().CreateClassification(ctx, core.ClassificationInput{
		Code:        code,
		Name:        name,
		NormalSide:  side,
		IsSystem:    system,
		BalanceRole: role,
	})
}
