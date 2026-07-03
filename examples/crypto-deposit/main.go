// Example: end-to-end EVM deposit booking using ledger as a library.
//
// Run order:
//
//  1. Start Postgres and set DATABASE_URL.
//  2. go run ./examples/crypto-deposit
//
// Assumes a currency with id=1 and that the deposit/withdrawal preset
// classifications + templates have been installed (see presets pkg).
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
	"github.com/azex-ai/ledger/presets"
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

	// One-line ledger wiring via the top-level facade.
	svc, err := ledger.New(pool)
	if err != nil {
		return fmt.Errorf("ledger facade: %w", err)
	}

	currencyUID, err := ensureCurrency(ctx, svc, "USDT", "Tether USD")
	if err != nil {
		return err
	}

	// Presets need the concrete store handles. ClassificationStore satisfies
	// both ClassificationStore and JournalTypeStore.
	classStore := postgres.NewClassificationStore(pool)
	tmplStore := postgres.NewTemplateStore(pool)
	if err := presets.InstallTemplateBundle(ctx, classStore, classStore, tmplStore, presets.DepositBundle()); err != nil {
		return fmt.Errorf("install deposit bundle: %w", err)
	}

	booker := svc.Booker()

	// 1. Book the deposit (status = pending, channel = evm).
	booking, err := booker.CreateBooking(ctx, core.CreateBookingInput{
		ClassificationCode: "deposit",
		AccountHolder:      1001,
		CurrencyUID:        currencyUID,
		Amount:             decimal.RequireFromString("500.00"),
		IdempotencyKey:     fmt.Sprintf("deposit:1001:%d", time.Now().UnixNano()),
		ChannelName:        "evm",
		Metadata:           map[string]string{"chain": "ethereum"},
	})
	if err != nil {
		return fmt.Errorf("create booking: %w", err)
	}
	fmt.Printf("created booking uid=%s status=%s\n", booking.UID, booking.Status)

	// 2. Mempool sighting -> confirming.
	if _, err := booker.Transition(ctx, core.TransitionInput{
		BookingUID: booking.UID,
		ToStatus:   "confirming",
		ChannelRef: "0xabc123",
	}); err != nil {
		return fmt.Errorf("transition confirming: %w", err)
	}

	// 3. Enough confirmations -> confirmed. Use RunInTx so the transition event
	// and the accounting journal commit atomically and cross-link via EventID.
	var confirmedEvent *core.Event
	var confirmedJournal *core.Journal
	err = svc.RunInTx(ctx, func(txSvc *ledger.Service) error {
		evt, err := txSvc.Booker().Transition(ctx, core.TransitionInput{
			BookingUID: booking.UID,
			ToStatus:   "confirmed",
			ChannelRef: "0xabc123",
			Amount:     decimal.RequireFromString("500.00"),
			Source:     "example.crypto_deposit",
		})
		if err != nil {
			return err
		}

		journal, err := txSvc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
			HolderID:       booking.AccountHolder,
			CurrencyUID:    booking.CurrencyUID,
			IdempotencyKey: fmt.Sprintf("deposit-confirm-journal:%s", booking.UID),
			EventUID:       evt.UID,
			Amounts:        map[string]decimal.Decimal{"amount": booking.Amount},
			Source:         "example.crypto_deposit",
		})
		if err != nil {
			return err
		}

		confirmedEvent = evt
		confirmedJournal = journal
		return nil
	})
	if err != nil {
		return fmt.Errorf("transition confirmed + journal: %w", err)
	}
	fmt.Printf("confirmed event uid=%s journal_uid=%s journal=%s\n", confirmedEvent.UID, confirmedEvent.JournalUID, confirmedJournal.UID)
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
