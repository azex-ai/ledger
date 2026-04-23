package main

import (
	"context"
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

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/server"
	"github.com/azex-ai/ledger/service"
)

// slogAdapter adapts slog.Logger to core.Logger.
type slogAdapter struct {
	l *slog.Logger
}

func (s *slogAdapter) Info(msg string, args ...any)  { s.l.Info(msg, args...) }
func (s *slogAdapter) Warn(msg string, args ...any)  { s.l.Warn(msg, args...) }
func (s *slogAdapter) Error(msg string, args ...any) { s.l.Error(msg, args...) }

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

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	coreLogger := &slogAdapter{l: logger}

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
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("create pool: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping db: %w", err)
	}
	logger.Info("database connected")

	// Create engine
	engine := core.NewEngine(
		core.WithLogger(coreLogger),
	)

	// Create stores
	ledgerStore := postgres.NewLedgerStore(pool)
	reserverStore := postgres.NewReserverStore(pool, ledgerStore)
	depositStore := postgres.NewDepositStore(pool)
	withdrawStore := postgres.NewWithdrawStore(pool)
	classStore := postgres.NewClassificationStore(pool)
	tmplStore := postgres.NewTemplateStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)
	queryStore := postgres.NewQueryStore(pool)

	// Create services
	// For rollup service, we need the RollupQueuer, CheckpointReadWriter, EntrySummer interfaces.
	// These are implemented in the integration_test but we need a rollup store.
	// For now, we create a rollup adapter from the pool.
	rollupAdapter := postgres.NewRollupAdapter(pool)
	rollupSvc := service.NewRollupService(rollupAdapter, rollupAdapter, rollupAdapter, classStore, engine)
	expirationSvc := service.NewExpirationService(rollupAdapter, reserverStore, rollupAdapter, depositStore, rollupAdapter, withdrawStore, nil, nil, engine)
	reconcileSvc := service.NewReconciliationService(rollupAdapter, rollupAdapter, rollupAdapter, classStore, engine)
	snapshotSvc := service.NewSnapshotService(rollupAdapter, rollupAdapter, engine)
	systemRollupSvc := service.NewSystemRollupService(rollupAdapter, rollupAdapter, engine)

	// Create worker
	workerCfg := service.DefaultWorkerConfig()
	worker := service.NewWorker(rollupSvc, expirationSvc, reconcileSvc, snapshotSvc, systemRollupSvc, workerCfg, engine)

	// Create HTTP server
	srv := server.New(
		ledgerStore,   // JournalWriter
		ledgerStore,   // BalanceReader
		reserverStore, // Reserver
		depositStore,  // Depositor
		withdrawStore, // Withdrawer
		classStore,    // ClassificationStore
		classStore,    // JournalTypeStore
		tmplStore,     // TemplateStore
		currencyStore, // CurrencyStore
		reconcileSvc,  // Reconciler
		snapshotSvc,   // Snapshotter
		systemRollupSvc,
		queryStore, // QueryProvider
	)

	httpServer := &http.Server{
		Addr:              ":" + httpPort,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Start worker in background
	go func() {
		logger.Info("worker starting")
		if err := worker.Run(ctx); err != nil {
			logger.Error("worker error", "error", err)
		}
	}()

	// Start HTTP server
	go func() {
		logger.Info("HTTP server starting", "port", httpPort)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP server error", "error", err)
			cancel()
		}
	}()

	// Wait for shutdown signal
	<-ctx.Done()
	logger.Info("shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}

	logger.Info("shutdown complete")
	return nil
}
