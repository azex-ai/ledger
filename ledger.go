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
package ledger

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres"
)

// Service bundles every store the ledger exposes as a library. Constructed
// once at program startup; safe for concurrent use because every underlying
// store is concurrency-safe.
type Service struct {
	pool *pgxpool.Pool

	logger  core.Logger
	metrics core.Metrics

	ledgerStore   *postgres.LedgerStore
	reserverStore *postgres.ReserverStore
	bookingStore  *postgres.BookingStore
	eventStore    *postgres.EventStore
	classStore    *postgres.ClassificationStore
	tmplStore     *postgres.TemplateStore
	currencyStore *postgres.CurrencyStore
	queryStore    *postgres.QueryStore
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
