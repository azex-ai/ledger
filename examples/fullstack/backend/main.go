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
//   - server.NewWithConfig(...)       — the full ledger HTTP API as an http.Handler
//   - r.Handle("/api/v1/*", ...)      — mounting that handler inside a host chi router
//   - svc.Worker(...)                 — background rollup/snapshot/reconcile loops
//
// Run:
//
//	export DATABASE_URL="postgres://user:pass@localhost:5432/ledger_example?sslmode=disable"
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
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/server"
	"github.com/azex-ai/ledger/service"
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

	// Migrations. golang-migrate wants the pgx5:// scheme; accept the common
	// postgres:// form and convert so the example runs with a stock URL.
	migrateURL := dbURL
	if rest, ok := strings.CutPrefix(migrateURL, "postgres://"); ok {
		migrateURL = "pgx5://" + rest
	} else if rest, ok := strings.CutPrefix(migrateURL, "postgresql://"); ok {
		migrateURL = "pgx5://" + rest
	}
	if err := ledger.Migrate(migrateURL); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	pool, err := pgxpool.New(rootCtx, dbURL)
	if err != nil {
		return fmt.Errorf("pgxpool: %w", err)
	}
	defer pool.Close()

	svc, err := ledger.New(pool)
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

	// Background worker: balance rollups, reservation expiry, snapshots,
	// reconciliation. Zero-value config takes safe defaults. The worker gets
	// its own context (not rootCtx) so a shutdown signal drains HTTP first;
	// workerCancel fires only after the server has stopped taking traffic.
	worker := svc.Worker(service.WorkerConfig{})
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

	ledgerAPI := newLedgerAPI(svc, pool)
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

// newLedgerAPI wires the ledger's HTTP server from the facade. Everything
// comes off svc accessors except the three stateless read-side services that
// share one rollup adapter.
func newLedgerAPI(svc *ledger.Service, pool *pgxpool.Pool) *server.Server {
	engine := core.NewEngine()
	rollup := postgres.NewRollupAdapter(pool)
	reconcile := service.NewReconciliationService(rollup, rollup, rollup, rollup, engine)
	snapshot := service.NewSnapshotService(rollup, rollup, engine)
	systemRollup := service.NewSystemRollupService(rollup, rollup, engine)

	// Dev config: wildcard CORS, no API keys (auth disabled). For production
	// use server.LoadConfig() and set API_KEYS + CORS_ALLOWED_ORIGIN.
	cfg := &server.Config{Env: "dev", CORSAllowOrigin: "*", MaxBodyBytes: 256 * 1024}

	srv := server.NewWithConfig(cfg,
		svc.JournalWriter(),
		svc.BalanceReader(),
		svc.Reserver(),
		svc.Booker(),
		svc.BookingReader(),
		svc.EventReader(),
		svc.Classifications(),
		svc.JournalTypes(),
		svc.Templates(),
		svc.Currencies(),
		svc.Channels(),
		reconcile,
		snapshot,
		systemRollup,
		svc.Queries(),
		svc.Audit(),
		svc.PlatformBalanceReader(),
		svc.SolvencyChecker(),
		svc.BalanceTrends(),
		svc.FullReconciler(service.FullReconciliationConfig{}),
		svc.AccountPolicies(),
		svc.PeriodCloser(),
		svc.TrialBalanceReader(),
	)
	srv.SetReady(true)
	if err := srv.SetHolderSurface(server.HolderConfig{TokenSecret: walletTokenSecret}, svc.HolderReader()); err != nil {
		panic(err) // static demo secret; cannot fail
	}
	return srv
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
	for _, c := range list {
		if c.Code == code {
			return c.UID, nil
		}
	}
	created, err := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{Code: code, Name: name, Exponent: 6})
	if err != nil {
		return "", fmt.Errorf("create currency: %w", err)
	}
	return created.UID, nil
}
