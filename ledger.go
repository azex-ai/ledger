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
	"maps"
	"reflect"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/channel"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/presets"
	"github.com/azex-ai/ledger/service"
)

// Service bundles every store the ledger exposes as a library. Constructed
// once at program startup; safe for concurrent use because every underlying
// store is concurrency-safe.
type Service struct {
	pool *pgxpool.Pool

	// tx is non-nil only on the short-lived clone produced by RunInTx; every
	// store on that clone is rebound to this transaction. Surfaced to callers
	// via DBTX() so user-side raw SQL can land on the same connection as the
	// ledger's writes.
	tx pgx.Tx

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
	configHistoryStore   *postgres.ConfigHistoryStore
	pendingStore         *postgres.PendingStore
	platformBalanceStore *postgres.PlatformBalanceStore
	reconcileAdapter     *postgres.ReconcileAdapter
	accountPolicyStore   *postgres.AccountPolicyStore
	periodCloseStore     *postgres.PeriodCloseStore
	trialBalanceStore    *postgres.TrialBalanceStore
	checkpointIntegrity  *postgres.CheckpointIntegrityStore
	verifiedBalanceStore *postgres.VerifiedBalanceStore

	channelsMu sync.RWMutex
	channels   map[string]channel.Adapter

	// onchain is nil until EnableOnchain is called -- consumers that never
	// opt into the crypto deposit + sweep bundle see no difference (design
	// doc 2026-07-11-crypto-deposit-sweep-design.md: "不配 ChainSet 的消费方零感知").
	onchain *service.Onchain

	// attestor/authVerifier back WithAttestor (design doc §7, P5).
	// attestor == nil (never calling WithAttestor) is the default: every
	// journal posts unsigned, exactly as before P5 existed.
	attestor     core.Attestor
	authVerifier core.AuthVerifier

	// silentWorker records WithSilentWorker: the consumer's explicit
	// acknowledgement that a Worker built from this Service may run with the
	// default no-op logger. See Worker and service.Worker.Run.
	silentWorker bool

	// custodialClassCodes records WithCustodialClassCodes. Empty means the
	// shipped default scope (postgres.DefaultCustodialClassCodes). Read
	// exactly once, by New, to build platformBalanceStore; the scope lives on
	// that store handle afterwards (including across WithDB onto a RunInTx
	// clone), so this field is empty on the clone by design.
	custodialClassCodes []string
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

// WithSilentWorker permits a Worker built from this Service to run with the
// default core.NopLogger. Without it, service.Worker.Run refuses to start
// under that logger and returns an error naming what is missing: every
// signal a worker produces -- which optional jobs are on, whether the
// attestation chain has an external anchor, every job's per-tick failure --
// travels over core.Logger and nowhere else, so a worker booted silently
// cannot be told apart from one that never booted at all.
//
// Use it when silence is a deliberate choice: a test asserting on database
// state rather than logs, or a deployment that reads
// (*service.Worker).StartupReport programmatically. Injecting a real logger
// with WithLogger is the other, usually better, answer.
func WithSilentWorker() Option {
	return func(s *Service) {
		s.silentWorker = true
	}
}

// WithCustodialClassCodes names the classifications SolvencyCheck treats as
// the platform's custodied asset position — the assets side of "do the
// balances we owe holders have something behind them".
//
// The default (no call) is postgres.DefaultCustodialClassCodes:
// {custodial, settlement}. Name your own set when your deployment's custody
// accounts are called something else, or when reserves sit across more
// classifications than the shipped presets create. Passing no codes is
// ignored, so this option can never silently empty the scope.
//
// A scope that matches no classification is rejected the first time solvency
// is read (core.ErrInvalidInput), not reported as Custodial = 0: a custody
// figure that could only ever be zero is a broken configuration, and
// answering "insolvent" to it would be an alarm nailed to ON. This library
// performs no I/O at construction (see New), so that check necessarily
// happens at read time rather than here.
func WithCustodialClassCodes(codes ...string) Option {
	return func(s *Service) {
		if len(codes) > 0 {
			s.custodialClassCodes = append([]string(nil), codes...)
		}
	}
}

// WithAttestor configures the per-journal authorization signing subsystem
// (docs/plans/2026-08-21-tamper-evident-ledger-design.md §7, P5). attestor
// signs every journal PostJournal posts (in pool mode -- see
// postgres.LedgerStore.PostJournal's doc comment for the tx-mode
// exception); authVerifier is stored only for consumers (see
// (*Service).AuthVerifier) -- LedgerStore itself never verifies.
//
// Never calling this option (the default) leaves every journal unsigned,
// exactly as before P5 existed (expand-safe). attestor must already be a
// working implementation by the time it is passed here -- e.g.
// authdev.NewLocalAttestor's own error return is where a bad key/seed
// fails, at the caller's composition root, not inside this library.
func WithAttestor(attestor core.Attestor, authVerifier core.AuthVerifier) Option {
	return func(s *Service) {
		s.attestor = attestor
		s.authVerifier = authVerifier
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
		pool:     pool,
		logger:   core.NopLogger(),
		metrics:  core.NopMetrics(),
		channels: make(map[string]channel.Adapter),
	}
	for _, opt := range opts {
		opt(s)
	}

	s.ledgerStore = postgres.NewLedgerStore(pool)
	if s.attestor != nil {
		s.ledgerStore = s.ledgerStore.WithAuth(s.attestor)
	}
	// s.authVerifier may still be nil here (WithAttestor never called) --
	// VerifiedBalanceStore treats that the same way (*Service).AuthVerifier
	// does: every dimension with at least one contributing journal comes
	// back UNDEFINED, never a fabricated "verified" zero. Built before
	// reserverStore because Reserve's optional RequireVerifiedBalance gate
	// (contracts §W2-2) depends on it.
	s.verifiedBalanceStore = postgres.NewVerifiedBalanceStore(pool, s.authVerifier)
	s.reserverStore = postgres.NewReserverStore(pool, s.ledgerStore, s.verifiedBalanceStore)
	s.bookingStore = postgres.NewBookingStore(pool)
	s.eventStore = postgres.NewEventStore(pool)
	// The composition root EventStore.SetLogger's own doc comment points at
	// is this one. Until this line existed the setter had zero production
	// call sites, so the "claim lost, outcome dropped" warnings -- the only
	// signal that a delivery lease was stolen -- went to slog.Default()
	// instead of the logger WithLogger installed, for every consumer.
	s.eventStore.SetLogger(s.logger)
	s.classStore = postgres.NewClassificationStore(pool)
	s.tmplStore = postgres.NewTemplateStore(pool)
	s.currencyStore = postgres.NewCurrencyStore(pool)
	s.queryStore = postgres.NewQueryStore(pool)
	s.snapshotExtraStore = postgres.NewSnapshotExtraStore(pool)
	s.balanceTrendsStore = postgres.NewBalanceTrendsStore(pool, s.ledgerStore)
	s.auditStore = postgres.NewAuditStore(pool)
	s.configHistoryStore = postgres.NewConfigHistoryStore(pool)
	s.pendingStore = postgres.NewPendingStore(pool, s.ledgerStore, s.classStore)
	s.platformBalanceStore = postgres.NewPlatformBalanceStore(pool)
	if len(s.custodialClassCodes) > 0 {
		s.platformBalanceStore = s.platformBalanceStore.WithCustodialClassCodes(s.custodialClassCodes...)
	}
	s.reconcileAdapter = postgres.NewReconcileAdapter(pool)
	s.accountPolicyStore = postgres.NewAccountPolicyStore(pool)
	s.periodCloseStore = postgres.NewPeriodCloseStore(pool)
	s.trialBalanceStore = postgres.NewTrialBalanceStore(pool)
	s.checkpointIntegrity = postgres.NewCheckpointIntegrityStore(pool)

	return s, nil
}

// Pool returns the underlying connection pool. Useful for callers that need
// transactional access alongside the ledger (the ledger itself does not hand
// out transactions).
//
// clone-safe: returns the pool on the transaction-bound clone too, by design
// -- the clone shares the Service's pool and only its store handles are
// rebound. Work issued through the returned pool from inside a RunInTx
// callback therefore lands OUTSIDE that transaction and commits
// independently; use DBTX() when you mean "the same transaction as the
// ledger's writes".
func (s *Service) Pool() *pgxpool.Pool { return s.pool }

// DBTX returns the database executor that the ledger's stores are currently
// bound to. On a top-level Service it is the connection pool. On the clone
// passed to a RunInTx callback it is the active pgx.Tx, so caller-owned raw
// SQL run via DBTX().Exec lands on the same transaction as the ledger writes.
//
// Use DBTX (not Pool) inside RunInTx when composing your own writes with
// ledger writes — Pool always returns the underlying pool and would commit
// outside the surrounding transaction.
func (s *Service) DBTX() postgres.DBTX {
	if s.tx != nil {
		return s.tx
	}
	return s.pool
}

// JournalWriter posts/reverses journals and executes templates.
func (s *Service) JournalWriter() core.JournalWriter { return s.ledgerStore }

// Authorize computes input's canonical digest and, if WithAttestor was
// used, signs it -- entirely outside any transaction, so the result
// (core.AuthorizedJournal) can safely be posted from inside a RunInTx
// callback via JournalWriter().PostAuthorized (design doc §7.5). Call it
// on the top-level Service, strictly before RunInTx opens; calling it on
// the *Service passed into a RunInTx callback returns an error (that
// Service is already transaction-bound, and financial.md forbids calling
// out to an Attestor from inside an open transaction).
func (s *Service) Authorize(ctx context.Context, input core.JournalInput) (core.AuthorizedJournal, error) {
	return s.ledgerStore.Authorize(ctx, input)
}

// AuthorizeTemplate renders templateCode with params into a
// core.JournalInput, then calls Authorize on it -- the template-driven
// equivalent of Authorize, for callers (e.g. service.Onchain, via
// TxComposer) that build their journal from a template rather than a raw
// JournalInput. Same placement rule as Authorize: call before RunInTx
// opens.
func (s *Service) AuthorizeTemplate(ctx context.Context, templateCode string, params core.TemplateParams) (core.AuthorizedJournal, error) {
	input, err := s.ledgerStore.RenderTemplate(ctx, templateCode, params)
	if err != nil {
		return core.AuthorizedJournal{}, err
	}
	return s.ledgerStore.Authorize(ctx, *input)
}

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

// JournalTypes manages journal types. The adapter re-routes
// SetDisplayLabelIfEmpty to the journal-type variant — the bare classStore
// would structurally satisfy the interface but write classification labels.
func (s *Service) JournalTypes() core.JournalTypeStore {
	return postgres.JournalTypeStoreAdapter{ClassificationStore: s.classStore}
}

// HolderReader serves the holder-scoped wallet read surface (balances,
// translated transactions, holds) — feed it to server.HolderHandler or
// consume it directly.
func (s *Service) HolderReader() core.HolderReader { return s.ledgerStore }

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
	extra := s.snapshotExtraStore
	if s.tx != nil {
		rollup = rollup.WithDB(s.tx)
		extra = extra.WithDB(s.tx)
	}
	svc := service.NewSnapshotBackfillService(rollup, extra, extra, engine)
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

// AttestationService returns a *service.AttestationService wired to this
// ledger's connection pool and the Attestor/AuthVerifier configured via
// WithAttestor (design doc §8, P6; T4, design doc §8 extended). anchor may
// be nil (see service.AttestationService.anchor's doc comment); it is not
// read from WithAttestor's config because P5's Attestor/AuthVerifier and
// P6's Anchor answer genuinely different questions ("is this key wired at
// all" vs "which external carrier"), and only the latter has no
// library-shipped production implementation to default to.
//
// s.authVerifier may be nil here (WithAttestor was called with a nil
// verifier, or never called with attestor set to a non-nil value via some
// other path) -- that is T4's own opt-in switch (see
// service.AttestationService.verifier's doc comment), not an error: the
// returned service still attests P6's batch chain, it just never computes
// auth verdicts.
//
// Returns an error if WithAttestor was never called -- RunAttestBatch has
// nothing to sign with otherwise, and failing here (once, at construction)
// is clearer than failing on every tick. Also returns an error when called
// on the transaction-bound clone RunInTx hands to its callback: the
// returned service reads/writes through s.pool directly (postJournal
// batches spanning many rows, not something that belongs inside a caller's
// transaction), so building one from a clone would silently operate outside
// the transaction the caller thinks it is composing with. Call this on the
// top-level Service, before or after RunInTx, never from inside it.
func (s *Service) AttestationService(anchor core.Anchor) (*service.AttestationService, error) {
	if s.tx != nil {
		return nil, fmt.Errorf("ledger: attestation service: called on a transaction-bound store; AttestationService reads/writes through the pool directly and must not be built from inside RunInTx: %w", core.ErrInvalidInput)
	}
	if s.attestor == nil {
		return nil, fmt.Errorf("ledger: attestation service: WithAttestor was never called")
	}
	return s.attestationServiceUnchecked(anchor), nil
}

// attestationServiceUnchecked builds the *service.AttestationService without
// re-checking s.attestor/s.tx -- callers (AttestationService above, and
// (*Service).Worker's auto-wiring) that have already established both
// preconditions use this directly so the precondition failure has exactly
// one error message, not two copies that could drift.
func (s *Service) attestationServiceUnchecked(anchor core.Anchor) *service.AttestationService {
	engine := core.NewEngine(core.WithLogger(s.logger), core.WithMetrics(s.metrics))
	store := postgres.NewAttestationStore(s.pool)
	return service.NewAttestationService(store, s.attestor, s.authVerifier, anchor, engine)
}

// VerifyLedger runs the five-step tamper-evidence verification (design doc
// §8.4) against this ledger: pull the trusted head from anchor, walk and
// check the attestation chain, recompute batch content from live entries,
// sample per-journal authorization signatures, and localize any mismatch to
// specific entry ids.
//
// Exists because every other capability of this library reaches the consumer
// through this facade, and verification did not: running it meant calling
// postgres.NewAttestationStore(pool) directly, which the package contract
// (CLAUDE.md) tells consumers never to do. cmd/ledger-cli could get away
// with it -- it lives in this repository and is not a consumer -- but an
// example that did the same would teach the wrong layering.
//
// anchor is required: without it there is no trusted head to compare
// against, and VerifyLedger returns NOT_RUN rather than a partial VERIFIED.
// The same is true when WithAttestor was never given a verifier. Both are
// fail-closed by design -- a check that could not run must never read as one
// that passed.
//
// Also returns NOT_RUN, for the same fail-closed reason, when called on the
// transaction-bound clone RunInTx hands to its callback: the attestation
// chain is read through s.pool directly regardless, while other checks
// would read through the clone's transaction -- mixing a live transactional
// view with a pool-level snapshot inside one verification run would make a
// "mismatch" finding ambiguous between "tampered" and "read before the
// transaction committed". Call this on the top-level Service.
//
// cfg's zero value is usable; see service.DefaultVerifyConfig for what the
// defaults are.
func (s *Service) VerifyLedger(ctx context.Context, anchor core.Anchor, cfg service.VerifyConfig) service.VerifyReport {
	if s.tx != nil {
		return service.VerifyReport{
			Status:  service.VerifyStatusNotRun,
			Reasons: []string{"VerifyLedger called on a transaction-bound store; call it on the top-level Service, not from inside RunInTx"},
		}
	}
	if cfg.JournalSampleSize == 0 && cfg.ChainPageSize == 0 && cfg.ReferenceEntries == nil {
		cfg = service.DefaultVerifyConfig()
	}
	store := postgres.NewAttestationStore(s.pool)
	return service.VerifyLedger(ctx, store, anchor, s.authVerifier, s.queryStore, cfg)
}

// Reconciler returns a core.Reconciler for the basic accounting-equation and
// per-account checks the HTTP reconcile endpoints expose. Provided so an
// HTTP-mode composition root can wire server.NewWithConfig entirely from
// svc.* accessors without importing the postgres adapter directly (the
// facade's contract; MJ-5, 2026-08-29 review). Honors an in-flight RunInTx
// transaction, like the other accessors.
func (s *Service) Reconciler() core.Reconciler {
	engine := core.NewEngine(core.WithLogger(s.logger), core.WithMetrics(s.metrics))
	rollupAdapter := postgres.NewRollupAdapter(s.pool)
	if s.tx != nil {
		rollupAdapter = rollupAdapter.WithDB(s.tx)
	}
	return service.NewReconciliationService(rollupAdapter, rollupAdapter, rollupAdapter, rollupAdapter, engine)
}

// FullReconciler returns a core.FullReconciler that runs the full
// reconciliation suite. cfg is optional; zero-value uses sensible defaults.
func (s *Service) FullReconciler(cfg service.FullReconciliationConfig) core.FullReconciler {
	engine := core.NewEngine(core.WithLogger(s.logger), core.WithMetrics(s.metrics))
	rollupAdapter := postgres.NewRollupAdapter(s.pool)
	reconcileAdapter := s.reconcileAdapter
	if s.tx != nil {
		rollupAdapter = rollupAdapter.WithDB(s.tx)
		reconcileAdapter = reconcileAdapter.WithDB(s.tx)
	}
	basic := service.NewReconciliationService(rollupAdapter, rollupAdapter, rollupAdapter, rollupAdapter, engine)
	full := service.NewFullReconciliationService(basic, reconcileAdapter, cfg, engine)
	// unauthorized_journals (contracts §W2-2, I-32): wired unconditionally,
	// even when s.authVerifier is nil -- SetAuthCheck's own contract is to
	// skip the check (Complete=false) rather than run with a nil verifier,
	// so there is no default policy decision being made here.
	full.SetAuthCheck(s.queryStore, s.authVerifier)
	return full
}

// RunInTx begins a new PostgreSQL transaction, builds a short-lived Service
// clone with every store rebound to that transaction, and calls fn with the
// clone. If fn returns nil the transaction is committed; any non-nil error
// causes a rollback. Panics roll back through the deferred cleanup and are
// then propagated unchanged to preserve the caller's panic semantics.
//
// The *Service passed to fn is valid only for the duration of fn — do not
// store it or use it after fn returns. It is also NOT safe for concurrent
// use: every store on the clone shares one pgx.Tx, and pgx transactions do
// not support concurrent statements — do not spawn goroutines inside fn that
// call the clone in parallel.
//
// Use RunInTxWithOptions when a specific isolation or access mode is required.
//
// Caveats when operating inside a RunInTx callback:
//   - GetBalance does NOT start its own REPEATABLE READ sub-transaction; the
//     transaction's isolation level (READ COMMITTED by default) applies.
//   - Advisory locks acquired inside fn are held until commit/rollback — this
//     is correct behaviour for the balance-locking invariant.
//   - Nesting is not supported: calling RunInTx (or RunInTxWithOptions) again
//     on the *Service handed to fn returns an error rather than silently
//     opening a second, independent transaction on a fresh pool connection
//     (which would defeat the atomicity RunInTx exists to provide, and can
//     self-deadlock if the outer and inner transactions both want the same
//     advisory lock). Compose your writes into one callback instead.
//   - core.JournalWriter.PostJournal / ExecuteTemplate / ExecuteTemplateBatch
//     called on the clone can never sign a journal, even when this Service
//     was constructed WithAttestor: there is no point left inside an
//     already-open transaction to call the Attestor without violating
//     financial.md. Every journal posted this way carries
//     core.AuthStatusUnsignedTxMode, and once posted it stays that way
//     forever (journals are append-only) — VerifiedBalanceReader treats any
//     dimension with such a contributing journal as permanently UNDEFINED,
//     with no remediation API. If a journal composed inside RunInTx must be
//     verifiable later, call Authorize (or AuthorizeTemplate) on the
//     top-level Service BEFORE RunInTx opens, then call PostAuthorized on
//     the clone inside the callback — see service/onchain.go's
//     postDepositConfirmedJournal for the pattern this library's own
//     internal callers use.
//   - Six methods reach past the clone's transaction and are refused on it,
//     because each would otherwise either operate outside the transaction the
//     caller believes it is composing with, or report success for a change
//     nobody keeps. Call them on the top-level Service:
//     AttestationService, VerifyLedger and Worker read/write through the pool
//     directly; EnableOnchain and RegisterChannel would set state on a Service
//     value discarded when fn returns; VerifiedBalanceReader's returned reader
//     would call a (possibly remote) core.AuthVerifier from inside the open
//     transaction, with the balance advisory lock held — see its own doc
//     comment for the correct gate-then-compose ordering.
//   - Every other exported method either operates correctly on the
//     transaction or declares its clone behaviour with a `clone-safe:` note in
//     its doc comment. That is not a promise maintained by hand:
//     TestCloneEscapeSurfaceIsDeclaredOrGuarded walks ledger.go's AST and
//     fails on any exported *Service method that touches s.pool or writes a
//     Service field without either an `s.tx != nil` guard or that note.
//     Currently declared rather than guarded: Pool and Ping (see each).
//   - Onchain() is readable on the clone and returns the same
//     *service.Onchain the top-level Service has (EnableOnchain still refuses
//     to configure one from here).
func (s *Service) RunInTx(ctx context.Context, fn func(*Service) error) error {
	return s.RunInTxWithOptions(ctx, pgx.TxOptions{}, fn)
}

// RunInTxWithOptions is RunInTx with explicit PostgreSQL transaction options.
func (s *Service) RunInTxWithOptions(ctx context.Context, opts pgx.TxOptions, fn func(*Service) error) error {
	if s.tx != nil {
		return fmt.Errorf("ledger: RunInTx: already inside a transaction (nested RunInTx is not supported: it would open an independent transaction from the pool instead of composing with the outer one — call fn's *Service directly instead): %w", core.ErrInvalidInput)
	}

	tx, err := s.pool.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("ledger: RunInTx: begin: %w", err)
	}

	// Ensure rollback on any exit path (commit below overrides this on success).
	committed := false
	defer func() {
		if !committed {
			// Rollback must still reach PostgreSQL when fn returns because its
			// request context was cancelled. Bound the detached cleanup so a
			// broken connection cannot stall the caller indefinitely.
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(cleanupCtx)
		}
	}()

	if err := fn(s.withTx(tx)); err != nil {
		return err
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

// AssertRuntimeRole reports whether this Service's connection authenticates as
// the database role the schema's ACLs are written against (`ledger_app`).
//
// Call it once from the composition root, at startup, and decide there what a
// mismatch means. It is not called automatically: migrations, development and
// the runbook's recovery procedures all connect as something else on purpose,
// and there is no default this library could pick that is right for all of
// them. What it can do is stop the prerequisite from being a sentence in a
// README that nothing checks.
//
// What is at stake is not access control -- a wrong role usually has MORE
// access, not less. It is that several invariants are enforced by GRANTs
// naming this one role: I-42 (journal_entries.id comes from the sequence
// alone, which is what keeps the balance equation's checkpoint+delta scan
// monotonic) is a column-level INSERT grant; the append-only guards, the
// webhook_subscribers write narrowing and the function EXECUTE whitelist are
// the same shape. On a connection as ledger_owner they are not violated -- they
// are absent, silently, and everything keeps working until it does not.
//
// Returns a core.ErrInvalidInput-wrapped error naming both roles on mismatch.
func (s *Service) AssertRuntimeRole(ctx context.Context) error {
	return postgres.CheckRuntimeRole(ctx, s.DBTX(), postgres.AppRole)
}

// ConfigHistory returns the read side of the forensic trail: who changed the
// rules that decide where money goes, and when.
//
// Exposed because migration 006 built that trail and nothing read it -- no
// query, no store method, no command, only tests. A consumer whose only route
// to the evidence is to go schema-diving during an incident has, in practice,
// no route to it: "somebody tampered with the config" and "nothing ever
// happened" stayed indistinguishable on every surface anyone reaches.
//
// Read alongside Audit(): that answers what the ledger recorded, this answers
// what the ledger was told to record it as.
func (s *Service) ConfigHistory() core.ConfigChangeReader { return s.configHistoryStore }

// AccountPolicies manages per-account freeze/close + balance-floor overrides.
func (s *Service) AccountPolicies() core.AccountPolicyStore { return s.accountPolicyStore }

// PeriodCloser manages the accounting period close line.
func (s *Service) PeriodCloser() core.PeriodCloser { return s.periodCloseStore }

// TrialBalanceReader computes a trial balance report.
func (s *Service) TrialBalanceReader() core.TrialBalanceReader { return s.trialBalanceStore }

// AuthVerifier returns the core.AuthVerifier passed to WithAttestor, or
// nil if it was never called. LedgerStore itself never calls this --
// signature verification is a downstream concern (a withdrawal gate,
// reconcile check, or ledger-cli verify -- none wired by P5, design doc
// §7.3/§7.4/§12); this accessor exists so those future consumers, and pin
// tests today, can reach the same verifier the composition root wired in.
func (s *Service) AuthVerifier() core.AuthVerifier { return s.authVerifier }

// CheckpointIntegrity returns the trusted, entries-only balance API
// (RecomputeBalance / RebuildCheckpoint) that never consults
// balance_checkpoints. See core.CheckpointIntegrityStore: withdrawal /
// large-amount paths must call RecomputeBalance instead of
// BalanceReader.GetBalance.
func (s *Service) CheckpointIntegrity() core.CheckpointIntegrityStore { return s.checkpointIntegrity }

// VerifiedBalanceReader returns the withdrawal-time authorization-gated
// balance reader (docs/plans/2026-08-21-integrity-hardening-contracts.md
// §W2-1). See core.VerifiedBalanceReader for the full contract, in
// particular the UNDEFINED case: calling code MUST check the returned
// error before trusting the amount. This is a mechanism the library
// offers, not a policy it imposes -- nothing in this library calls it
// automatically (e.g. Reserve does not), so a consumer that never calls
// this accessor sees no behavior change at all (contracts §W2-3).
//
// Call it on the top-level Service, never from inside a RunInTx callback:
// the gate verifies every contributing journal live, and core.AuthVerifier
// is explicitly allowed to run off-host, so calling it from inside an open
// transaction is the "external call inside a DB transaction" financial.md
// forbids -- with the balance advisory lock held across every round trip.
// The returned reader refuses on a transaction-bound store for that reason.
// The correct ordering is the one service/onchain.go's
// postDepositConfirmedJournal uses: clear the gate on the pool first, then
// open RunInTx and compose the withdrawal journal inside the callback.
func (s *Service) VerifiedBalanceReader() core.VerifiedBalanceReader { return s.verifiedBalanceStore }

// withTx returns a short-lived Service clone with every store rebound to tx.
// The clone shares pool and options with the original; only the store handles
// change. The caller (RunInTx) owns the transaction lifecycle.
// pgx.Tx satisfies postgres.DBTX (it has Exec, Query, QueryRow, and Begin).
func (s *Service) withTx(tx pgx.Tx) *Service {
	ls := s.ledgerStore.WithDB(tx)
	cs := s.classStore.WithDB(tx)
	s.channelsMu.RLock()
	channels := maps.Clone(s.channels)
	s.channelsMu.RUnlock()
	return &Service{
		pool:    s.pool,
		tx:      tx,
		logger:  s.logger,
		metrics: s.metrics,
		// attestor/authVerifier: carried onto the clone so accessors that
		// read them directly (AuthVerifier(), and the s.tx-guarded
		// AttestationService()/VerifyLedger() above) see the same
		// configuration the top-level Service has, rather than silently
		// observing "WithAttestor was never called" for a Service that in
		// fact was. This has no effect on signing itself: ls (the clone's
		// LedgerStore) already carries its own attestor via WithDB (see
		// postgres.LedgerStore.WithDB), and PostJournal's tx-mode branch
		// never consults it regardless -- see RunInTx's doc comment on
		// AuthStatusUnsignedTxMode.
		attestor:     s.attestor,
		authVerifier: s.authVerifier,
		silentWorker: s.silentWorker,
		// custodialClassCodes is deliberately NOT copied: unlike
		// attestor/authVerifier above, nothing reads it after New -- the
		// scope lives on the platformBalanceStore handle, which carries it
		// across WithDB (postgres.PlatformBalanceStore.WithDB), so the
		// clone's solvency reads already use it. Copying it would be a line
		// no test could ever turn red, which is the shape I-54 exists to
		// stop this file accumulating.
		// onchain: carried for exactly the reason spelled out above for
		// attestor/authVerifier -- Onchain() reads the field directly, and
		// dropping it made tx.Onchain() return a bare nil on a Service whose
		// top-level EnableOnchain had succeeded, so `tx.Onchain().IngestDeposit(...)`
		// nil-panicked. Carrying it does not make the clone able to CHANGE
		// onchain: EnableOnchain's own s.tx guard still refuses the write.
		onchain:              s.onchain,
		ledgerStore:          ls,
		reserverStore:        s.reserverStore.WithDB(tx, ls),
		bookingStore:         s.bookingStore.WithDB(tx),
		eventStore:           s.eventStore.WithDB(tx),
		classStore:           cs,
		tmplStore:            s.tmplStore.WithDB(tx),
		currencyStore:        s.currencyStore.WithDB(tx),
		queryStore:           s.queryStore.WithDB(tx),
		snapshotExtraStore:   s.snapshotExtraStore.WithDB(tx),
		balanceTrendsStore:   s.balanceTrendsStore.WithDB(tx, ls),
		auditStore:           s.auditStore.WithDB(tx),
		configHistoryStore:   s.configHistoryStore.WithDB(tx),
		pendingStore:         s.pendingStore.WithDB(tx, ls, cs),
		platformBalanceStore: s.platformBalanceStore.WithDB(tx),
		reconcileAdapter:     s.reconcileAdapter.WithDB(tx),
		accountPolicyStore:   s.accountPolicyStore.WithDB(tx),
		periodCloseStore:     s.periodCloseStore.WithDB(tx),
		trialBalanceStore:    s.trialBalanceStore.WithDB(tx),
		checkpointIntegrity:  s.checkpointIntegrity.WithDB(tx),
		verifiedBalanceStore: s.verifiedBalanceStore.WithDB(tx),
		channels:             channels,
	}
}

// ---------------------------------------------------------------------------
// Migrate — package-level thin alias for postgres.Migrate
// ---------------------------------------------------------------------------

// Migrate runs all pending schema migrations against the given database URL.
// It is a thin re-export of postgres.Migrate so consumers only need to import
// this package:
//
//	if err := ledger.Migrate("pgx5://user:pass@host/db"); err != nil { ... }
func Migrate(databaseURL string) error {
	return postgres.Migrate(databaseURL)
}

// ---------------------------------------------------------------------------
// Preset installation
// ---------------------------------------------------------------------------

// InstallDefaultPresets installs the deposit and withdrawal classification,
// journal-type, and template presets. Safe to call on every startup — existing
// rows are validated and reused.
func (s *Service) InstallDefaultPresets(ctx context.Context) error {
	if err := presets.InstallDefaultTemplatePresets(ctx, s.classStore, s.JournalTypes(), s.tmplStore); err != nil {
		return fmt.Errorf("ledger: install default presets: %w", err)
	}
	return nil
}

// InstallExtendedPresets installs the full preset suite: all 8 bundles —
// deposit, withdrawal, transfer, fee, capital, settlement, spread, and FX.
// (FX is what docs/COOKBOOK.md's buy-credits and cash-out recipes are built
// on; it is installed here, not separately.) Safe to call alongside or after
// InstallDefaultPresets — duplicate rows are validated and skipped.
func (s *Service) InstallExtendedPresets(ctx context.Context) error {
	if err := presets.InstallExtendedPresets(ctx, s.classStore, s.JournalTypes(), s.tmplStore); err != nil {
		return fmt.Errorf("ledger: install extended presets: %w", err)
	}
	return nil
}

// InstallDevCreditPreset installs the developer-credit bundle: the accounting
// half of a "simulate a deposit" facility, which credits a holder with no
// custodied asset behind it. Deliberately absent from both InstallDefaultPresets
// and InstallExtendedPresets — a deployment gains the ability to mint balance
// out of nothing only by naming it here, explicitly.
//
// Journals posted against it are ordinary journals (append-only, corrected via
// reversal). Because their system-side leg is presets.DevCreditClassificationCode
// rather than custodial, the balance they create shows up as a solvency
// shortfall equal to that account's balance — which is the truth.
func (s *Service) InstallDevCreditPreset(ctx context.Context) error {
	if err := presets.InstallDevCreditBundle(ctx, s.classStore, s.JournalTypes(), s.tmplStore); err != nil {
		return fmt.Errorf("ledger: install dev credit preset: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Channel registry
// ---------------------------------------------------------------------------

// RegisterChannel registers an inbound-webhook channel adapter. The name is
// taken from adapter.Name(); registering a nil adapter, an adapter with an
// empty name, or one whose name is already registered returns an error so
// silent collisions cannot bury startup-time misconfiguration.
//
// Call before you pass svc.Channels() into the HTTP layer: server takes a
// snapshot of that map (see Channels), so a registration made after that
// point is invisible to it even though the server has not started listening
// yet. "Before ListenAndServe" is not early enough. Concurrent registrations
// are serialised by a mutex.
//
// Returns an error when called on the transaction-bound clone RunInTx hands
// to its callback: the registration would land in the clone's own channel
// map, which RunInTx discards when the callback returns.
func (s *Service) RegisterChannel(adapter channel.Adapter) error {
	if s.tx != nil {
		return fmt.Errorf("ledger: RegisterChannel: called on a transaction-bound store; the registration would be written to a clone RunInTx discards when the callback returns, leaving the top-level Service without it despite this call reporting success -- call RegisterChannel on the top-level Service: %w", core.ErrInvalidInput)
	}
	if adapter == nil {
		return fmt.Errorf("ledger: RegisterChannel: adapter is nil")
	}
	name := adapter.Name()
	if name == "" {
		return fmt.Errorf("ledger: RegisterChannel: adapter Name() is empty")
	}

	s.channelsMu.Lock()
	defer s.channelsMu.Unlock()
	if _, exists := s.channels[name]; exists {
		return fmt.Errorf("ledger: RegisterChannel: %q already registered", name)
	}
	s.channels[name] = adapter
	return nil
}

// Channels returns a snapshot of all registered channel adapters. The returned
// map is a copy — mutations do not affect the registry, and neither does a
// RegisterChannel call made after this returns: whoever holds the snapshot
// (server.NewWithConfig, typically) keeps serving the set that existed at
// this moment. Register everything first, then call this once.
func (s *Service) Channels() map[string]channel.Adapter {
	s.channelsMu.RLock()
	defer s.channelsMu.RUnlock()
	out := make(map[string]channel.Adapter, len(s.channels))
	for k, v := range s.channels {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------------------
// Onchain (crypto deposit + sweep, optional)
// ---------------------------------------------------------------------------

// EnableOnchain wires and returns the optional crypto deposit + sweep
// subsystem (docs/plans/2026-07-11-crypto-deposit-sweep-design.md). Call at
// most once per Service; reader/scanner/sweeper may be nil to disable the
// corresponding background jobs (e.g. a webhook-only deployment passes a nil
// reader and skips the pull watcher entirely -- service.Onchain.Run degrades
// gracefully per missing dependency).
//
// Validates the M3.1 secure-by-default AutoCreditCeiling fence
// (service.Onchain.ValidateAutoCreditCeilings, design doc §9.2 addendum,
// docs/bugs/2026-07-11-m3-security-review.md MJ1) and the mi5
// ReconcileFailureLimit fence (service.Onchain.ValidateReconcileFailureLimits,
// same doc, mi5) right here, not only in Run(): a push-only/webhook-only
// consumer that drives IngestDeposit straight from an HTTP handler and
// never calls Run() at all (no background jobs needed) must not be able to
// skip either check simply by not calling Run(). On validation failure, no
// *service.Onchain is handed back and s.onchain is left unset, so a caller
// cannot route deposits through an unvalidated instance nor retry
// EnableOnchain with a fixed config (the "already configured" guard above
// only trips once s.onchain is actually set).
func (s *Service) EnableOnchain(chains core.ChainSet, reader core.ChainReader, scanner core.ChainScanner, sweeper core.Sweeper, opts ...service.OnchainOption) (*service.Onchain, error) {
	if s.tx != nil {
		return nil, fmt.Errorf("ledger: EnableOnchain: called on a transaction-bound store; s.onchain would be set on a clone that RunInTx discards when the callback returns, leaving the top-level Service.Onchain() nil despite this call reporting success -- call EnableOnchain on the top-level Service instead: %w", core.ErrInvalidInput)
	}
	if s.onchain != nil {
		return nil, fmt.Errorf("ledger: EnableOnchain: already configured")
	}
	deps := service.OnchainDeps{
		Registry:            postgres.NewDepositAddressStore(s.pool),
		Cursors:             postgres.NewChainCursorStore(s.pool),
		Booker:              s.bookingStore,
		BookingReader:       s.bookingStore,
		Journals:            s.ledgerStore,
		TxComposer:          onchainTxComposer{svc: s},
		Reader:              reader,
		RegistrationRescans: postgres.NewRegistrationRescanStore(s.pool),
		Scanner:             scanner,
		Sweeper:             sweeper,
		DeadLetters:         postgres.NewIngestDeadLetterStore(s.pool),
		// ReorgRecorder is the durable half of reorg handling (W1-onchain
		// G-M8): without it service.Onchain.Run refuses to start whenever a
		// ChainReader is configured, because a detector whose verdict is
		// only a log line that goes quiet after the recheck window leaves
		// on-call nothing to act on. Wired here rather than left to the
		// consumer so the facade's own deployments are never in that state.
		ReorgRecorder:   postgres.NewDepositReorgStore(s.pool),
		Currencies:      s.currencyStore,
		Classifications: s.classStore,
		Logger:          s.logger,
		Metrics:         s.metrics,
	}
	// WithPool FIRST, so a caller-supplied option can still override it.
	//
	// Without this line every advisory-lock single-flight inside Onchain is
	// inert for facade consumers: service.NewLockedJob treats a nil pool as
	// "skip locking and run unconditionally", and EnableOnchain never
	// passed one. So the per-chain sweep lock -- and, as of B-m7, the
	// per-chain watch lock -- existed in service/ and did nothing here,
	// which is the same "the mechanism is implemented, the wiring is
	// absent" shape as F-M1 (SetPartitionService) and I-R1
	// (EventStore.SetLogger). Multi-replica deployments were therefore
	// broadcasting duplicate sweeps at the same nonce and racing the
	// forward-scan cursor that I-52 now relies on holding still.
	opts = append([]service.OnchainOption{service.WithPool(s.pool)}, opts...)
	onchain := service.NewOnchain(deps, chains, opts...)
	if err := onchain.ValidateAutoCreditCeilings(); err != nil {
		return nil, fmt.Errorf("ledger: EnableOnchain: %w", err)
	}
	if err := onchain.ValidateReconcileFailureLimits(); err != nil {
		return nil, fmt.Errorf("ledger: EnableOnchain: %w", err)
	}
	s.onchain = onchain
	return s.onchain, nil
}

// Onchain returns the crypto deposit + sweep subsystem, or nil if
// EnableOnchain was never called.
func (s *Service) Onchain() *service.Onchain { return s.onchain }

// onchainTxComposer adapts (*Service).RunInTx to service.TxComposer, so
// service/onchain.go's confirmed-transition + deposit_confirm journal
// composition shares the exact same atomic-commit mechanism every other
// RunInTx caller uses (examples/crypto-deposit's manual flow, now
// orchestrated), without service/ needing to import this package (which
// would cycle -- this package already imports service/).
type onchainTxComposer struct{ svc *Service }

func (c onchainTxComposer) RunInTx(ctx context.Context, fn func(ctx context.Context, booker core.Booker, journals core.JournalWriter) error) error {
	return c.svc.RunInTx(ctx, func(tx *Service) error {
		return fn(ctx, tx.Booker(), tx.JournalWriter())
	})
}

// AuthorizeTemplate delegates to the top-level Service (never the
// transaction-bound clone RunInTx hands to its callback) -- c.svc is
// always the top-level Service here, since onchainTxComposer is
// constructed once in EnableOnchain, not inside a RunInTx callback. See
// design doc §7.5.
func (c onchainTxComposer) AuthorizeTemplate(ctx context.Context, templateCode string, params core.TemplateParams) (core.AuthorizedJournal, error) {
	return c.svc.AuthorizeTemplate(ctx, templateCode, params)
}

// ---------------------------------------------------------------------------
// Worker accessor
// ---------------------------------------------------------------------------

// Worker builds a fully-wired background Worker from the internal stores and
// the provided WorkerConfig. The caller is responsible for running it:
//
//	worker, err := svc.Worker(service.DefaultWorkerConfig())
//	if err != nil { return err }
//	go func() { _ = worker.Run(ctx) }()
//
// Returns an error when called on the transaction-bound clone RunInTx hands
// to its callback. The Worker built there would be a chimera: its expiration
// service holds stores bound to a transaction RunInTx destroys when the
// callback returns, while everything else (rollup, partition DDL, the event
// poller, the advisory-lock pool, and the AttestationService this method
// auto-wires -- the very object AttestationService() refuses to build from a
// clone) runs on the pool. It would start, log "worker: started", and then
// fail every expiration tick against a closed transaction: expired
// reservations and bookings never reclaimed, forever, behind one Error line
// per tick.
//
// Any zero-valued field on cfg is filled in from service.DefaultWorkerConfig
// so callers get a safe-by-default Worker even when they pass a partially
// populated config or service.WorkerConfig{}. The EventStore and RollupAdapter
// claim-leases are configured from the merged cfg.
//
// If this Service was constructed WithAttestor, the returned Worker also has
// the P6 batch attestation job wired in (Worker.SetAttestor), with anchor
// left nil (no external carrier configured) -- the batch chain still
// advances and every batch is still signed, it just is not published
// anywhere outside this database. Consumers that do have an anchor can
// override this with their own worker.SetAttestor(as) call after Worker
// returns; a plain setter call like that simply replaces the auto-wired
// default. Previously SetAttestor had to be called manually and no shipped
// entry point ever did, so a WithAttestor deployment's batch chain never
// advanced even though DefaultWorkerConfig configured an interval for it.
//
// The full reconciliation suite (Worker.SetFullReconciler) is wired the same
// way, and for the same reason: DefaultWorkerConfig has always configured a
// FullReconcileInterval, so the fifteen checks it runs -- including
// unauthorized_journals, checkpoint_balance and journal_dr_cr -- looked
// enabled while nothing ever registered the reconciler that makes them run.
// Override it after this returns if you want a non-default
// FullReconciliationConfig.
func (s *Service) Worker(cfg service.WorkerConfig) (*service.Worker, error) {
	if s.tx != nil {
		return nil, fmt.Errorf("ledger: Worker: called on a transaction-bound store; the Worker would be stitched from stores bound to a transaction RunInTx discards when the callback returns, while its remaining jobs (and the AttestationService this wires automatically, which AttestationService() itself refuses to build here) run on the pool -- call Worker on the top-level Service: %w", core.ErrInvalidInput)
	}
	cfg = mergeWorkerConfig(cfg)
	engine := core.NewEngine(core.WithLogger(s.logger), core.WithMetrics(s.metrics))

	rollupAdapter := postgres.NewRollupAdapter(s.pool)
	rollupAdapter.SetClaimLease(cfg.RollupClaimLease)

	// A dedicated EventStore, not the shared s.eventStore also handed out by
	// EventReader(): SetClaimLease below mutates a plain unsynchronized
	// field, and s.eventStore is one instance shared by every accessor and
	// every past/future Worker() call on this Service. Reusing it here meant
	// two Worker() calls (even sequential ones building two long-lived
	// workers, e.g. one per replica-local goroutine) silently fought over
	// the same claim lease -- whichever call happened last won, retroactively
	// changing the lease an already-running worker's poller uses -- and
	// under -race, concurrent Worker() calls flagged a genuine data race on
	// that field. A fresh instance per Worker() call has no shared state to
	// race or clobber.
	eventPoller := postgres.NewEventStore(s.pool)
	eventPoller.SetClaimLease(cfg.EventClaimLease)
	// Same wiring as ledger.New's shared EventStore: this instance is the one
	// that actually polls, so its claim-lost warnings are the ones an
	// operator most needs on the injected logger rather than slog.Default().
	eventPoller.SetLogger(s.logger)

	rollupSvc := service.NewRollupService(rollupAdapter, rollupAdapter, rollupAdapter, rollupAdapter, engine)
	expirationSvc := service.NewExpirationService(rollupAdapter, s.reserverStore, s.reserverStore, s.bookingStore, s.bookingStore, engine)
	reconcileSvc := service.NewReconciliationService(rollupAdapter, rollupAdapter, rollupAdapter, rollupAdapter, engine)
	snapshotSvc := service.NewSnapshotService(rollupAdapter, rollupAdapter, engine)
	systemRollupSvc := service.NewSystemRollupService(rollupAdapter, rollupAdapter, engine)

	w := service.NewWorker(rollupSvc, expirationSvc, reconcileSvc, snapshotSvc, systemRollupSvc, cfg, engine)
	// Partition management: keeps the journal_entries monthly-partition
	// horizon ahead of now (advisory-locked; see service/partition.go).
	w.SetPartitionService(service.NewPartitionService(postgres.NewPartitionStore(s.pool), engine))
	w.SetPool(s.pool)
	// Wire the poller that backs Worker.Subscribe. This does not start the
	// callback loop -- only Subscribe does that -- but it means a consumer who
	// subscribes gets a working subscription without having to know about a
	// separate wiring call. Before this, Subscribe built a dispatcher with a
	// nil poller, which failed on every tick, logged, and delivered nothing.
	w.SetLocalPoller(eventPoller)
	// The full reconciliation suite, wired for the same reason SetAttestor is
	// (see this method's doc comment). FullReconciler's own SetAuthCheck
	// contract makes a nil AuthVerifier skip the auth check rather than run
	// with one, so wiring this unconditionally makes no policy decision here.
	w.SetFullReconciler(s.FullReconciler(service.FullReconciliationConfig{}))
	if s.attestor != nil {
		w.SetAttestor(s.attestationServiceUnchecked(nil))
	}
	if s.silentWorker {
		w.AllowSilent()
	}
	return w, nil
}

// mergeWorkerConfig fills non-positive fields of cfg with their counterparts
// from service.DefaultWorkerConfig, so service.WorkerConfig{} (or any partial
// config) produces a Worker with safe intervals — service/worker.go's
// time.NewTicker would otherwise panic on a zero Duration, and a zero batch
// size means a job that runs and processes nothing.
//
// Field-driven rather than a hand-written list of if statements, one per
// field. The list version shipped without AttestInterval/AttestBatchSize for
// a full release: the P6 attestation job's interval stayed zero, runLoop
// skipped it, and nothing anywhere connected "I added two config fields" to
// "I must add two more if statements". Reflection makes adding a field
// sufficient; TestMergeWorkerConfig_FillsEveryField pins that no field is
// left at its zero value, so a field of a kind this function cannot fill
// fails loudly instead of silently disabling a job.
func mergeWorkerConfig(cfg service.WorkerConfig) service.WorkerConfig {
	defaults := reflect.ValueOf(service.DefaultWorkerConfig())
	out := reflect.ValueOf(&cfg).Elem()

	for i := 0; i < out.NumField(); i++ {
		field := out.Field(i)
		if !field.CanSet() {
			continue
		}
		switch field.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			// time.Duration is an int64 kind, so this covers every interval,
			// batch size and lease the config currently holds.
			if field.Int() <= 0 {
				field.Set(defaults.Field(i))
			}
		default:
			// Deliberately not a silent skip: a new field of an unhandled
			// kind must not fall through to "left at its zero value", which
			// is how the AttestInterval gap disabled a job for a release.
			// The pin test asserts every field ends up non-zero and equal to
			// the default, so this branch is reachable only between adding a
			// field and teaching this switch about it.
			continue
		}
	}
	return cfg
}

// ---------------------------------------------------------------------------
// Health check
// ---------------------------------------------------------------------------

// Ping verifies the database executor this Service's stores are bound to by
// executing SELECT 1 through it. Returns a wrapped error on failure.
//
// clone-safe: it probes through DBTX(), so on the clone RunInTx hands to its
// callback it probes that transaction rather than the pool. That is
// deliberate -- a clone accidentally retained past the callback answers
// "tx is closed" here, matching what every one of its data-plane reads and
// writes will answer, instead of reporting a healthy connection while the
// store it belongs to is dead.
func (s *Service) Ping(ctx context.Context) error {
	if _, err := s.DBTX().Exec(ctx, "SELECT 1"); err != nil {
		return fmt.Errorf("ledger: ping: %w", err)
	}
	return nil
}
