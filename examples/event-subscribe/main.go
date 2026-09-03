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
//   - At-least-once delivery: a handler that errors on its first attempt IS
//     invoked again for the SAME event, so handlers must dedupe by event UID
//   - Graceful shutdown with worker drain via context cancellation
//
// Run:
//
//	export DATABASE_URL="postgres://user:pass@localhost:5432/ledger_dev?sslmode=disable"
//	# optional: migrations on their own credential (see docs/RUNBOOK.md "Database roles")
//	export MIGRATE_DATABASE_URL="postgres://ledger_owner:pass@localhost:5432/ledger_dev?sslmode=disable"
//	go run ./examples/event-subscribe
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/slogadapter"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/presets"
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

	// WithLogger is not optional for anything that runs a Worker: without it
	// the Service installs core.NopLogger, every worker signal goes nowhere,
	// and Worker.Run refuses to start rather than run invisibly.
	svc, err := ledger.New(pool, ledger.WithLogger(slogadapter.New(slog.Default())))
	if err != nil {
		return fmt.Errorf("ledger.New: %w", err)
	}

	// Install the deposit preset so we can create a booking below.
	if err := svc.InstallDefaultPresets(ctx); err != nil {
		return fmt.Errorf("install presets: %w", err)
	}

	// The presets install accounting templates, not lifecycles -- and the
	// schema ships a label-only "deposit" classification, so the preset finds
	// one already there and leaves it alone. A booking needs a lifecycle, so
	// attach one. SetLifecycleIfEmpty seeds it only when there is none, and
	// never clobbers a lifecycle an operator has customised.
	depositClass, err := svc.Classifications().GetByCode(ctx, "deposit")
	if err != nil {
		return fmt.Errorf("get deposit classification: %w", err)
	}
	if err := svc.Classifications().SetLifecycleIfEmpty(ctx, depositClass.UID, presets.DepositLifecycle); err != nil {
		return fmt.Errorf("install deposit lifecycle: %w", err)
	}

	currencyUID, err := ensureCurrency(ctx, svc, "USDT", "Tether USD")
	if err != nil {
		return err
	}

	// -----------------------------------------------------------------------
	// Build the background worker with default intervals.
	// -----------------------------------------------------------------------
	cfg := service.DefaultWorkerConfig()
	cfg.EventDeliveryInterval = 100 * time.Millisecond // fast poll for demo
	worker, err := svc.Worker(cfg)
	if err != nil {
		return fmt.Errorf("svc.Worker: %w", err)
	}

	// -----------------------------------------------------------------------
	// Subscribe to events. The handler receives every emitted core.Event.
	//
	// Delivery is AT-LEAST-ONCE, the same guarantee webhook delivery makes
	// (service/worker.go's Subscribe godoc, delivery.WebhookDeliverer): if
	// the handler returns an error, the event is scheduled for retry rather
	// than marked delivered — so a handler that errors after doing partial
	// work WILL be invoked again for the SAME event. Handlers must therefore
	// be idempotent per event UID; do not write one that assumes "returned
	// an error" means "had no effect". handle (below) guards on evt.UID for
	// exactly this reason.
	// -----------------------------------------------------------------------
	received := make(chan core.Event, 10)
	var mu sync.Mutex
	processed := make(map[string]bool) // idempotency guard: event UID -> already applied
	handle := func(_ context.Context, evt core.Event) error {
		mu.Lock()
		defer mu.Unlock()
		if processed[evt.UID] {
			fmt.Printf("[event] id=%s redelivered -- ignored (already processed; handlers must dedupe by UID)\n", evt.UID)
			return nil
		}
		processed[evt.UID] = true
		fmt.Printf("[event] id=%s class=%s %s -> %s actor=%d source=%q\n",
			evt.UID, evt.ClassificationCode, evt.FromStatus, evt.ToStatus,
			evt.ActorID, evt.Source)
		received <- evt
		return nil
	}
	if err := worker.Subscribe(handle); err != nil {
		return fmt.Errorf("worker.Subscribe: %w", err)
	}

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
		CurrencyUID:        currencyUID,
		Amount:             decimal.RequireFromString("250.00"),
		IdempotencyKey:     ledger.NewIdempotencyKey("event-demo"),
		ChannelName:        "evm",
	})
	if err != nil {
		cancelWorker()
		<-workerDone
		return fmt.Errorf("create booking: %w", err)
	}
	fmt.Printf("created booking uid=%s status=%s\n", booking.UID, booking.Status)

	if _, err := booker.Transition(ctx, core.TransitionInput{
		BookingUID:     booking.UID,
		ToStatus:       "confirming",
		ChannelRef:     "0xdemo",
		Source:         "event-subscribe-example",
		IdempotencyKey: ledger.NewIdempotencyKey("event-demo-confirming"),
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
		fmt.Printf("handler received event uid=%s to_status=%s\n", evt.UID, evt.ToStatus)

		// Prove the at-least-once contract instead of just asserting it: a
		// crash after partial handler work, a claim-lease expiry, or a prior
		// transient error can all cause the SAME event to reach this handler
		// again (the real path retries with exponential backoff starting at
		// 1 minute -- service/delivery/webhook.go's retryIntervals -- too
		// long to wait out here). Call the same handler directly with the
		// same event to simulate that redelivery and show the dedupe-by-UID
		// guard in `handle` is what makes it safe.
		if err := handle(ctx, evt); err != nil {
			cancelWorker()
			<-workerDone
			return fmt.Errorf("simulated redelivery: %w", err)
		}
	case <-time.After(3 * time.Second):
		// Failing here rather than printing and carrying on: the whole point
		// of this example is that the handler receives the event, so an exit
		// code of 0 with no delivery would report success for a run in which
		// nothing happened.
		cancelWorker()
		<-workerDone
		return fmt.Errorf("no event reached the handler within 3s (30 poll cycles at the configured 100ms interval) -- the subscription is not working")
	}

	// Graceful shutdown: cancel ctx and wait for worker to drain.
	cancelWorker()
	if err := <-workerDone; err != nil {
		return fmt.Errorf("worker: %w", err)
	}
	fmt.Println("worker drained, exiting")
	return nil
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
