// Example: in-process event subscription using Worker.Subscribe.
//
// Every booking state transition emits a core.Event. In library mode you
// can receive those events synchronously in the same process instead of
// setting up an outbound webhook server.
//
// Demonstrates:
//   - ledger.New(pool) + svc.Worker(cfg)
//   - worker.Subscribe(func(ctx, evt) error { ... })
//   - Triggering a booking transition and observing the handler fires
//   - Graceful shutdown with worker drain via context cancellation
//
// Run:
//
//	export DATABASE_URL="postgres://user:pass@localhost:5432/ledger_dev?sslmode=disable"
//	go run ./examples/event-subscribe
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
	"github.com/azex-ai/ledger/service"
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

	// Install the deposit preset so we can create a booking below.
	if err := svc.InstallDefaultPresets(ctx); err != nil {
		return fmt.Errorf("install presets: %w", err)
	}

	// -----------------------------------------------------------------------
	// Build the background worker with default intervals.
	// -----------------------------------------------------------------------
	cfg := service.DefaultWorkerConfig()
	cfg.EventDeliveryInterval = 100 * time.Millisecond // fast poll for demo
	worker := svc.Worker(cfg)

	// -----------------------------------------------------------------------
	// Subscribe to events. The handler receives every emitted core.Event.
	// If the handler returns an error the event is still marked delivered —
	// a buggy handler should not block the queue.
	// -----------------------------------------------------------------------
	received := make(chan core.Event, 10)
	worker.Subscribe(func(_ context.Context, evt core.Event) error {
		fmt.Printf("[event] id=%d class=%s %s -> %s actor=%d source=%q\n",
			evt.ID, evt.ClassificationCode, evt.FromStatus, evt.ToStatus,
			evt.ActorID, evt.Source)
		received <- evt
		return nil
	})

	// -----------------------------------------------------------------------
	// Run the worker in the background. Cancel ctx to trigger graceful drain.
	// -----------------------------------------------------------------------
	workerCtx, cancelWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()

	// -----------------------------------------------------------------------
	// Create a deposit booking and drive it to "confirming".
	// Each Transition call writes an Event; the LocalDispatcher picks it up
	// on the next poll tick.
	// -----------------------------------------------------------------------
	booker := svc.Booker()
	booking, err := booker.CreateBooking(ctx, core.CreateBookingInput{
		ClassificationCode: "deposit",
		AccountHolder:      2001,
		CurrencyID:         1,
		Amount:             decimal.RequireFromString("250.00"),
		IdempotencyKey:     ledger.NewIdempotencyKey("event-demo"),
		ChannelName:        "evm",
	})
	if err != nil {
		cancelWorker()
		<-workerDone
		return fmt.Errorf("create booking: %w", err)
	}
	fmt.Printf("created booking id=%d status=%s\n", booking.ID, booking.Status)

	if _, err := booker.Transition(ctx, core.TransitionInput{
		BookingID:  booking.ID,
		ToStatus:   "confirming",
		ChannelRef: "0xdemo",
		Source:     "event-subscribe-example",
	}); err != nil {
		cancelWorker()
		<-workerDone
		return fmt.Errorf("transition: %w", err)
	}
	fmt.Println("transitioned to confirming")

	// -----------------------------------------------------------------------
	// Wait for the event to arrive (up to 3 seconds), then shut down.
	// -----------------------------------------------------------------------
	select {
	case evt := <-received:
		fmt.Printf("handler received event id=%d to_status=%s\n", evt.ID, evt.ToStatus)
	case <-time.After(3 * time.Second):
		fmt.Println("timeout waiting for event — check EventDeliveryInterval config")
	}

	// Graceful shutdown: cancel ctx and wait for worker to drain.
	cancelWorker()
	if err := <-workerDone; err != nil {
		return fmt.Errorf("worker: %w", err)
	}
	fmt.Println("worker drained, exiting")
	return nil
}
