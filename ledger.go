// Package ledger is the top-level facade for using azex-ai/ledger as a Go
// library. Construct a single Service from a *pgxpool.Pool and pull whichever
// interfaces your code needs:
//
//	svc, err := ledger.New(pool)
//	if err != nil { return err }
//
//	booker := svc.Booker()
//	balances := svc.BalanceReader()
//
// All accessors return interfaces from the core package so application code
// can depend on core/* without importing the postgres adapter directly.
//
// # Transaction composition
//
// Use RunInTx to combine ledger writes with your own database writes in a
// single atomic transaction:
//
//	err = svc.RunInTx(ctx, func(tx *ledger.Service) error {
//	    _, err := tx.JournalWriter().PostJournal(ctx, journalInput)
//	    return err
//	})
//
// When the callback returns nil the transaction is committed; any non-nil
// error (or a panic) triggers a rollback. The *Service passed to the callback
// is a short-lived clone; do not retain it after the callback returns.
package ledger

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

// Service bundles every store the ledger exposes as a library. Constructed
// once at program startup; safe for concurrent use because every underlying
// store is concurrency-safe.
type Service struct {
	pool *pgxpool.Pool

	logger  core.Logger
	metrics core.Metrics

	ledgerStore          *postgres.LedgerStore
	reserverStore        *postgres.ReserverStore
	bookingStore         *postgres.BookingStore
	eventStore           *postgres.EventStore
	classStore           *postgres.ClassificationStore
	tmplStore            *postgres.TemplateStore
	currencyStore        *postgres.CurrencyStore
	queryStore           *postgres.QueryStore
	snapshotExtraStore   *postgres.SnapshotExtraStore
	balanceTrendsStore   *postgres.BalanceTrendsStore
	auditStore           *postgres.AuditStore
	pendingStore         *postgres.PendingStore
	platformBalanceStore *postgres.PlatformBalanceStore
	reconcileAdapter     *postgres.ReconcileAdapter
}

// Option mutates a Service during construction.
type Option func(*Service)

// WithLogger injects a core.Logger. Defaults to core.NopLogger().
func WithLogger(l core.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithMetrics injects a core.Metrics implementation. Defaults to core.NopMetrics().
func WithMetrics(m core.Metrics) Option {
	return func(s *Service) {
		if m != nil {
			s.metrics = m
		}
	}
}

// New wires every postgres-backed store from a single connection pool. It
// performs no I/O — caller is responsible for migrations and pool lifecycle.
//
// Returns an error if pool is nil so callers don't get a confusing nil-deref
// panic on first use.
func New(pool *pgxpool.Pool, opts ...Option) (*Service, error) {
	if pool == nil {
		return nil, fmt.Errorf("ledger: pool is nil")
	}

	s := &Service{
		pool:    pool,
		logger:  core.NopLogger(),
		metrics: core.NopMetrics(),
	}
	for _, opt := range opts {
		opt(s)
	}

	s.ledgerStore = postgres.NewLedgerStore(pool)
	s.reserverStore = postgres.NewReserverStore(pool, s.ledgerStore)
	s.bookingStore = postgres.NewBookingStore(pool)
	s.eventStore = postgres.NewEventStore(pool)
	s.classStore = postgres.NewClassificationStore(pool)
	s.tmplStore = postgres.NewTemplateStore(pool)
	s.currencyStore = postgres.NewCurrencyStore(pool)
	s.queryStore = postgres.NewQueryStore(pool)
	s.snapshotExtraStore = postgres.NewSnapshotExtraStore(pool)
	s.balanceTrendsStore = postgres.NewBalanceTrendsStore(pool, s.ledgerStore)
	s.auditStore = postgres.NewAuditStore(pool)
	s.pendingStore = postgres.NewPendingStore(pool, s.ledgerStore, s.classStore)
	s.platformBalanceStore = postgres.NewPlatformBalanceStore(pool)
	s.reconcileAdapter = postgres.NewReconcileAdapter(pool)

	return s, nil
}

// Pool returns the underlying connection pool. Useful for callers that need
// transactional access alongside the ledger (the ledger itself does not hand
// out transactions).
func (s *Service) Pool() *pgxpool.Pool { return s.pool }

// JournalWriter posts/reverses journals and executes templates.
func (s *Service) JournalWriter() core.JournalWriter { return s.ledgerStore }

// TemplateBatchExecutor executes multiple templates atomically.
func (s *Service) TemplateBatchExecutor() core.TemplateBatchExecutor { return s.ledgerStore }

// BalanceReader reads balances.
func (s *Service) BalanceReader() core.BalanceReader { return s.ledgerStore }

// Reserver implements reserve/settle/release.
func (s *Service) Reserver() core.Reserver { return s.reserverStore }

// Booker creates and transitions bookings.
func (s *Service) Booker() core.Booker { return s.bookingStore }

// BookingReader reads bookings.
func (s *Service) BookingReader() core.BookingReader { return s.bookingStore }

// EventReader reads events.
func (s *Service) EventReader() core.EventReader { return s.eventStore }

// Classifications manages classifications. Also satisfies core.JournalTypeStore.
func (s *Service) Classifications() core.ClassificationStore { return s.classStore }

// JournalTypes manages journal types. (ClassificationStore in postgres also
// implements JournalTypeStore — this accessor exposes that capability cleanly.)
func (s *Service) JournalTypes() core.JournalTypeStore { return s.classStore }

// Templates manages entry templates.
func (s *Service) Templates() core.TemplateStore { return s.tmplStore }

// Currencies manages currencies.
func (s *Service) Currencies() core.CurrencyStore { return s.currencyStore }

// Queries returns the read-only query provider used by the HTTP layer.
func (s *Service) Queries() core.QueryProvider { return s.queryStore }

// SnapshotBackfiller returns a core.SnapshotBackfiller that fills historical
// snapshot gaps.  The returned service uses sparse storage (only inserts when
// the balance has changed) and can detect gaps on startup via
// (*service.SnapshotBackfillService).CheckAndBackfillOnStartup.
func (s *Service) SnapshotBackfiller() core.SnapshotBackfiller {
	engine := core.NewEngine(core.WithLogger(s.logger), core.WithMetrics(s.metrics))
	rollup := postgres.NewRollupAdapter(s.pool)
	svc := service.NewSnapshotBackfillService(rollup, s.snapshotExtraStore, s.snapshotExtraStore, engine)
	return svc
}

// PendingBalanceWriter returns the two-phase pending balance writer.
// Requires the pending bundle to be installed (presets.InstallPendingBundle).
func (s *Service) PendingBalanceWriter() core.PendingBalanceWriter { return s.pendingStore }

// PendingTimeoutSweeper returns the sweeper that expires stale pending deposits.
// Requires the pending bundle to be installed (presets.InstallPendingBundle).
func (s *Service) PendingTimeoutSweeper() core.PendingTimeoutSweeper { return s.pendingStore }

// PlatformBalanceReader returns the structured platform-balance read API.
// Use this to retrieve per-classification breakdowns split by user-side vs
// system-side holders, and to compute total liability by currency.
func (s *Service) PlatformBalanceReader() core.PlatformBalanceReader {
	return s.platformBalanceStore
}

// SolvencyChecker returns the solvency check API for a single currency.
// It compares total user-side liability against the custodial system balance.
func (s *Service) SolvencyChecker() core.SolvencyChecker { return s.platformBalanceStore }

// FullReconciler returns a core.FullReconciler that runs the complete 10-check
// reconciliation suite. cfg is optional; zero-value uses sensible defaults.
func (s *Service) FullReconciler(cfg service.FullReconciliationConfig) core.FullReconciler {
	engine := core.NewEngine(core.WithLogger(s.logger), core.WithMetrics(s.metrics))
	rollupAdapter := postgres.NewRollupAdapter(s.pool)
	basic := service.NewReconciliationService(rollupAdapter, rollupAdapter, rollupAdapter, s.classStore, engine)
	return service.NewFullReconciliationService(basic, s.reconcileAdapter, cfg, engine)
}

// RunInTx begins a new PostgreSQL transaction, builds a short-lived Service
// clone with every store rebound to that transaction, and calls fn with the
// clone. If fn returns nil the transaction is committed; any non-nil error
// (including a panic recovered internally) causes a rollback.
//
// The *Service passed to fn is valid only for the duration of fn — do not
// store it or use it after fn returns.
//
// Callers that need a specific isolation level should use Pool().BeginTx and
// call the individual store methods directly; RunInTx always uses the default
// READ COMMITTED isolation level.
//
// Caveats when operating inside a RunInTx callback:
//   - GetBalance does NOT start its own REPEATABLE READ sub-transaction; the
//     transaction's isolation level (READ COMMITTED by default) applies.
//   - Advisory locks acquired inside fn are held until commit/rollback — this
//     is correct behaviour for the balance-locking invariant.
func (s *Service) RunInTx(ctx context.Context, fn func(*Service) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("ledger: RunInTx: begin: %w", err)
	}

	// Ensure rollback on any exit path (commit below overrides this on success).
	committed := false
	defer func() {
		if !committed {
			// Ignore rollback error — original error is more informative.
			_ = tx.Rollback(ctx)
		}
	}()

	// Recover panics so we always roll back before re-panicking.
	var callErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				callErr = fmt.Errorf("ledger: RunInTx: panic: %v", r)
			}
		}()
		callErr = fn(s.withTx(tx))
	}()

	if callErr != nil {
		return callErr
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("ledger: RunInTx: commit: %w", err)
	}
	committed = true
	return nil
}

// BalanceTrends returns the historical balance trend reader.
func (s *Service) BalanceTrends() core.BalanceTrendReader { return s.balanceTrendsStore }

// Audit returns the read-only audit query interface.
func (s *Service) Audit() core.AuditQuerier { return s.auditStore }

// withTx returns a short-lived Service clone with every store rebound to tx.
// The clone shares pool and options with the original; only the store handles
// change. The caller (RunInTx) owns the transaction lifecycle.
// pgx.Tx satisfies postgres.DBTX (it has Exec, Query, QueryRow, and Begin).
func (s *Service) withTx(tx pgx.Tx) *Service {
	ls := s.ledgerStore.WithDB(tx)
	cs := s.classStore.WithDB(tx)
	return &Service{
		pool:                 s.pool,
		logger:               s.logger,
		metrics:              s.metrics,
		ledgerStore:          ls,
		reserverStore:        s.reserverStore.WithDB(tx, ls),
		bookingStore:         s.bookingStore.WithDB(tx),
		eventStore:           s.eventStore.WithDB(tx),
		classStore:           cs,
		tmplStore:            s.tmplStore.WithDB(tx),
		currencyStore:        s.currencyStore.WithDB(tx),
		queryStore:           s.queryStore.WithDB(tx),
		snapshotExtraStore:   s.snapshotExtraStore,
		balanceTrendsStore:   s.balanceTrendsStore.WithDB(tx, ls),
		auditStore:           s.auditStore.WithDB(tx),
		pendingStore:         s.pendingStore.WithDB(tx, ls, cs),
		platformBalanceStore: s.platformBalanceStore, // read-only, pool-backed is fine
		reconcileAdapter:     s.reconcileAdapter,     // read-only, pool-backed is fine
	}
}
