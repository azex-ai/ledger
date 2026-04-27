package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/channel"
	chanOnchain "github.com/azex-ai/ledger/channel/onchain"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/observability"
	"github.com/azex-ai/ledger/pkg/slogadapter"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/presets"
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
	// Config from environment
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	httpPort := os.Getenv("HTTP_PORT")
	if httpPort == "" {
		httpPort = "8080"
	}

	// Server config — fails fast on missing CORS_ALLOWED_ORIGIN in production.
	srvCfg, err := server.LoadConfig()
	if err != nil {
		return fmt.Errorf("server config: %w", err)
	}

	rootCtx, rootCancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer rootCancel()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	coreLogger := slogadapter.New(logger)

	// Run migrations (convert postgres:// to pgx5:// for migrate)
	migrateURL := databaseURL
	if strings.HasPrefix(migrateURL, "postgres://") {
		migrateURL = "pgx5://" + migrateURL[len("postgres://"):]
	} else if strings.HasPrefix(migrateURL, "postgresql://") {
		migrateURL = "pgx5://" + migrateURL[len("postgresql://"):]
	}
	logger.Info("running migrations")
	if err := postgres.Migrate(migrateURL); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	// Create connection pool
	pool, err := pgxpool.New(rootCtx, databaseURL)
	if err != nil {
		return fmt.Errorf("create pool: %w", err)
	}
	// pool.Close() must run last — after HTTP shutdown and worker drain.
	defer pool.Close()

	if err := pool.Ping(rootCtx); err != nil {
		return fmt.Errorf("ping db: %w", err)
	}
	logger.Info("database connected")

	// Prometheus observability — wired so /metrics serves real numbers.
	promMetrics := observability.NewPrometheusMetrics()

	// Create engine
	engine := core.NewEngine(
		core.WithLogger(coreLogger),
		core.WithMetrics(promMetrics),
	)

	// Top-level facade builds all the canonical postgres stores.
	svc, err := ledger.New(pool, ledger.WithLogger(coreLogger), ledger.WithMetrics(promMetrics))
	if err != nil {
		return fmt.Errorf("ledger facade: %w", err)
	}

	// Concrete store handles still needed by adapter-only consumers (worker
	// claim lease tuning, webhook subscriber store).
	bookingStore := postgres.NewBookingStore(pool)
	eventStore := postgres.NewEventStore(pool)
	webhookSubscriberStore := postgres.NewWebhookSubscriberStore(pool)
	classStore := postgres.NewClassificationStore(pool)
	tmplStore := postgres.NewTemplateStore(pool)

	workerCfg := service.DefaultWorkerConfig()
	eventStore.SetClaimLease(workerCfg.EventClaimLease)

	if err := presets.InstallDefaultTemplatePresets(rootCtx, classStore, classStore, tmplStore); err != nil {
		return fmt.Errorf("install default template presets: %w", err)
	}
	logger.Info("default template presets installed")

	// Create services
	rollupAdapter := postgres.NewRollupAdapter(pool)
	rollupAdapter.SetClaimLease(workerCfg.RollupClaimLease)
	rollupSvc := service.NewRollupService(rollupAdapter, rollupAdapter, rollupAdapter, classStore, engine)
	expirationSvc := service.NewExpirationService(rollupAdapter, svc.Reserver(), bookingStore, bookingStore, engine)
	reconcileSvc := service.NewReconciliationService(rollupAdapter, rollupAdapter, rollupAdapter, classStore, engine)
	snapshotSvc := service.NewSnapshotService(rollupAdapter, rollupAdapter, engine)
	systemRollupSvc := service.NewSystemRollupService(rollupAdapter, rollupAdapter, engine)

	// Create worker with event delivery
	worker := service.NewWorker(rollupSvc, expirationSvc, reconcileSvc, snapshotSvc, systemRollupSvc, workerCfg, engine)

	// Set up webhook delivery (service mode)
	webhookDeliverer := delivery.NewWebhookDeliverer(eventStore, webhookSubscriberStore, coreLogger)
	worker.SetEventDeliverer(webhookDeliverer)

	// Channel adapters
	channels := map[string]channel.Adapter{}
	evmSigningKey := os.Getenv("EVM_WEBHOOK_SECRET")
	if evmSigningKey != "" {
		channels["evm"] = chanOnchain.New([]byte(evmSigningKey))
	}

	// Create HTTP server
	srv := server.NewWithConfig(
		srvCfg,
		svc.JournalWriter(),
		svc.BalanceReader(),
		svc.Reserver(),
		bookingStore, // also Booker; concrete handle reused above for store-only access
		bookingStore, // also BookingReader
		eventStore,   // EventReader (concrete handle for SetClaimLease)
		classStore,
		classStore, // JournalTypeStore (same impl)
		tmplStore,
		svc.Currencies(),
		channels,
		reconcileSvc,
		snapshotSvc,
		systemRollupSvc,
		svc.Queries(),
	)
	srv.SetMetricsHandler(promMetrics.Handler())

	// Rate limiter GC loop — stopped when rateLimiterStop is closed.
	rateLimiterStop := make(chan struct{})
	srv.StartRateLimiterGC(rateLimiterStop)

	httpServer := &http.Server{
		Addr:              ":" + httpPort,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Worker runs under its own derived context so we can drain it
	// independently of HTTP shutdown.
	workerCtx, workerCancel := context.WithCancel(rootCtx)
	workerDone := make(chan error, 1)
	go func() {
		logger.Info("worker starting")
		workerDone <- worker.Run(workerCtx)
	}()

	// Mark ready after worker is launched (migrations already applied above).
	srv.SetReady(true)

	// Start HTTP server
	httpDone := make(chan error, 1)
	go func() {
		logger.Info("HTTP server starting", "port", httpPort)
		err := httpServer.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpDone <- err
			rootCancel()
			return
		}
		httpDone <- nil
	}()

	// Block until signal or HTTP server errors out.
	select {
	case <-rootCtx.Done():
		logger.Info("shutdown signal received")
	case err := <-httpDone:
		if err != nil {
			logger.Error("HTTP server error", "error", err)
		}
	}
	srv.SetReady(false)

	// Step 1: stop accepting new connections, drain in-flight requests (15s).
	httpShutdownCtx, httpShutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer httpShutdownCancel()
	if err := httpServer.Shutdown(httpShutdownCtx); err != nil {
		logger.Error("http shutdown failed", "error", err)
	}

	// Step 2: cancel worker; wait up to 30s for in-flight jobs to finish.
	workerCancel()
	close(rateLimiterStop)
	select {
	case err := <-workerDone:
		if err != nil {
			logger.Error("worker exited with error", "error", err)
		} else {
			logger.Info("worker drained cleanly")
		}
	case <-time.After(30 * time.Second):
		logger.Warn("worker drain timed out after 30s; abandoning in-flight jobs")
	}

	logger.Info("shutdown complete")
	return nil
}
