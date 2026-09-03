// Example: full-stack quickstart — the backend half.
//
// This is a plain chi application (the shape any chi scaffold gives you) that
// imports the ledger as a library and mounts its complete HTTP API next to the
// host app's own routes. The frontend half in ../web is a Next.js app that
// imports @azex/ledger-react and renders the admin dashboard against this
// server.
//
// Demonstrates:
//   - ledger.New(pool)                — single facade construction
//   - svc.InstallDefaultPresets       — deposit/withdrawal bundles ready to use
//   - server.NewFromDeps(cfg, deps)   — the full ledger HTTP API as an http.Handler,
//     returning an error instead of panicking on an invalid config, and naming
//     each dependency by field instead of by position (see server.Deps)
//   - r.Handle("/api/v1/*", ...)      — mounting that handler inside a host chi router
//   - svc.Worker(...)                 — background rollup/expiry/snapshot loops, PLUS
//     the one job svc.Worker does not wire on its own: worker.SetEventDeliverer
//     (webhook delivery — it needs a subscriber store the library cannot pick
//     for you). Skipping it is silent — events sit in the events table forever
//     with no error, no log line, nothing — so a "complete assembly" example
//     wires it explicitly instead of teaching the gap.
//
// Run:
//
//	export DATABASE_URL="postgres://user:pass@localhost:5432/ledger_example?sslmode=disable"
//	# optional: migrations on their own credential (see docs/RUNBOOK.md "Database roles")
//	export MIGRATE_DATABASE_URL="postgres://ledger_owner:pass@localhost:5432/ledger_dev?sslmode=disable"
//	go run ./examples/fullstack/backend
//
// Then in ../web: npm install && npm run dev — the dashboard on :3090 talks to
// this server on :8090.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/slogadapter"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/server"
	"github.com/azex-ai/ledger/service"
	"github.com/azex-ai/ledger/service/delivery"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	port := os.Getenv("HTTP_PORT")
	if port == "" {
		port = "8090"
	}

	rootCtx, rootCancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer rootCancel()

	// ledger.Migrate accepts both postgres:// and postgresql:// directly and
	// translates to the pgx5:// scheme golang-migrate's driver registry wants
	// internally (postgres/migrate.go's toMigrateURL) -- no conversion needed
	// on the caller's side.
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
			"a serving pool should be able to do. Acceptable for a local example, not for production.")
	}
	if err := ledger.Migrate(migrateURL); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	pool, err := pgxpool.New(rootCtx, dbURL)
	if err != nil {
		return fmt.Errorf("pgxpool: %w", err)
	}
	defer pool.Close()

	// WithLogger is not optional for anything that runs a Worker: without it
	// the Service installs core.NopLogger, every worker signal (the startup
	// report naming which optional jobs are on, each job's per-tick failure)
	// goes nowhere, and Worker.Run refuses to start rather than run invisibly.
	svc, err := ledger.New(pool, ledger.WithLogger(slogadapter.New(slog.Default())))
	if err != nil {
		return fmt.Errorf("ledger.New: %w", err)
	}

	// Deposit + withdrawal classifications, journal types, and templates.
	// Idempotent — safe on every startup.
	if err := svc.InstallDefaultPresets(rootCtx); err != nil {
		return fmt.Errorf("install presets: %w", err)
	}

	// A few demo deposits so the dashboard has something to show. Deterministic
	// idempotency keys make this a no-op on restart.
	if err := seed(rootCtx, svc); err != nil {
		return fmt.Errorf("seed: %w", err)
	}

	// Background worker: balance rollups, reservation expiry, snapshots.
	// Zero-value config takes safe defaults. The worker gets its own context
	// (not rootCtx) so a shutdown signal drains HTTP first; workerCancel
	// fires only after the server has stopped taking traffic.
	worker, err := svc.Worker(service.WorkerConfig{})
	if err != nil {
		return fmt.Errorf("svc.Worker: %w", err)
	}

	// svc.Worker wires the full reconciliation suite itself now, but NOT
	// webhook event delivery -- that one stays an opt-in Set* call on
	// *service.Worker because it needs a subscriber store the library cannot
	// choose for you, and skipping it is silent: events sit in the events
	// table forever, unretried, unlogged, with no error anywhere. This is the
	// wiring gap six independent audit passes each found on their own (see
	// docs/audits/2026-08-25-financial-engineering/consumer-surface.md). A
	// "complete assembly" example has to close it or it teaches the same gap
	// it exists to prevent.
	worker.SetEventDeliverer(delivery.NewWebhookDeliverer(
		postgres.NewEventStore(pool),             // implements delivery.EventPoller
		postgres.NewWebhookSubscriberStore(pool), // implements delivery.SubscriberLister
		core.NopLogger(), nil,                    // metrics nil defaults to a no-op inside NewWebhookDeliverer; logger does not, so it can't be nil
	))

	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()

	// ------------------------------------------------------------------
	// The host app's own chi router — this is where your scaffold's routes
	// live. The ledger's full HTTP API is just another handler on it.
	// ------------------------------------------------------------------
	r := chi.NewRouter()
	r.Get("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("host app is up — ledger API under /api/v1, dashboard on :3090\n"))
	})

	// ------------------------------------------------------------------
	// End-user wallet surface (holder-scoped, read-only). Two pieces:
	//  1. The /api/v1/holder/* endpoints, enabled on the ledger API below
	//     (SetHolderSurface). A library host that doesn't want the admin
	//     API in-process would mount server.HolderHandler(...) instead —
	//     same three endpoints, zero admin routes.
	//  2. A host session endpoint that mints holder tokens IN-PROCESS.
	//     Real apps authenticate their session here and map user → holder;
	//     the demo fixes holder 1001 (seeded above). The ledger API key
	//     never reaches the browser — only this short-lived token does.
	// ------------------------------------------------------------------
	// This endpoint lives on the HOST router, outside the ledger API's CORS
	// middleware — the demo web app calls it cross-origin (:3090 → :8090),
	// so it needs its own CORS headers (dev-wide "*", match your real CORS
	// policy in production).
	sessionCORS := func(w http.ResponseWriter) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
	r.Options("/api/session/wallet-token", func(w http.ResponseWriter, _ *http.Request) {
		sessionCORS(w)
		w.WriteHeader(http.StatusNoContent)
	})
	r.Post("/api/session/wallet-token", func(w http.ResponseWriter, _ *http.Request) {
		sessionCORS(w)
		token, err := server.MintHolderToken(walletTokenSecret, 1001, 15*time.Minute, time.Now())
		if err != nil {
			http.Error(w, "mint failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"token": token})
	})

	ledgerAPI, err := newLedgerAPI(svc)
	if err != nil {
		return fmt.Errorf("assemble ledger API: %w", err)
	}
	rlStop := make(chan struct{})
	ledgerAPI.StartRateLimiterGC(rlStop) // reaps idle per-IP rate-limit buckets
	defer close(rlStop)
	r.Handle("/api/v1/*", ledgerAPI)

	httpServer := &http.Server{
		Addr:              ":" + port,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-rootCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("http shutdown: %v", err)
		}
	}()

	log.Printf("listening on :%s (ledger API at /api/v1)", port)
	httpErr := httpServer.ListenAndServe()

	// HTTP is fully drained (or failed to start) — now stop the worker and
	// wait for its loops to exit. Run returns nil on clean cancellation.
	workerCancel()
	if err := <-workerDone; err != nil {
		return fmt.Errorf("worker: %w", err)
	}
	if httpErr != nil && !errors.Is(httpErr, http.ErrServerClosed) {
		return fmt.Errorf("http: %w", httpErr)
	}
	return nil
}

// newLedgerAPI wires the ledger's HTTP server entirely from svc.* accessors --
// no direct postgres/service dependency.
func newLedgerAPI(svc *ledger.Service) (*server.Server, error) {
	// Dev config: wildcard CORS, no API keys (auth disabled). For production
	// use server.LoadConfig() and set API_KEYS + CORS_ALLOWED_ORIGIN.
	cfg := &server.Config{Env: "dev", CORSAllowOrigin: "*", MaxBodyBytes: 256 * 1024}

	// The HTTP layer is assembled entirely from svc.* accessors -- no direct
	// postgres/service dependency (MJ-5, 2026-08-29 review). The snapshot and
	// system-rollup services the server used to take are Worker concerns, not
	// HTTP ones, and are wired into the Worker separately.
	//
	// NewFromDeps over NewWithConfig: naming each dependency by struct field
	// instead of position removes the "two swapped same-shaped interface
	// arguments compile clean and fail at runtime" trap NewWithConfig has,
	// and it returns an error instead of panicking on an invalid Config.
	srv, err := server.NewFromDeps(cfg, server.Deps{
		Journals:         svc.JournalWriter(),
		Balances:         svc.BalanceReader(),
		Reserver:         svc.Reserver(),
		Booker:           svc.Booker(),
		BookingReader:    svc.BookingReader(),
		EventReader:      svc.EventReader(),
		Classifications:  svc.Classifications(),
		JournalTypes:     svc.JournalTypes(),
		Templates:        svc.Templates(),
		Currencies:       svc.Currencies(),
		Channels:         svc.Channels(),
		Reconciler:       svc.Reconciler(),
		Queries:          svc.Queries(),
		Audit:            svc.Audit(),
		PlatformBalances: svc.PlatformBalanceReader(),
		Solvency:         svc.SolvencyChecker(),
		BalanceTrends:    svc.BalanceTrends(),
		FullReconciler:   svc.FullReconciler(service.FullReconciliationConfig{}),
		AccountPolicies:  svc.AccountPolicies(),
		PeriodCloser:     svc.PeriodCloser(),
		TrialBalance:     svc.TrialBalanceReader(),
	})
	if err != nil {
		return nil, fmt.Errorf("server.NewFromDeps: %w", err)
	}
	srv.SetReady(true)
	if err := srv.SetHolderSurface(server.HolderConfig{TokenSecret: walletTokenSecret}, svc.HolderReader()); err != nil {
		return nil, fmt.Errorf("set holder surface: %w", err) // static demo secret; should not fail, but no more panics in a "complete assembly" example
	}
	return srv, nil
}

// walletTokenSecret signs the demo's holder tokens (32+ bytes). Use an env
// secret (HOLDER_TOKEN_SECRET) in anything beyond a local demo.
var walletTokenSecret = []byte("fullstack-demo-wallet-secret-0123456789")

// seed posts a few confirmed deposits through the preset template so the
// dashboard renders real balances. Idempotent: fixed keys + identical payloads
// resolve to the original journals on re-run.
func seed(ctx context.Context, svc *ledger.Service) error {
	currencyUID, err := ensureCurrency(ctx, svc, "USDT", "Tether USD")
	if err != nil {
		return err
	}

	deposits := []struct {
		holder int64
		amount string
	}{
		{1001, "1500.00"},
		{1002, "250.00"},
		{1003, "75.50"},
	}
	for _, d := range deposits {
		_, err := svc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
			HolderID:       d.holder,
			CurrencyUID:    currencyUID,
			IdempotencyKey: fmt.Sprintf("fullstack-seed-deposit-%d", d.holder),
			Amounts:        map[string]decimal.Decimal{"amount": decimal.RequireFromString(d.amount)},
			Source:         "fullstack-example-seed",
		})
		if err != nil {
			return fmt.Errorf("seed deposit for %d: %w", d.holder, err)
		}
	}
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
