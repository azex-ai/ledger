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

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
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

	q := sqlcgen.New(pool)
	classStore := postgres.NewClassificationStore(pool)
	tmplStore := postgres.NewTemplateStore(pool)
	bookingStore := postgres.NewBookingStore(pool, q)

	// ClassificationStore satisfies both ClassificationStore and JournalTypeStore.
	if err := presets.InstallDefaultTemplatePresets(ctx, classStore, classStore, tmplStore); err != nil {
		return fmt.Errorf("install presets: %w", err)
	}

	// 1. Book the deposit (status = pending, channel = evm).
	booking, err := bookingStore.CreateBooking(ctx, core.CreateBookingInput{
		ClassificationCode: "deposit",
		AccountHolder:      1001,
		CurrencyID:         1,
		Amount:             decimal.RequireFromString("500.00"),
		IdempotencyKey:     fmt.Sprintf("deposit:1001:%d", time.Now().UnixNano()),
		ChannelName:        "evm",
		Metadata:           map[string]any{"chain": "ethereum"},
	})
	if err != nil {
		return fmt.Errorf("create booking: %w", err)
	}
	fmt.Printf("created booking id=%d status=%s\n", booking.ID, booking.Status)

	// 2. Mempool sighting -> confirming.
	if _, err := bookingStore.Transition(ctx, core.TransitionInput{
		BookingID:  booking.ID,
		ToStatus:   "confirming",
		ChannelRef: "0xabc123",
	}); err != nil {
		return fmt.Errorf("transition confirming: %w", err)
	}

	// 3. Enough confirmations -> confirmed (this is where journals post in real flow).
	evt, err := bookingStore.Transition(ctx, core.TransitionInput{
		BookingID:  booking.ID,
		ToStatus:   "confirmed",
		ChannelRef: "0xabc123",
		Amount:     decimal.RequireFromString("500.00"),
	})
	if err != nil {
		return fmt.Errorf("transition confirmed: %w", err)
	}
	fmt.Printf("confirmed event id=%d journal_id=%v\n", evt.ID, evt.JournalID)
	return nil
}
