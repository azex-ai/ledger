// Package service: onchain.go
//
// Onchain orchestrates the crypto deposit + sweep bundle
// (docs/plans/2026-07-11-crypto-deposit-sweep-design.md): deriving and
// registering CREATE2 deposit addresses, ingesting deposit sightings from
// both the pull watcher (chains/evm) and the push webhook
// (channel/onchain), advancing deposit bookings through their confirmation
// lifecycle, detecting reorgs, and periodically sweeping registered
// addresses into the factory's treasury. Like the rest of service/, this
// file only sees core ports -- RPC, signing, and event-log parsing live in
// the chains/evm adapter module.
package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"golang.org/x/sync/errgroup"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/presets"
)

// --- consumer-side ports (defined here, implemented elsewhere) ---

// TxComposer atomically composes a booking transition with a journal
// template execution, so a deposit's confirmed-transition and its
// deposit_confirm journal commit or roll back together and cross-link via
// EventUID (mirrors examples/crypto-deposit's manual svc.RunInTx flow).
// service/ cannot depend on the root `ledger` package (ledger.go imports
// service -- a reverse dependency would cycle), so the composition root
// (ledger.go) implements this by closing over (*ledger.Service).RunInTx.
type TxComposer interface {
	RunInTx(ctx context.Context, fn func(ctx context.Context, booker core.Booker, journals core.JournalWriter) error) error
	// AuthorizeTemplate renders templateCode with params into a
	// core.JournalInput and, if a core.Attestor is configured, signs it --
	// entirely outside any transaction (design doc §7.5). Call it BEFORE
	// RunInTx opens, then pass the result into the RunInTx callback's
	// core.JournalWriter.PostAuthorized instead of ExecuteTemplate. This
	// is what closes the gap where a journal composed via RunInTx always
	// posted unsigned, regardless of whether an Attestor was configured --
	// P5's headline use case (M5: forged deposit accounting) is exactly a
	// RunInTx-composed journal (postDepositConfirmedJournal below).
	AuthorizeTemplate(ctx context.Context, templateCode string, params core.TemplateParams) (core.AuthorizedJournal, error)
}

// DeadLetterRecorder persists a deposit sighting that IngestDeposit could
// not idempotently reconcile (design doc §6: a CreateBooking ErrConflict is
// a normalization bug signal, not a transient error -- these must never be
// silently dropped or endlessly retried). Implemented by
// postgres.IngestDeadLetterStore.
type DeadLetterRecorder interface {
	RecordDeadLetter(ctx context.Context, sighting core.DepositSighting, idempotencyKey, reason string) error
}

// currencyResolver resolves a currency code (e.g. "USDT") to its uid,
// caching the result. core.CurrencyStore has no GetByCode -- this mirrors
// the list-then-match pattern examples/crypto-deposit uses inline.
type currencyResolver struct {
	mu    sync.RWMutex
	byUID map[string]string // code -> uid
}

func newCurrencyResolver() *currencyResolver {
	return &currencyResolver{byUID: make(map[string]string)}
}

func (r *currencyResolver) resolve(ctx context.Context, currencies core.CurrencyStore, code string) (string, error) {
	r.mu.RLock()
	uid, ok := r.byUID[code]
	r.mu.RUnlock()
	if ok {
		return uid, nil
	}

	list, err := currencies.ListCurrencies(ctx, false)
	if err != nil {
		return "", fmt.Errorf("service: onchain: list currencies: %w", err)
	}
	r.mu.Lock()
	for _, c := range list {
		r.byUID[c.Code] = c.UID
	}
	uid, ok = r.byUID[code]
	r.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("service: onchain: currency code %q not registered: %w", code, core.ErrNotFound)
	}
	return uid, nil
}

// classResolver resolves a classification code to its uid, caching the
// result (classifications never change uid once created).
type classResolver struct {
	mu    sync.RWMutex
	byUID map[string]string // code -> uid
}

func newClassResolver() *classResolver {
	return &classResolver{byUID: make(map[string]string)}
}

func (r *classResolver) resolve(ctx context.Context, classifications core.ClassificationStore, code string) (string, error) {
	r.mu.RLock()
	uid, ok := r.byUID[code]
	r.mu.RUnlock()
	if ok {
		return uid, nil
	}

	class, err := classifications.GetByCode(ctx, code)
	if err != nil {
		return "", fmt.Errorf("service: onchain: resolve classification %q: %w", code, err)
	}
	r.mu.Lock()
	r.byUID[code] = class.UID
	r.mu.Unlock()
	return class.UID, nil
}

// Channel/source/template constants for the onchain subsystem. The
// classification codes themselves are presets.DepositClassificationCode /
// presets.SweepClassificationCode (presets only depends on core, so
// importing it here does not cycle) -- keeping one source of truth avoids
// the two ever drifting apart.
const (
	depositConfirmTemplate = "deposit_confirm"
	onchainChannelName     = "onchain"
	onchainSource          = "onchain"
)

// sweepSystemHolderBase offsets the per-chain sweep system holder well
// outside the realistic range of SystemAccountHolder(userID) negations, so a
// sweep booking's AccountHolder can never collide with a real user's system
// mirror account. Sweep bookings never post a journal (presets.SweepLifecycle
// doc comment), so this value never enters the accounting equation -- it
// exists purely so CreateBooking has a non-zero AccountHolder to key on.
const sweepSystemHolderBase = int64(9_000_000_000_000)

// sweepSystemHolder returns the sentinel AccountHolder used for every sweep
// booking on chainID. Distinct per chain so ledger-cli / audit tooling can
// tell sweep batches on different chains apart.
func sweepSystemHolder(chainID int64) int64 {
	return -(sweepSystemHolderBase + chainID)
}

// OnchainDeps bundles every dependency Onchain needs. Required fields are
// validated at Run() time (not at construction, since NewOnchain returns no
// error) -- calling EnsureDepositAddress/IngestDeposit directly with missing
// deps fails fast with a wrapped core.ErrInvalidInput instead of panicking.
type OnchainDeps struct {
	Registry      core.AddressRegistry
	Cursors       core.ChainCursorStore
	Booker        core.Booker
	BookingReader core.BookingReader
	Journals      core.JournalWriter // used directly only for standalone ReverseJournal (deep-reorg auto-reverse); the confirm path goes through TxComposer
	TxComposer    TxComposer
	Reader        core.ChainReader  // nil: watcher/recheck/reorg loops are skipped
	Scanner       core.ChainScanner // nil: sweep loop is skipped
	Sweeper       core.Sweeper      // nil: sweep loop is skipped
	// DepositConfirmer is the deposit path's second, independent
	// reconciliation source (design doc §9.3). Nil (the default) disables
	// the reconciliation gate entirely -- only TokenConfig.AutoCreditCeiling
	// applies.
	DepositConfirmer    core.DepositConfirmer
	RegistrationRescans core.RegistrationRescanStore
	DeadLetters         DeadLetterRecorder
	Currencies          core.CurrencyStore
	Classifications     core.ClassificationStore
	Logger              core.Logger  // defaults to core.NopLogger()
	Metrics             core.Metrics // defaults to core.NopMetrics()
}

// validateCore checks the dependencies required for the deposit ingestion
// path (EnsureDepositAddress / IngestDeposit) -- the minimum viable
// configuration. Watcher/sweep loops have their own additional checks in Run.
func (d OnchainDeps) validateCore() error {
	missing := func(ok bool, name string) string {
		if ok {
			return ""
		}
		return name
	}
	var names []string
	for _, n := range []string{
		missing(d.Registry != nil, "Registry"),
		missing(d.Cursors != nil, "Cursors"),
		missing(d.Booker != nil, "Booker"),
		missing(d.BookingReader != nil, "BookingReader"),
		missing(d.Journals != nil, "Journals"),
		missing(d.TxComposer != nil, "TxComposer"),
		missing(d.DeadLetters != nil, "DeadLetters"),
		missing(d.Currencies != nil, "Currencies"),
		missing(d.Classifications != nil, "Classifications"),
		missing(d.Reader == nil || d.RegistrationRescans != nil, "RegistrationRescans"),
	} {
		if n != "" {
			names = append(names, n)
		}
	}
	if len(names) > 0 {
		return fmt.Errorf("service: onchain: missing required deps %v: %w", names, core.ErrInvalidInput)
	}
	return nil
}

// OnchainOption mutates an Onchain during construction.
type OnchainOption func(*Onchain)

// WithReorgPolicy sets the deep-reorg handling policy. Default: core.ReorgPolicyManual.
func WithReorgPolicy(p core.ReorgPolicy) OnchainOption {
	return func(o *Onchain) { o.reorgPolicy = p }
}

// WithSweepPolicies configures the sweep job's (chain, token) policies. If
// never called, the sweep loop does not run (design doc §0: sweep is
// evaluated per configured policy, none by default).
func WithSweepPolicies(policies ...core.SweepPolicy) OnchainOption {
	return func(o *Onchain) { o.sweepPolicies = policies }
}

// WithPool attaches a *pgxpool.Pool for pg_try_advisory_lock-based
// single-flight on the sweep job (design doc §4). Without a pool, sweep runs
// unconditionally on every replica -- fine for single-instance deployments.
func WithPool(pool *pgxpool.Pool) OnchainOption {
	return func(o *Onchain) { o.pool = pool }
}

// WithDepositConfirmer wires the deposit path's second, independent
// reconciliation source (design doc §9.3). Without it, the reconciliation
// gate is disabled regardless of TokenConfig.ReconcileCeiling -- only the
// threshold gate (TokenConfig.AutoCreditCeiling) applies.
func WithDepositConfirmer(c core.DepositConfirmer) OnchainOption {
	return func(o *Onchain) { o.deps.DepositConfirmer = c }
}

// WithWatchInterval sets the per-chain forward-scan tick interval. Default: 15s.
func WithWatchInterval(d time.Duration) OnchainOption {
	return func(o *Onchain) { o.watchInterval = d }
}

// WithMaxBlocksPerScan bounds how many blocks a single forward-scan tick
// covers, so a long watcher outage does not request an unbounded log range
// in one call. Default: 2000.
func WithMaxBlocksPerScan(n int64) OnchainOption {
	return func(o *Onchain) { o.maxBlocksPerScan = n }
}

// WithRecheckInterval sets the pending/confirming recheck tick interval. Default: 20s.
func WithRecheckInterval(d time.Duration) OnchainOption {
	return func(o *Onchain) { o.recheckInterval = d }
}

// WithReorgRecheckInterval sets the confirmed-deposit deep-reorg recheck tick interval. Default: 5m.
func WithReorgRecheckInterval(d time.Duration) OnchainOption {
	return func(o *Onchain) { o.reorgRecheckInterval = d }
}

// WithReorgRecheckWindow bounds the deep-reorg recheck to confirmed deposits
// within this many blocks of the current tip -- older confirmations are
// treated as final, bounding the recheck query's cost. Default: 500.
func WithReorgRecheckWindow(blocks int64) OnchainOption {
	return func(o *Onchain) { o.reorgRecheckWindow = blocks }
}

// WithRegistrationRescanTimeout bounds EnsureDepositAddress's background
// historical rescan (design doc §5-2b). Default: 10m.
func WithRegistrationRescanTimeout(d time.Duration) OnchainOption {
	return func(o *Onchain) { o.registrationRescanTimeout = d }
}

// WithSweepStuckAfter sets how long a "sent" sweep booking waits with no
// on-chain inclusion before a gas-bump retry is attempted. Default: 5m.
func WithSweepStuckAfter(d time.Duration) OnchainOption {
	return func(o *Onchain) { o.sweepStuckAfter = d }
}

// WithMaxSweepBumps bounds gas-bump retries before a stuck sweep booking is
// transitioned to failed and alerted on. Default: 5.
func WithMaxSweepBumps(n int) OnchainOption {
	return func(o *Onchain) { o.maxSweepBumps = n }
}

// WithMaxConcurrentRegistrationRescans bounds how many
// EnsureDepositAddress-triggered background rescans (design doc §5-2b) may
// run at once, so a burst of holder onboarding does not fan out an unbounded
// number of concurrent full-history log scans against the RPC provider.
// Default: 4.
func WithMaxConcurrentRegistrationRescans(n int) OnchainOption {
	return func(o *Onchain) { o.maxConcurrentRescans = n }
}

// Onchain orchestrates crypto deposit ingestion and sweep collection
// (design doc). Construct via NewOnchain and start its background jobs via
// Run; EnsureDepositAddress and IngestDeposit may also be called directly
// (e.g. from an HTTP handler) without Run ever having started.
type Onchain struct {
	deps        OnchainDeps
	chains      core.ChainSet
	reorgPolicy core.ReorgPolicy

	sweepPolicies []core.SweepPolicy
	pool          *pgxpool.Pool

	watchInterval              time.Duration
	maxBlocksPerScan           int64
	recheckInterval            time.Duration
	reorgRecheckInterval       time.Duration
	reorgRecheckWindow         int64
	registrationRescanTimeout  time.Duration
	registrationRescanInterval time.Duration
	sweepStuckAfter            time.Duration
	maxSweepBumps              int
	maxConcurrentRescans       int

	currencies *currencyResolver
	classes    *classResolver

	sweepMu   sync.Mutex
	sweepTx   map[string]string // booking uid -> latest broadcast tx hash
	sweepBump map[string]int    // booking uid -> gas-bump attempts

	// reconcileFailures tracks, per booking uid, consecutive
	// DepositConfirmer.ConfirmDeposit errors since the booking last entered
	// "confirming" (mi5 fix, docs/bugs/2026-07-11-m3-security-review.md):
	// reviewGate escalates to review once a booking's count reaches its
	// TokenConfig.ReconcileFailureLimit, instead of retrying the
	// second-source RPC forever. In-memory only -- a process restart resets
	// the count, which only delays escalation (the count restarts from
	// zero on the next failure), it never causes a silent stuck deposit to
	// go unnoticed forever, and it never affects fund safety: reviewGate's
	// fail-closed behavior below the threshold is unchanged.
	reconcileMu       sync.Mutex
	reconcileFailures map[string]int32
}

// NewOnchain builds an Onchain from deps and chains. Options override the
// zero-valued defaults below (design doc §0/§6 default values).
func NewOnchain(deps OnchainDeps, chains core.ChainSet, opts ...OnchainOption) *Onchain {
	if deps.Logger == nil {
		deps.Logger = core.NopLogger()
	}
	if deps.Metrics == nil {
		deps.Metrics = core.NopMetrics()
	}
	o := &Onchain{
		deps:                       deps,
		chains:                     chains,
		reorgPolicy:                core.ReorgPolicyManual,
		watchInterval:              15 * time.Second,
		maxBlocksPerScan:           2000,
		recheckInterval:            20 * time.Second,
		reorgRecheckInterval:       5 * time.Minute,
		reorgRecheckWindow:         500,
		registrationRescanTimeout:  10 * time.Minute,
		registrationRescanInterval: time.Second,
		sweepStuckAfter:            5 * time.Minute,
		maxSweepBumps:              5,
		maxConcurrentRescans:       4,
		currencies:                 newCurrencyResolver(),
		classes:                    newClassResolver(),
		sweepTx:                    make(map[string]string),
		sweepBump:                  make(map[string]int),
		reconcileFailures:          make(map[string]int32),
	}
	for _, opt := range opts {
		opt(o)
	}
	if o.maxConcurrentRescans <= 0 {
		o.maxConcurrentRescans = 1
	}
	if o.maxBlocksPerScan <= 0 {
		o.maxBlocksPerScan = 2000
	}
	return o
}

func (o *Onchain) log() core.Logger      { return o.deps.Logger }
func (o *Onchain) metrics() core.Metrics { return o.deps.Metrics }

// canonicalFactory returns the (factory, initHash) pair every configured
// chain must share (design doc §2: one salt=holder pair derives the same
// address on every chain). Returns core.ErrInvalidInput if the chain set is
// empty or the chains disagree.
func (o *Onchain) canonicalFactory() (factory, initHash string, err error) {
	if len(o.chains) == 0 {
		return "", "", fmt.Errorf("service: onchain: no chains configured: %w", core.ErrInvalidInput)
	}
	for chainID, cfg := range o.chains {
		if cfg.ScanStartBlock < 0 {
			return "", "", fmt.Errorf("service: onchain: scan_start_block must not be negative for chain %d: %w", chainID, core.ErrInvalidInput)
		}
		if factory == "" {
			factory, initHash = cfg.Factory, cfg.InitHash
			continue
		}
		if cfg.Factory != factory || cfg.InitHash != initHash {
			return "", "", fmt.Errorf("service: onchain: chain set factory/init_hash must match across all chains: %w", core.ErrInvalidInput)
		}
	}
	return factory, initHash, nil
}

// validateAutoCreditCeilings enforces the M3.1 secure-by-default fence
// (design doc §9.2 addendum, docs/bugs/2026-07-11-m3-security-review.md
// MJ1): every configured CreditTokens entry, on every chain, must
// deliberately set TokenConfig.AutoCreditCeiling -- either to a positive
// bound, or to the explicit core.UnboundedAutoCredit sentinel. A token left
// at the zero value is refused here rather than silently treated as
// "unbounded", because that silent default is exactly the pre-M3
// single-source-RPC unbounded-mint trust model M3 exists to close.
//
// Called from both ValidateAutoCreditCeilings (the exported form composition
// roots call right after NewOnchain -- see that method's doc comment) and
// Run() (defense in depth for callers that construct Onchain directly and
// only ever call Run()). Neither call site is optional: a push-only/
// webhook-only consumer that wires Onchain via a composition root but never
// calls Run() (no background jobs needed) still gets this check via
// EnableOnchain -> ValidateAutoCreditCeilings.
func (o *Onchain) validateAutoCreditCeilings() error {
	for chainID, cfg := range o.chains {
		for token, tc := range cfg.CreditTokens {
			if !tc.AutoCreditCeilingConfigured() {
				return fmt.Errorf("service: onchain: chain %d token %q has no AutoCreditCeiling configured -- set a positive ceiling to cap unreviewed auto-credit, or core.UnboundedAutoCredit to explicitly accept unbounded single-source-RPC trust (design doc §9.2): %w", chainID, token, core.ErrInvalidInput)
			}
		}
	}
	return nil
}

// ValidateAutoCreditCeilings is the exported form of the M3.1
// secure-by-default startup check (see validateAutoCreditCeilings): every
// CreditTokens entry, on every configured chain, must deliberately set
// AutoCreditCeiling. Composition roots that wire Onchain via NewOnchain
// directly (e.g. ledger.go's EnableOnchain) must call this right after
// construction and refuse to hand back the instance on error -- Run() alone
// is not enough, since some consumers (push-only/webhook-only deployments
// driving IngestDeposit from an HTTP handler with no background jobs) never
// call Run() at all.
func (o *Onchain) ValidateAutoCreditCeilings() error {
	return o.validateAutoCreditCeilings()
}

// validateReconcileFailureLimits enforces the mi5 fix (docs/bugs/2026-07-11-m3-security-review.md
// mi5): whenever the reconciliation gate is actually active for a token
// (OnchainDeps.DepositConfirmer configured and TokenConfig.ReconcileCeiling
// positive), TokenConfig.ReconcileFailureLimit must be a deliberate positive
// integer -- the number of consecutive ConfirmDeposit errors that escalate a
// stuck confirming deposit to review instead of retrying it forever. Left at
// zero, a long-lived second-source outage leaves a legitimate deposit's
// confirming status indistinguishable from "nobody is working on it"
// (working-agreements §3).
//
// Unlike validateAutoCreditCeilings there is no "explicitly unbounded"
// sentinel to accept here: this is an availability fence, not a
// mint-safety one, so a consumer that does not want it simply does not
// activate the reconciliation gate in the first place (leaves
// DepositConfirmer nil or ReconcileCeiling at zero -- design doc §9.3),
// which already makes this field irrelevant.
//
// Called from both ValidateReconcileFailureLimits (the exported form
// composition roots call right after NewOnchain, alongside
// ValidateAutoCreditCeilings) and Run(), same two call sites and same
// rationale as validateAutoCreditCeilings.
func (o *Onchain) validateReconcileFailureLimits() error {
	if o.deps.DepositConfirmer == nil {
		return nil
	}
	for chainID, cfg := range o.chains {
		for token, tc := range cfg.CreditTokens {
			if tc.ReconcileCeiling.IsPositive() && tc.ReconcileFailureLimit <= 0 {
				return fmt.Errorf("service: onchain: chain %d token %q has ReconcileCeiling set but no ReconcileFailureLimit configured -- set a positive limit so a persistent second-source outage escalates a stuck confirming deposit to review instead of retrying forever (mi5, docs/bugs/2026-07-11-m3-security-review.md): %w", chainID, token, core.ErrInvalidInput)
			}
		}
	}
	return nil
}

// ValidateReconcileFailureLimits is the exported form of the mi5 fix's
// startup check (see validateReconcileFailureLimits). Composition roots
// that wire Onchain via NewOnchain directly (e.g. ledger.go's EnableOnchain)
// must call this right after construction, alongside
// ValidateAutoCreditCeilings, and refuse to hand back the instance on
// error -- Run() alone is not enough, for the same push-only/webhook-only
// reason documented on ValidateAutoCreditCeilings.
func (o *Onchain) ValidateReconcileFailureLimits() error {
	return o.validateReconcileFailureLimits()
}

// EnsureDepositAddress derives holder's CREATE2 deposit address from the
// canonical (factory, initHash) shared across every configured chain,
// registers it (idempotent), and durably enqueues one historical rescan per
// chain before returning -- closing the "deposit sent before registration"
// gap without relying on a request-scoped or fire-and-forget goroutine.
func (o *Onchain) EnsureDepositAddress(ctx context.Context, holder int64) (*core.DepositAddress, error) {
	if err := o.deps.validateCore(); err != nil {
		return nil, err
	}
	factory, initHash, err := o.canonicalFactory()
	if err != nil {
		return nil, err
	}
	addr, err := core.DeriveDepositAddress(factory, initHash, holder)
	if err != nil {
		return nil, fmt.Errorf("service: onchain: ensure deposit address: %w", err)
	}
	da, err := o.deps.Registry.EnsureAddress(ctx, core.AddressRegistrationInput{
		AccountHolder: holder,
		Address:       addr,
		Factory:       factory,
		InitHash:      initHash,
	})
	if err != nil {
		return nil, fmt.Errorf("service: onchain: ensure deposit address: %w", err)
	}

	if o.deps.Reader != nil {
		jobs := make([]core.RegistrationRescan, 0, len(o.chains))
		for chainID, cfg := range o.chains {
			jobs = append(jobs, core.RegistrationRescan{
				ChainID: chainID, Address: da.Address, NextBlock: cfg.ScanStartBlock,
			})
		}
		if err := o.deps.RegistrationRescans.EnqueueRegistrationRescans(ctx, jobs); err != nil {
			return nil, fmt.Errorf("service: onchain: ensure deposit address: enqueue registration rescan: %w", err)
		}
	}
	return da, nil
}

// runRegistrationRescansOnce leases a bounded batch and advances each job by
// one RPC-sized block window. Progress is persisted only after every sighting
// in the window has been ingested successfully.
func (o *Onchain) runRegistrationRescansOnce(ctx context.Context) {
	jobs, err := o.deps.RegistrationRescans.ClaimRegistrationRescans(ctx, o.maxConcurrentRescans, o.registrationRescanTimeout)
	if err != nil {
		o.log().Error("service: onchain: claim registration rescans failed", "error", err)
		return
	}
	var wg sync.WaitGroup
	for _, job := range jobs {
		job := job
		wg.Add(1)
		go func() {
			defer wg.Done()
			jobCtx, cancel := context.WithTimeout(ctx, o.registrationRescanTimeout)
			defer cancel()
			if err := o.processRegistrationRescan(jobCtx, job); err != nil {
				delay := time.Second << min(int(job.Attempts), 6)
				if retryErr := o.deps.RegistrationRescans.RetryRegistrationRescan(ctx, job.UID, err.Error(), time.Now().Add(delay), job.Attempts); retryErr != nil {
					o.log().Error("service: onchain: persist registration rescan retry failed", "uid", job.UID, "error", retryErr)
				}
				o.log().Error("service: onchain: registration rescan failed", "chain_id", job.ChainID, "address", job.Address, "error", err)
				o.metrics().RegistrationRescanFailed(job.ChainID)
			}
		}()
	}
	wg.Wait()
}

func (o *Onchain) processRegistrationRescan(ctx context.Context, job core.RegistrationRescan) error {
	latest, err := o.deps.Reader.LatestBlock(ctx, job.ChainID)
	if err != nil {
		return fmt.Errorf("latest block: %w", err)
	}
	if job.NextBlock > latest {
		return o.deps.RegistrationRescans.AdvanceRegistrationRescan(ctx, job.UID, job.NextBlock, true, job.Attempts)
	}
	to := job.NextBlock + o.maxBlocksPerScan - 1
	if to > latest {
		to = latest
	}
	sightings, err := o.deps.Reader.FetchDeposits(ctx, job.ChainID, job.NextBlock, to, []string{job.Address})
	if err != nil {
		return fmt.Errorf("fetch deposits [%d,%d]: %w", job.NextBlock, to, err)
	}
	for _, sighting := range sightings {
		if _, err := o.IngestDeposit(ctx, sighting); err != nil {
			return fmt.Errorf("ingest deposit %s/%d: %w", sighting.TxHash, sighting.TxLogSeq, err)
		}
	}
	return o.deps.RegistrationRescans.AdvanceRegistrationRescan(ctx, job.UID, to+1, to == latest, job.Attempts)
}

// IngestDeposit is the single orchestration entry point both ingestion
// paths -- the chains/evm watcher and the channel/onchain webhook -- funnel
// into (design doc §3). Returns (nil, nil) for sightings this ledger has no
// business booking (unregistered address, non-whitelisted token): not an
// error, just nothing to do.
func (o *Onchain) IngestDeposit(ctx context.Context, s core.DepositSighting) (*core.Booking, error) {
	if err := o.deps.validateCore(); err != nil {
		return nil, err
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("service: onchain: ingest deposit: %w", err)
	}

	cfg, ok := o.chains[s.ChainID]
	if !ok {
		o.log().Info("service: onchain: ingest deposit: chain not configured, ignoring", "chain_id", s.ChainID)
		return nil, nil
	}
	tokenKey := strings.ToLower(s.Token)
	tokenCfg, ok := cfg.CreditTokens[tokenKey]
	if !ok {
		o.log().Info("service: onchain: ingest deposit: token not in credit allowlist, ignoring", "chain_id", s.ChainID, "token", tokenKey)
		return nil, nil
	}

	addr, err := core.ChecksumAddress(s.To)
	if err != nil {
		return nil, fmt.Errorf("service: onchain: ingest deposit: %w", err)
	}
	da, err := o.deps.Registry.GetByAddress(ctx, addr)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			o.log().Warn("service: onchain: ingest deposit: sighting to unregistered address, ignoring", "chain_id", s.ChainID, "to", addr, "tx_hash", s.TxHash)
			return nil, nil
		}
		return nil, fmt.Errorf("service: onchain: ingest deposit: %w", err)
	}

	currencyUID, err := o.currencies.resolve(ctx, o.deps.Currencies, tokenCfg.CurrencyCode)
	if err != nil {
		return nil, fmt.Errorf("service: onchain: ingest deposit: %w", err)
	}

	idemKey := depositIdempotencyKey(s.ChainID, s.TxHash, s.TxLogSeq)
	booking, err := o.deps.Booker.CreateBooking(ctx, core.CreateBookingInput{
		ClassificationCode: presets.DepositClassificationCode,
		AccountHolder:      da.AccountHolder,
		CurrencyUID:        currencyUID,
		Amount:             s.Amount,
		IdempotencyKey:     idemKey,
		ChannelName:        onchainChannelName,
		// Confirmations is deliberately excluded (design doc §3): it varies
		// across observations of the same transfer and CreateBooking's
		// idempotency check compares Metadata, so including it would make
		// the watcher and webhook paths derive divergent "payloads" for the
		// same sighting and spuriously ErrConflict.
		//
		// block_number IS included, unlike Confirmations -- the recheck loop
		// (recheckOneDeposit/recheckOneConfirmedDeposit below) needs it back
		// from the booking to recompute confirmations without re-scanning
		// the chain, so it must be persisted somewhere reachable even for a
		// booking that never advances past "pending" (e.g. a crash between
		// this call and the pending->confirming transition below). It is
		// also observation-variant (a reorg can re-mine the same tx into a
		// different block between two observations of the identical
		// idempotency key), so postgres's ensureBookingMatchesInput
		// (postgres/idempotency_match.go's bookingMetadataMatches)
		// deliberately excludes this one key from its equality check --
		// everything else here is still compared exactly.
		Metadata: map[string]string{
			"chain_id":     strconv.FormatInt(s.ChainID, 10),
			"tx_hash":      s.TxHash,
			"txlog_seq":    strconv.FormatInt(int64(s.TxLogSeq), 10),
			"token":        tokenKey,
			"block_number": strconv.FormatInt(s.BlockNumber, 10),
		},
	})
	if err != nil {
		if errors.Is(err, core.ErrConflict) {
			if dlErr := o.deps.DeadLetters.RecordDeadLetter(ctx, s, idemKey, err.Error()); dlErr != nil {
				o.log().Error("service: onchain: ingest deposit: record dead letter failed", "idempotency_key", idemKey, "error", dlErr)
			}
		}
		return nil, fmt.Errorf("service: onchain: ingest deposit: create booking: %w", err)
	}

	return o.advanceConfirmation(ctx, booking, s.Confirmations, depositChannelRef(s.TxHash, s.TxLogSeq), cfg)
}

func depositIdempotencyKey(chainID int64, txHash string, txLogSeq int32) string {
	return fmt.Sprintf("deposit-%d-%s-%d", chainID, txHash, txLogSeq)
}

// depositChannelRef qualifies a tx hash with its txlog_seq before using it as
// a booking's ChannelRef. bookings has a UNIQUE (channel_name, channel_ref)
// index (migration 012) -- a single transaction can carry more than one
// Transfer log crediting our registered addresses, and each becomes its own
// booking (see depositIdempotencyKey), so the raw tx hash alone is not
// unique enough to be the shared "onchain" channel's ChannelRef.
func depositChannelRef(txHash string, txLogSeq int32) string {
	return fmt.Sprintf("%s#%d", txHash, txLogSeq)
}

// depositTransitionKey derives Booker.Transition's mandatory idempotency key
// (I-3) for a step in the deposit lifecycle (pending -> confirming ->
// confirmed|failed|review; review -> confirmed|failed -- presets/deposit.go).
// DepositLifecycle has no cycles: every status is reached at most once per
// booking, ever. That makes the booking's own identity enough to key on --
// it is itself derived deterministically from the triggering deposit
// sighting (depositIdempotencyKey, via CreateBooking's idempotent create),
// so two observations of the same on-chain event always resolve to the same
// booking and therefore the same key here, while two different bookings
// (necessarily different deposits) never collide. Unlike the sweep saga
// below, there is no revisit to guard against, so (booking, to_status) alone
// is a safe key -- see TransitionInput.IdempotencyKey's doc comment for why
// that shortcut is NOT safe in general.
func depositTransitionKey(bookingUID string, toStatus core.Status) string {
	return fmt.Sprintf("deposit-%s-%s", toStatus, bookingUID)
}

// advanceConfirmation pushes booking through pending -> confirming ->
// confirmed|review as confirmations allows, sharing logic between
// IngestDeposit's first-seen call and the pending/confirming recheck loop's
// re-invocation. channelRef is the booking's ChannelRef value
// (depositChannelRef's output), not a raw tx hash. "review" is included
// alongside the terminal statuses in the early no-op switch below
// deliberately (design doc §9.1): only a human calling ApproveReview /
// RejectReview may move a booking out of review, never this function --
// e.g. a webhook/watcher re-observing an already-reviewed sighting must be a
// no-op here, not re-evaluate the gate.
func (o *Onchain) advanceConfirmation(ctx context.Context, booking *core.Booking, confirmations int32, channelRef string, cfg core.ChainConfig) (*core.Booking, error) {
	switch booking.Status {
	case "confirmed", "failed", "expired", "review":
		return booking, nil
	case "pending":
		if _, err := o.deps.Booker.Transition(ctx, core.TransitionInput{
			BookingUID:     booking.UID,
			ToStatus:       "confirming",
			ChannelRef:     channelRef,
			Source:         onchainSource,
			IdempotencyKey: depositTransitionKey(booking.UID, "confirming"),
		}); err != nil {
			return nil, fmt.Errorf("advance to confirming: %w", err)
		}
		booking.Status = "confirming"
		fallthrough
	case "confirming":
		if confirmations < cfg.Confirmations {
			return booking, nil
		}

		// M3 compensating controls (design doc §9): before ever posting the
		// journal that credits this deposit, check whether it must instead be
		// parked for human review. tc's zero value (token config not found,
		// or configured with zero ceilings) disables both gates -- pre-M3
		// behavior.
		tc := cfg.CreditTokens[booking.Metadata["token"]]
		reason, err := o.reviewGate(ctx, booking, tc)
		if err != nil {
			return nil, fmt.Errorf("advance to confirmed: review gate: %w", err)
		}
		if reason != "" {
			return o.routeToReview(ctx, booking, channelRef, reason)
		}

		refreshed, err := o.postDepositConfirmedJournal(ctx, booking, channelRef, "")
		if err != nil {
			return nil, fmt.Errorf("advance to confirmed: %w", err)
		}
		return refreshed, nil
	default:
		return booking, nil
	}
}

// Review reasons recorded on a deposit booking's review-transition metadata
// and on the emitted deposit.review_required signal (design doc §9.1/§9.4).
const (
	reviewReasonOverCeiling       = "over_ceiling"
	reviewReasonReconcileMismatch = "reconcile_mismatch"
	// reviewReasonReconcileUnavailable is recorded when the second-source
	// reconciliation RPC (OnchainDeps.DepositConfirmer) has failed
	// TokenConfig.ReconcileFailureLimit consecutive times for this booking
	// (mi5 fix, docs/bugs/2026-07-11-m3-security-review.md): rather than
	// retrying forever and leaving a legitimate deposit silently stuck in
	// "confirming" (working-agreements §3), it is parked for human review,
	// same as the other review reasons.
	reviewReasonReconcileUnavailable = "reconcile_unavailable"
	// reviewReasonMetaKey is the TransitionInput.Metadata key routeToReview
	// records reviewReasonOverCeiling/reviewReasonReconcileMismatch/
	// reviewReasonReconcileUnavailable under.
	reviewReasonMetaKey = "review_reason"
	// rejectReasonMetaKey is the TransitionInput.Metadata key RejectReview
	// records its caller-supplied reason under.
	rejectReasonMetaKey = "reject_reason"
	// approvedByMetaKey / rejectedByMetaKey record the caller-supplied actor
	// identity (design doc §9.4 addendum, MJ2: approve/reject is the
	// highest-privilege action on the deposit path -- it directly triggers
	// minting -- and must be attributable). ApproveReview/RejectReview leave
	// these unset (no key written) when actor is "" (no authenticated
	// identity available), rather than writing an empty string -- see each
	// method's doc comment.
	approvedByMetaKey = "approved_by"
	rejectedByMetaKey = "rejected_by"
)

// reviewGate decides whether a confirming deposit that has reached its
// confirmation threshold may proceed straight to confirmed, or must be
// routed to human review first (design doc §9: RPC is the deposit path's
// single trusted oracle, and threshold-gate + reconciliation are its only
// compensating controls). Returns "" when the booking may proceed, or one of
// reviewReasonOverCeiling / reviewReasonReconcileMismatch /
// reviewReasonReconcileUnavailable when it must not.
//
// The reconciliation RPC call (DepositConfirmer.ConfirmDeposit) happens
// here, deliberately outside any DB transaction -- golang.md forbids
// external calls inside a tx, and this always runs before
// postDepositConfirmedJournal ever opens TxComposer.RunInTx.
//
// mi5 fix: a ConfirmDeposit error below tc.ReconcileFailureLimit still
// returns (reason="", err!=nil), same as before -- advanceConfirmation
// surfaces it, the caller logs it, and the next recheck tick retries
// (fail-closed, no journal posted). Only once the booking's consecutive
// failure count reaches the limit does this method return
// (reviewReasonReconcileUnavailable, nil) instead, so the booking is routed
// to review rather than retried forever.
func (o *Onchain) reviewGate(ctx context.Context, booking *core.Booking, tc core.TokenConfig) (reason string, err error) {
	if tc.AutoCreditCeiling.IsPositive() && booking.Amount.GreaterThan(tc.AutoCreditCeiling) {
		return reviewReasonOverCeiling, nil
	}
	if o.deps.DepositConfirmer == nil || !tc.ReconcileCeiling.IsPositive() || booking.Amount.LessThanOrEqual(tc.ReconcileCeiling) {
		return "", nil
	}

	chainID, txHash, txLogSeq, _, ok := parseDepositMeta(booking.Metadata)
	if !ok {
		return "", fmt.Errorf("reconcile: booking %s missing identity metadata", booking.UID)
	}
	amount, included, err := o.deps.DepositConfirmer.ConfirmDeposit(ctx, chainID, txHash, txLogSeq)
	if err != nil {
		if o.recordReconcileFailure(booking.UID, tc.ReconcileFailureLimit) {
			return reviewReasonReconcileUnavailable, nil
		}
		return "", fmt.Errorf("reconcile: %w", err)
	}
	o.clearReconcileFailure(booking.UID)
	if !included || !amount.Equal(booking.Amount) {
		return reviewReasonReconcileMismatch, nil
	}
	return "", nil
}

// recordReconcileFailure increments booking uid's consecutive
// ConfirmDeposit-error count and reports whether it has now reached limit
// (mi5 fix). validateReconcileFailureLimits requires limit to be positive
// whenever this is reachable through a validated composition root
// (DepositConfirmer configured and ReconcileCeiling positive for this
// token) -- see that method's doc comment. limit<=0 here means a caller
// invoked IngestDeposit/the recheck loop directly without going through
// that gate (e.g. a test, or a push-only consumer that skips Run()): treat
// it the same as "escalation not configured" and never escalate, rather
// than tripping on the very first failure -- the unconfigured case is a
// startup-time error, not a runtime one. Reaching the limit clears the
// count, since the booking is about to leave "confirming" for "review" and
// reviewGate will never be called for it again (advanceConfirmation no-ops
// on "review").
func (o *Onchain) recordReconcileFailure(bookingUID string, limit int32) bool {
	if limit <= 0 {
		return false
	}
	o.reconcileMu.Lock()
	defer o.reconcileMu.Unlock()
	o.reconcileFailures[bookingUID]++
	if o.reconcileFailures[bookingUID] >= limit {
		delete(o.reconcileFailures, bookingUID)
		return true
	}
	return false
}

// clearReconcileFailure resets booking uid's consecutive-failure count
// (mi5 fix) -- called on every successful ConfirmDeposit call, since the
// escalation threshold counts *consecutive* failures, not failures overall.
func (o *Onchain) clearReconcileFailure(bookingUID string) {
	o.reconcileMu.Lock()
	defer o.reconcileMu.Unlock()
	delete(o.reconcileFailures, bookingUID)
}

// routeToReview transitions a confirming deposit to review instead of
// confirmed (design doc §9.1) -- no journal is posted; the booking's
// account_holder balance does not move (I-21) until a human calls
// ApproveReview or RejectReview.
func (o *Onchain) routeToReview(ctx context.Context, booking *core.Booking, channelRef, reason string) (*core.Booking, error) {
	if _, err := o.deps.Booker.Transition(ctx, core.TransitionInput{
		BookingUID:     booking.UID,
		ToStatus:       "review",
		ChannelRef:     channelRef,
		Source:         onchainSource,
		Metadata:       map[string]string{reviewReasonMetaKey: reason},
		IdempotencyKey: depositTransitionKey(booking.UID, "review"),
	}); err != nil {
		return nil, fmt.Errorf("route to review: %w", err)
	}

	chainID, _, _, _, _ := parseDepositMeta(booking.Metadata)
	o.log().Warn("service: onchain: deposit.review_required", "booking_uid", booking.UID, "reason", reason, "amount", booking.Amount.String(), "chain_id", chainID)
	o.metrics().DepositReviewRequired(chainID, reason)

	refreshed, err := o.deps.BookingReader.GetBooking(ctx, booking.UID)
	if err != nil {
		return nil, fmt.Errorf("route to review: reload booking: %w", err)
	}
	return refreshed, nil
}

// postDepositConfirmedJournal transitions booking to confirmed and posts its
// deposit_confirm journal atomically, cross-linked via EventUID (design doc
// §3) -- shared by advanceConfirmation's normal confirming->confirmed path
// (actor "": system/watcher-driven, nobody to attribute) and ApproveReview's
// review->confirmed path (design doc §9.4), so the two can never diverge on
// how a deposit's journal gets posted.
//
// actor is the human/API-key identity that triggered this posting (MJ2:
// approve is the highest-privilege action on the deposit path). "" means
// "no actor to record" (the normal automatic path) -- in that case
// approvedByMetaKey is left off the transition/journal metadata entirely,
// rather than written as an empty string.
//
// Signing (design doc §7.5): this is P5's headline use case (M5: forged
// deposit accounting is exactly what per-journal signing exists to
// defeat), and it is composed via RunInTx -- so it is exactly the path
// that always posted unsigned before this fix, regardless of whether an
// Attestor was configured. AuthorizeTemplate runs BEFORE RunInTx opens
// (the only safe point to call an Attestor); the result is posted via
// PostAuthorized instead of ExecuteTemplate, which never calls the
// Attestor and is therefore safe from inside the transaction.
//
// EventUID: the event this journal links to is minted by booker.Transition
// INSIDE the transaction below, so it cannot be known at AuthorizeTemplate
// time -- but CanonicalJournalDigest never covers EventUID (Team Lead's
// 2026-08-21 ruling, board #12/#13: it is provenance metadata, not posting
// intent, and an earlier draft of this fix that signed with EventUID=""
// would have made VerifyJournalAuth spuriously reject every legitimately
// signed, event-linked journal). authorized.Input.EventUID is set to the
// real event uid just before PostAuthorized purely for the FK link; doing
// so never invalidates authorized.Digest/Signature. The event/journal
// link remains a DB-structural guarantee (I-10 + 045's set-once FK), never
// a cryptographic one -- signing could not add atomicity to it anyway.
func (o *Onchain) postDepositConfirmedJournal(ctx context.Context, booking *core.Booking, channelRef, actor string) (*core.Booking, error) {
	var transitionMeta, journalMeta map[string]string
	if actor != "" {
		transitionMeta = map[string]string{approvedByMetaKey: actor}
		journalMeta = map[string]string{approvedByMetaKey: actor}
	}

	authorized, err := o.deps.TxComposer.AuthorizeTemplate(ctx, depositConfirmTemplate, core.TemplateParams{
		HolderID:       booking.AccountHolder,
		CurrencyUID:    booking.CurrencyUID,
		IdempotencyKey: "deposit-confirm-" + booking.UID,
		Amounts:        map[string]decimal.Decimal{"amount": booking.Amount},
		Source:         onchainSource,
		Metadata:       journalMeta,
	})
	if err != nil {
		return nil, fmt.Errorf("authorize deposit confirm journal: %w", err)
	}

	err = o.deps.TxComposer.RunInTx(ctx, func(ctx context.Context, booker core.Booker, journals core.JournalWriter) error {
		evt, err := booker.Transition(ctx, core.TransitionInput{
			BookingUID:     booking.UID,
			ToStatus:       "confirmed",
			ChannelRef:     channelRef,
			Amount:         booking.Amount,
			Source:         onchainSource,
			Metadata:       transitionMeta,
			IdempotencyKey: depositTransitionKey(booking.UID, "confirmed"),
		})
		if err != nil {
			return err
		}
		authorized.Input.EventUID = evt.UID
		_, err = journals.PostAuthorized(ctx, authorized)
		return err
	})
	if err != nil {
		return nil, err
	}
	// Re-fetch: the RunInTx callback posted a journal and backfilled
	// bookings.journal_id atomically (design doc: EventUID cross-link) --
	// the in-memory booking passed into this function predates that write,
	// so its JournalUID/Status would otherwise be reported stale.
	refreshed, err := o.deps.BookingReader.GetBooking(ctx, booking.UID)
	if err != nil {
		return nil, fmt.Errorf("reload booking: %w", err)
	}
	return refreshed, nil
}

// requireDepositBooking loads bookingUID and verifies it is actually a
// deposit booking (mi1, design doc §9.4 addendum): ApproveReview/RejectReview
// only make sense against the deposit lifecycle's "review" status, and
// today no other classification ever reaches "review" -- but that is a
// coincidence of the currently-installed presets, not something the
// classification's own lifecycle enforces. Without this check, a future
// classification that reuses the "review" status name would let a
// ScopeWrite caller drive it to "confirmed" through the deposit
// approve/reject endpoints, forcing it through depositConfirmTemplate.
func (o *Onchain) requireDepositBooking(ctx context.Context, bookingUID string) (*core.Booking, error) {
	booking, err := o.deps.BookingReader.GetBooking(ctx, bookingUID)
	if err != nil {
		return nil, err
	}
	depositUID, err := o.classes.resolve(ctx, o.deps.Classifications, presets.DepositClassificationCode)
	if err != nil {
		return nil, fmt.Errorf("resolve deposit classification: %w", err)
	}
	if booking.ClassificationUID != depositUID {
		return nil, fmt.Errorf("booking %s is not a deposit booking: %w", bookingUID, core.ErrConflict)
	}
	return booking, nil
}

// ApproveReview approves a deposit booking parked in review (design doc
// §9.4), posting its deposit_confirm journal via the same
// postDepositConfirmedJournal path advanceConfirmation's normal
// confirming->confirmed transition uses (EventUID cross-link, I-21).
//
// actor identifies who approved this (MJ2: audit attribution for the
// deposit path's highest-privilege action) -- typically the authenticated
// API key's name (server/handler_deposit_reviews.go). "" records no actor
// (see postDepositConfirmedJournal's doc comment).
//
// Idempotent: calling it again once the booking is already confirmed
// (i.e. a prior ApproveReview call already succeeded) is a no-op returning
// the current booking. Calling it on any other non-review status is
// core.ErrConflict.
func (o *Onchain) ApproveReview(ctx context.Context, bookingUID, actor string) (*core.Booking, error) {
	if err := o.deps.validateCore(); err != nil {
		return nil, err
	}
	booking, err := o.requireDepositBooking(ctx, bookingUID)
	if err != nil {
		return nil, fmt.Errorf("service: onchain: approve review: %w", err)
	}
	switch booking.Status {
	case "confirmed":
		return booking, nil
	case "review":
		// proceed below
	default:
		return nil, fmt.Errorf("service: onchain: approve review: booking %s is %s, not review: %w", bookingUID, booking.Status, core.ErrConflict)
	}

	refreshed, err := o.postDepositConfirmedJournal(ctx, booking, booking.ChannelRef, actor)
	if err != nil {
		return nil, fmt.Errorf("service: onchain: approve review: %w", err)
	}
	o.log().Warn("service: onchain: deposit.review_approved", "booking_uid", booking.UID, "amount", booking.Amount.String(), "actor", actor)
	return refreshed, nil
}

// RejectReview rejects a deposit booking parked in review, transitioning it
// to failed with no journal ever posted (I-21) -- the deposit is never
// credited. reason is recorded verbatim on the transition's metadata (design
// doc §9.4: "脱敏入事件" -- callers are responsible for sanitizing reason
// before it reaches here, since it flows into the booking's audit trail and
// emitted event). actor identifies who rejected this (MJ2, same attribution
// as ApproveReview); "" records no actor.
//
// Idempotent: calling it again once the booking is already failed (i.e. a
// prior RejectReview call already succeeded) is a no-op returning the
// current booking. Calling it on any other non-review status is
// core.ErrConflict.
func (o *Onchain) RejectReview(ctx context.Context, bookingUID, actor, reason string) (*core.Booking, error) {
	if err := o.deps.validateCore(); err != nil {
		return nil, err
	}
	booking, err := o.requireDepositBooking(ctx, bookingUID)
	if err != nil {
		return nil, fmt.Errorf("service: onchain: reject review: %w", err)
	}
	switch booking.Status {
	case "failed":
		return booking, nil
	case "review":
		// proceed below
	default:
		return nil, fmt.Errorf("service: onchain: reject review: booking %s is %s, not review: %w", bookingUID, booking.Status, core.ErrConflict)
	}

	rejectMeta := map[string]string{rejectReasonMetaKey: reason}
	if actor != "" {
		rejectMeta[rejectedByMetaKey] = actor
	}
	if _, err := o.deps.Booker.Transition(ctx, core.TransitionInput{
		BookingUID:     booking.UID,
		ToStatus:       "failed",
		ChannelRef:     booking.ChannelRef,
		Source:         onchainSource,
		Metadata:       rejectMeta,
		IdempotencyKey: depositTransitionKey(booking.UID, "failed"),
	}); err != nil {
		return nil, fmt.Errorf("service: onchain: reject review: %w", err)
	}
	o.log().Warn("service: onchain: deposit.review_rejected", "booking_uid", booking.UID, "reason", reason, "actor", actor)

	refreshed, err := o.deps.BookingReader.GetBooking(ctx, booking.UID)
	if err != nil {
		return nil, fmt.Errorf("service: onchain: reject review: reload booking: %w", err)
	}
	return refreshed, nil
}

// ListReviews lists deposit bookings currently parked in review status
// (design doc §9.4: the `review` status is itself the review queue -- no
// dedicated table), oldest first, cursor-paginated. Backs the HTTP review
// queue endpoint (server.DepositReviewer).
func (o *Onchain) ListReviews(ctx context.Context, cursor string, limit int32) ([]core.Booking, string, error) {
	if err := o.deps.validateCore(); err != nil {
		return nil, "", err
	}
	depositUID, err := o.classes.resolve(ctx, o.deps.Classifications, presets.DepositClassificationCode)
	if err != nil {
		return nil, "", fmt.Errorf("service: onchain: list reviews: %w", err)
	}
	bookings, next, err := o.deps.BookingReader.ListBookings(ctx, core.BookingFilter{
		ClassificationUID: depositUID,
		Status:            "review",
		Cursor:            cursor,
		Limit:             int(limit),
	})
	if err != nil {
		return nil, "", fmt.Errorf("service: onchain: list reviews: %w", err)
	}
	return bookings, next, nil
}

// parseDepositMeta extracts the stable identity fields IngestDeposit records
// on every deposit booking's metadata. ok is false if any field is missing
// or malformed -- callers should log and skip rather than fail a whole
// recheck batch over one bad row.
func parseDepositMeta(m map[string]string) (chainID int64, txHash string, txLogSeq int32, blockNumber int64, ok bool) {
	chainIDStr, hasChain := m["chain_id"]
	txHash, hasTx := m["tx_hash"]
	txLogSeqStr, hasSeq := m["txlog_seq"]
	blockStr, hasBlock := m["block_number"]
	if !hasChain || !hasTx || !hasSeq || !hasBlock || txHash == "" {
		return 0, "", 0, 0, false
	}
	chainID, err1 := strconv.ParseInt(chainIDStr, 10, 64)
	txLogSeq64, err2 := strconv.ParseInt(txLogSeqStr, 10, 32)
	blockNumber, err3 := strconv.ParseInt(blockStr, 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, "", 0, 0, false
	}
	return chainID, txHash, int32(txLogSeq64), blockNumber, true
}

// blockCache memoizes ChainReader.LatestBlock within a single recheck tick,
// so a batch touching many bookings on the same chain issues one RPC call
// per chain instead of one per booking.
type blockCache struct {
	mu   sync.Mutex
	vals map[int64]int64
}

func newBlockCache() *blockCache { return &blockCache{vals: make(map[int64]int64)} }

func (o *Onchain) latestBlock(ctx context.Context, c *blockCache, chainID int64) (int64, error) {
	c.mu.Lock()
	if v, ok := c.vals[chainID]; ok {
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()

	v, err := o.deps.Reader.LatestBlock(ctx, chainID)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	c.vals[chainID] = v
	c.mu.Unlock()
	return v, nil
}

// --- watcher: forward scan ---

func (o *Onchain) scanChainOnce(ctx context.Context, chainID int64) error {
	cfg := o.chains[chainID]

	cursor, err := o.deps.Cursors.GetCursor(ctx, chainID)
	from := int64(0)
	switch {
	case err == nil:
		from = cursor.LastScannedBlock + 1
	case errors.Is(err, core.ErrNotFound):
		// Never scanned: start from genesis. New chains are expected to be
		// configured with cursors seeded out-of-band if genesis scanning is
		// infeasible; this library has no opinion on that.
	default:
		return fmt.Errorf("get cursor: %w", err)
	}

	latest, err := o.deps.Reader.LatestBlock(ctx, chainID)
	if err != nil {
		return fmt.Errorf("latest block: %w", err)
	}
	if latest < from {
		o.metrics().ChainCursorLag(chainID, 0)
		return nil
	}
	to := latest
	if to-from+1 > o.maxBlocksPerScan {
		to = from + o.maxBlocksPerScan - 1
	}

	addrRows, err := o.deps.Registry.ListAddresses(ctx)
	if err != nil {
		return fmt.Errorf("list addresses: %w", err)
	}
	if len(addrRows) == 0 {
		if err := o.deps.Cursors.SetCursor(ctx, chainID, to); err != nil {
			return fmt.Errorf("set cursor: %w", err)
		}
		o.metrics().ChainCursorLag(chainID, latest-to)
		return nil
	}
	addrs := make([]string, len(addrRows))
	for i, a := range addrRows {
		addrs[i] = a.Address
	}

	sightings, err := o.deps.Reader.FetchDeposits(ctx, chainID, from, to, addrs)
	if err != nil {
		return fmt.Errorf("fetch deposits: %w", err)
	}
	for _, s := range sightings {
		if _, err := o.IngestDeposit(ctx, s); err != nil {
			o.log().Error("service: onchain: watcher: ingest failed", "chain_id", chainID, "tx_hash", s.TxHash, "error", err)
		}
	}

	if err := o.deps.Cursors.SetCursor(ctx, chainID, to); err != nil {
		return fmt.Errorf("set cursor: %w", err)
	}
	_ = cfg // cfg reserved for future per-chain scan tuning
	o.metrics().ChainCursorLag(chainID, latest-to)
	return nil
}

// --- pending/confirming recheck + shallow reorg ---

// RunPendingRecheckOnce runs a single pending/confirming recheck pass outside
// Run's ticker loop -- mirrors RunSweepOnce, useful for an ops-triggered
// manual recheck and for tests that need to exercise the recheck path
// directly against a fake ChainReader without waiting on recheckInterval.
func (o *Onchain) RunPendingRecheckOnce(ctx context.Context) {
	o.recheckPendingDeposits(ctx)
}

func (o *Onchain) recheckPendingDeposits(ctx context.Context) {
	depositUID, err := o.classes.resolve(ctx, o.deps.Classifications, presets.DepositClassificationCode)
	if err != nil {
		o.log().Error("service: onchain: recheck: resolve deposit classification failed", "error", err)
		return
	}
	cache := newBlockCache()
	for _, status := range []core.Status{"pending", "confirming"} {
		cursor := ""
		for {
			bookings, next, err := o.deps.BookingReader.ListBookings(ctx, core.BookingFilter{
				ClassificationUID: depositUID,
				Status:            string(status),
				Cursor:            cursor,
				Limit:             200,
			})
			if err != nil {
				o.log().Error("service: onchain: recheck: list bookings failed", "status", status, "error", err)
				break
			}
			for i := range bookings {
				o.recheckOneDeposit(ctx, cache, &bookings[i])
			}
			if next == "" {
				break
			}
			cursor = next
		}
	}
}

func (o *Onchain) recheckOneDeposit(ctx context.Context, cache *blockCache, b *core.Booking) {
	chainID, txHash, txLogSeq, blockNumber, ok := parseDepositMeta(b.Metadata)
	if !ok {
		o.log().Warn("service: onchain: recheck: deposit booking missing identity metadata, skipping", "booking_uid", b.UID)
		return
	}
	cfg, ok := o.chains[chainID]
	if !ok {
		return
	}
	latest, err := o.latestBlock(ctx, cache, chainID)
	if err != nil {
		o.log().Error("service: onchain: recheck: latest block failed", "chain_id", chainID, "error", err)
		return
	}
	confirmations := latest - blockNumber + 1
	if confirmations < 0 {
		confirmations = 0
	}

	if int32(confirmations) < cfg.Confirmations {
		included, err := o.deps.Reader.TxIncluded(ctx, chainID, txHash)
		if err != nil {
			o.log().Error("service: onchain: recheck: tx included check failed", "chain_id", chainID, "tx_hash", txHash, "error", err)
			return
		}
		if !included {
			// Shallow reorg: the tx vanished before reaching the confirmation
			// threshold. No journal was ever posted for this booking, so a
			// plain failed transition is sufficient (design doc §6).
			if _, err := o.deps.Booker.Transition(ctx, core.TransitionInput{
				BookingUID:     b.UID,
				ToStatus:       "failed",
				ChannelRef:     depositChannelRef(txHash, txLogSeq),
				Source:         onchainSource,
				IdempotencyKey: depositTransitionKey(b.UID, "failed"),
			}); err != nil {
				o.log().Error("service: onchain: recheck: transition to failed failed", "booking_uid", b.UID, "error", err)
			}
		}
		return
	}

	if _, err := o.advanceConfirmation(ctx, b, int32(confirmations), depositChannelRef(txHash, txLogSeq), cfg); err != nil {
		o.log().Error("service: onchain: recheck: advance confirmation failed", "booking_uid", b.UID, "error", err)
	}
}

// --- confirmed deposit deep-reorg recheck ---

func (o *Onchain) recheckConfirmedDeposits(ctx context.Context) {
	depositUID, err := o.classes.resolve(ctx, o.deps.Classifications, presets.DepositClassificationCode)
	if err != nil {
		o.log().Error("service: onchain: reorg recheck: resolve deposit classification failed", "error", err)
		return
	}
	cache := newBlockCache()
	cursor := ""
	for {
		bookings, next, err := o.deps.BookingReader.ListBookings(ctx, core.BookingFilter{
			ClassificationUID: depositUID,
			Status:            "confirmed",
			Cursor:            cursor,
			Limit:             200,
		})
		if err != nil {
			o.log().Error("service: onchain: reorg recheck: list bookings failed", "error", err)
			return
		}
		for i := range bookings {
			o.recheckOneConfirmedDeposit(ctx, cache, &bookings[i])
		}
		if next == "" {
			return
		}
		cursor = next
	}
}

func (o *Onchain) recheckOneConfirmedDeposit(ctx context.Context, cache *blockCache, b *core.Booking) {
	chainID, txHash, _, blockNumber, ok := parseDepositMeta(b.Metadata)
	if !ok {
		return
	}
	latest, err := o.latestBlock(ctx, cache, chainID)
	if err != nil {
		o.log().Error("service: onchain: reorg recheck: latest block failed", "chain_id", chainID, "error", err)
		return
	}
	if latest-blockNumber > o.reorgRecheckWindow {
		return // old enough to be treated as final; bounds recheck cost
	}
	included, err := o.deps.Reader.TxIncluded(ctx, chainID, txHash)
	if err != nil {
		o.log().Error("service: onchain: reorg recheck: tx included check failed", "chain_id", chainID, "tx_hash", txHash, "error", err)
		return
	}
	if included {
		return
	}
	o.handleReorg(ctx, b, chainID)
}

func (o *Onchain) handleReorg(ctx context.Context, b *core.Booking, chainID int64) {
	o.log().Warn("service: onchain: deep reorg detected", "booking_uid", b.UID, "channel_ref", b.ChannelRef, "chain_id", chainID, "policy", o.reorgPolicy)
	o.metrics().DepositReorgDetected(chainID)

	if o.reorgPolicy != core.ReorgPolicyAutoReverse {
		// Manual (default): alert only, on-call reverses via RUNBOOK.
		return
	}
	if b.JournalUID == "" {
		o.log().Error("service: onchain: auto-reverse: confirmed booking has no journal_uid, cannot reverse", "booking_uid", b.UID)
		return
	}
	reason := fmt.Sprintf("chain reorg auto-reverse: booking %s tx %s no longer included on chain %d", b.UID, b.ChannelRef, chainID)
	if _, err := o.deps.Journals.ReverseJournal(ctx, b.JournalUID, reason); err != nil {
		if errors.Is(err, core.ErrConflict) {
			// Already reversed by a prior tick -- ReverseJournal's own
			// idempotency guard is the dedupe mechanism here, since a
			// terminal ("confirmed") booking's status/metadata cannot be
			// updated to mark "already handled" (Lifecycle forbids any
			// transition out of a terminal state, even to itself).
			return
		}
		o.log().Error("service: onchain: auto-reverse: reverse journal failed", "booking_uid", b.UID, "journal_uid", b.JournalUID, "error", err)
		return
	}
	o.log().Warn("service: onchain: auto-reverse: reversal journal posted", "booking_uid", b.UID, "journal_uid", b.JournalUID)
}

// --- sweep ---

// RunSweepOnce runs a single sweep evaluation for policy outside of Run's
// ticker loop -- useful for an ops-triggered manual sweep and for tests. Does
// not take the advisory lock Run's per-policy loop wraps this in; callers
// invoking it alongside a running Run should be aware of that (design doc §4
// single-flight is a Run-loop property, not enforced here).
func (o *Onchain) RunSweepOnce(ctx context.Context, policy core.SweepPolicy) error {
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("service: onchain: run sweep once: %w", err)
	}
	return o.sweepTick(ctx, policy)
}

func (o *Onchain) sweepTick(ctx context.Context, policy core.SweepPolicy) error {
	if o.deps.Scanner == nil || o.deps.Sweeper == nil {
		return fmt.Errorf("sweep: scanner/sweeper not configured: %w", core.ErrInvalidInput)
	}
	cfg, ok := o.chains[policy.ChainID]
	if !ok {
		return fmt.Errorf("sweep: chain %d not configured: %w", policy.ChainID, core.ErrInvalidInput)
	}

	gasPrice, err := o.deps.Sweeper.GasPrice(ctx, policy.ChainID)
	if err != nil {
		return fmt.Errorf("sweep: gas price: %w", err)
	}
	if gasPrice.GreaterThan(policy.GasCeiling) {
		o.log().Info("service: onchain: sweep: gas price above ceiling, skipping round", "chain_id", policy.ChainID, "token", policy.Token, "gas_price", gasPrice.String(), "ceiling", policy.GasCeiling.String())
		return nil
	}

	inFlight, err := o.findInFlightSweep(ctx, policy.ChainID, policy.Token)
	if err != nil {
		return err
	}
	if inFlight != nil {
		return o.advanceSweep(ctx, inFlight, policy)
	}

	// M5 hardening: a booking that exhausted its gas-bump retries and
	// transitioned to terminal "failed" (advanceSweep/recheckSweepSent
	// below) is invisible to findInFlightSweep (it only looks at
	// pending/sent) -- without this lookup, the code below would try to
	// CreateBooking a fresh sweep whose idempotency key collides with the
	// failed booking's whenever NextNonce still reports the same nonce
	// (the common case: the failed tx's nonce slot is never actually freed
	// by anything we do), silently returning that same terminal "failed"
	// booking forever and permanently stalling this (chain,token)'s
	// collection. See reviveFailedSweep.
	failed, err := o.findFailedSweep(ctx, policy.ChainID, policy.Token)
	if err != nil {
		return err
	}

	addrRows, err := o.deps.Registry.ListAddresses(ctx)
	if err != nil {
		return fmt.Errorf("sweep: list addresses: %w", err)
	}
	addrs := make([]string, len(addrRows))
	for i, a := range addrRows {
		addrs[i] = a.Address
	}
	if len(addrs) == 0 {
		return nil
	}

	balances, err := o.deps.Scanner.ScanBalances(ctx, policy.ChainID, policy.Token, addrs)
	if err != nil {
		return fmt.Errorf("sweep: scan balances: %w", err)
	}

	eligible := make([]string, 0, len(balances))
	for addr, bal := range balances {
		if bal.GreaterThanOrEqual(policy.MinThreshold) {
			eligible = append(eligible, addr)
		}
	}
	if len(eligible) == 0 {
		return nil
	}
	sort.Strings(eligible) // deterministic batch composition
	if int32(len(eligible)) > policy.BatchLimit {
		eligible = eligible[:policy.BatchLimit]
	}
	total := decimal.Zero
	for _, addr := range eligible {
		total = total.Add(balances[addr])
	}

	nonce, err := o.deps.Sweeper.NextNonce(ctx, policy.ChainID)
	if err != nil {
		return fmt.Errorf("sweep: next nonce: %w", err)
	}

	currencyUID, unattributed, err := o.resolveSweepCurrency(ctx, cfg, policy.Token)
	if err != nil {
		return err
	}

	if unattributed {
		o.log().Warn("service: onchain: sweep.unattributed: collecting token with no ledger attribution", "chain_id", policy.ChainID, "token", policy.Token, "amount", total.String(), "address_count", len(eligible))
		o.metrics().SweepUnattributed(policy.ChainID)
	}

	if failed != nil {
		return o.reviveFailedSweep(ctx, failed, nonce, eligible, policy)
	}

	idemKey := fmt.Sprintf("sweep-%d-%s-%d", policy.ChainID, policy.Token, nonce)
	booking, err := o.deps.Booker.CreateBooking(ctx, core.CreateBookingInput{
		ClassificationCode: presets.SweepClassificationCode,
		AccountHolder:      sweepSystemHolder(policy.ChainID),
		CurrencyUID:        currencyUID,
		Amount:             total,
		IdempotencyKey:     idemKey,
		ChannelName:        onchainChannelName,
		Metadata: map[string]string{
			"chain_id":  strconv.FormatInt(policy.ChainID, 10),
			"token":     policy.Token,
			"nonce":     strconv.FormatUint(nonce, 10),
			"addresses": strings.Join(eligible, ","),
		},
	})
	if err != nil {
		return fmt.Errorf("sweep: create booking: %w", err)
	}

	return o.advanceSweep(ctx, booking, policy)
}

// sweepReviveKey, sweepSentKey, sweepConfirmedKey and sweepFailedKey derive
// Booker.Transition's mandatory idempotency key (I-3) for each step of a
// sweep booking's saga. Unlike the deposit lifecycle, SweepLifecycle has a
// real cycle -- failed -> pending -> sent -> confirmed|failed
// (presets/sweep.go: gas-bump exhaustion revives the SAME booking rather
// than minting a new one, reviveFailedSweep's doc comment) -- so the SAME
// booking legitimately revisits "pending", "sent" and "failed" once per
// revival. A key of just (booking, to_status) collides across two
// genuinely different revivals; each helper below folds in the on-chain
// broadcast tx hash that uniquely identifies which cycle a given call
// belongs to.
//
//   - sweepSentKey/sweepConfirmedKey key off the tx hash of THIS call's own
//     broadcast (BatchSweep mints a fresh hash on every call, initial send or
//     gas-bump), so distinct broadcasts always get distinct keys.
//   - sweepFailedKey keys off the booking's persisted ChannelRef, which
//     recheckSweepSent's sent->failed transition deliberately leaves
//     unchanged across in-cycle gas-bumps (see that function's doc comment)
//     but which IS refreshed to a new broadcast hash every time the booking
//     re-enters "sent" via a revival -- so it is stable within one cycle and
//     distinct across cycles, exactly matching how many times "failed" can
//     legitimately be reached.
//   - sweepReviveKey keys off the failed booking's own ChannelRef for the
//     same reason: it identifies which failed cycle is being revived, so two
//     distinct revivals (of two distinct failed cycles) never collide.
func sweepSentKey(bookingUID, txHash string) string {
	return fmt.Sprintf("sweep-sent-%s-%s", bookingUID, txHash)
}

func sweepConfirmedKey(bookingUID, txHash string) string {
	return fmt.Sprintf("sweep-confirmed-%s-%s", bookingUID, txHash)
}

func sweepFailedKey(bookingUID, cycleChannelRef string) string {
	return fmt.Sprintf("sweep-failed-%s-%s", bookingUID, cycleChannelRef)
}

func sweepReviveKey(failedBookingUID, failedCycleChannelRef string) string {
	return fmt.Sprintf("sweep-revive-%s-%s", failedBookingUID, failedCycleChannelRef)
}

// reviveFailedSweep re-drives a sweep booking that exhausted its gas-bump
// retries and was transitioned to terminal "failed" (SweepLifecycle's
// failed->pending retry edge, presets/sweep.go), instead of leaving it
// stuck forever (M5).
//
// Nonce lifecycle: failed's signerNonce was broadcast repeatedly (up to
// maxSweepBumps times, each a fee-bump replacement of the SAME nonce) but
// never observed included on-chain. From the signer EOA's perspective that
// nonce slot is still "next" -- NextNonce (PendingNonceAt) will keep
// reporting it until either the stuck tx eventually lands, or something
// external moves the EOA past it (an on-call operator manually clearing it
// per RUNBOOK, or the node's mempool view of it changing). The nonce
// passed in here is a FRESH NextNonce call made by the caller (sweepTick):
// if the EOA has moved on, it is a genuinely new nonce; if not, it is the
// same stale value and this retry re-attempts with current gas pricing.
//
// The failed booking is reused via Transition rather than a new
// CreateBooking call: its idempotency key was minted against its now-stale
// nonce and can never be safely reissued (a second CreateBooking with a
// colliding key is exactly the ErrConflict/silent-no-op M5 describes), so
// there is deliberately no "new booking key" here to collide with anything
// -- reusing the same booking UID both avoids that collision and keeps one
// audit trail per (chain,token) sweep saga. Metadata (nonce, addresses) is
// refreshed to the just-rescanned values; the booking's original Amount
// column is immutable and is not corrected to the re-scanned total (sweep
// bookings never drive accounting -- design doc §4 -- so this is cosmetic).
func (o *Onchain) reviveFailedSweep(ctx context.Context, failed *core.Booking, nonce uint64, eligible []string, policy core.SweepPolicy) error {
	if _, err := o.deps.Booker.Transition(ctx, core.TransitionInput{
		BookingUID: failed.UID,
		ToStatus:   "pending",
		Source:     onchainSource,
		Metadata: map[string]string{
			"nonce":     strconv.FormatUint(nonce, 10),
			"addresses": strings.Join(eligible, ","),
		},
		IdempotencyKey: sweepReviveKey(failed.UID, failed.ChannelRef),
	}); err != nil {
		return fmt.Errorf("sweep: revive failed booking %s: %w", failed.UID, err)
	}
	revived, err := o.deps.BookingReader.GetBooking(ctx, failed.UID)
	if err != nil {
		return fmt.Errorf("sweep: revive failed booking %s: reload: %w", failed.UID, err)
	}
	return o.advanceSweep(ctx, revived, policy)
}

// resolveSweepCurrency looks up the ledger currency a sweep batch's booking
// is denominated in (every booking needs one, even though sweep never posts
// a journal) and reports whether the token is outside the deposit-side
// CreditTokens allowlist (design doc §4: native/non-whitelisted collection
// has no corresponding user ledger balance).
func (o *Onchain) resolveSweepCurrency(ctx context.Context, cfg core.ChainConfig, token string) (currencyUID string, unattributed bool, err error) {
	tc, ok := cfg.SweepTokens[token]
	if !ok {
		return "", false, fmt.Errorf("sweep: token %q not in chain %d sweep_tokens allowlist: %w", token, cfg.ChainID, core.ErrInvalidInput)
	}
	currencyUID, err = o.currencies.resolve(ctx, o.deps.Currencies, tc.CurrencyCode)
	if err != nil {
		return "", false, fmt.Errorf("sweep: %w", err)
	}
	_, credited := cfg.CreditTokens[token]
	return currencyUID, !credited, nil
}

// findInFlightSweep returns the (chain,token)'s currently in-flight sweep
// booking, if any -- one still being actively driven toward a terminal
// state (as opposed to "failed", which is terminal-but-stuck; see
// findFailedSweep/reviveFailedSweep).
func (o *Onchain) findInFlightSweep(ctx context.Context, chainID int64, token string) (*core.Booking, error) {
	return o.findSweepBookingByStatus(ctx, chainID, token, "pending", "sent")
}

// findFailedSweep returns the (chain,token)'s sweep booking currently
// parked in terminal "failed" (gas-bump retries exhausted without
// on-chain inclusion), if any -- see reviveFailedSweep (M5).
func (o *Onchain) findFailedSweep(ctx context.Context, chainID int64, token string) (*core.Booking, error) {
	return o.findSweepBookingByStatus(ctx, chainID, token, "failed")
}

func (o *Onchain) findSweepBookingByStatus(ctx context.Context, chainID int64, token string, statuses ...core.Status) (*core.Booking, error) {
	sweepUID, err := o.classes.resolve(ctx, o.deps.Classifications, presets.SweepClassificationCode)
	if err != nil {
		return nil, fmt.Errorf("sweep: resolve sweep classification: %w", err)
	}
	chainIDStr := strconv.FormatInt(chainID, 10)
	for _, status := range statuses {
		cursor := ""
		for {
			bookings, next, err := o.deps.BookingReader.ListBookings(ctx, core.BookingFilter{
				ClassificationUID: sweepUID,
				Status:            string(status),
				Cursor:            cursor,
				Limit:             100,
			})
			if err != nil {
				return nil, fmt.Errorf("sweep: list bookings (status=%s): %w", status, err)
			}
			for i := range bookings {
				b := bookings[i]
				if b.Metadata["chain_id"] == chainIDStr && b.Metadata["token"] == token {
					return &b, nil
				}
			}
			if next == "" {
				break
			}
			cursor = next
		}
	}
	return nil, nil
}

// sweepTargets resolves each address to a core.SweepTarget (address +
// account holder) via the registry. The factory's on-chain batchSweep ABI
// takes CREATE2 salts (account holder ids), not addresses -- CREATE2
// derivation is one-way, so chains/evm cannot recover a holder's salt from
// its address alone; this is the point where that gap gets closed, using
// data the registry already has.
func (o *Onchain) sweepTargets(ctx context.Context, addrs []string) ([]core.SweepTarget, error) {
	targets := make([]core.SweepTarget, 0, len(addrs))
	for _, addr := range addrs {
		da, err := o.deps.Registry.GetByAddress(ctx, addr)
		if err != nil {
			return nil, fmt.Errorf("resolve sweep target %s: %w", addr, err)
		}
		targets = append(targets, core.SweepTarget{Address: da.Address, AccountHolder: da.AccountHolder})
	}
	return targets, nil
}

func (o *Onchain) advanceSweep(ctx context.Context, b *core.Booking, policy core.SweepPolicy) error {
	chainID, err := strconv.ParseInt(b.Metadata["chain_id"], 10, 64)
	if err != nil {
		return fmt.Errorf("sweep: booking %s missing chain_id metadata: %w", b.UID, core.ErrInvalidInput)
	}
	token := b.Metadata["token"]
	nonce, err := strconv.ParseUint(b.Metadata["nonce"], 10, 64)
	if err != nil {
		return fmt.Errorf("sweep: booking %s missing nonce metadata: %w", b.UID, core.ErrInvalidInput)
	}
	addrs := strings.Split(b.Metadata["addresses"], ",")

	switch b.Status {
	case "confirmed", "failed":
		return nil
	case "pending":
		targets, err := o.sweepTargets(ctx, addrs)
		if err != nil {
			return fmt.Errorf("sweep: resolve targets: %w", err)
		}
		txHash, err := o.deps.Sweeper.BatchSweep(ctx, chainID, token, targets, nonce)
		if err != nil {
			return fmt.Errorf("sweep: batch sweep: %w", err)
		}
		if _, err := o.deps.Booker.Transition(ctx, core.TransitionInput{
			BookingUID:     b.UID,
			ToStatus:       "sent",
			ChannelRef:     txHash,
			Source:         onchainSource,
			IdempotencyKey: sweepSentKey(b.UID, txHash),
		}); err != nil {
			return fmt.Errorf("sweep: transition sent: %w", err)
		}
		o.trackSweepTx(b.UID, txHash)
		return nil
	case "sent":
		return o.recheckSweepSent(ctx, b, chainID, token, nonce, addrs, policy)
	default:
		return nil
	}
}

// recheckSweepSent polls a broadcast sweep transaction for inclusion, and
// gas-bumps (re-broadcasts with the same nonce) if it has been stuck longer
// than sweepStuckAfter. The latest broadcast tx hash for a gas-bumped
// booking is tracked in-memory only (see sweepTx doc comment) -- the
// booking's persisted ChannelRef reflects only the *first* broadcast
// attempt, since Transition's same-status idempotency guard rejects a
// changed ChannelRef on repeat "sent" calls (by design: it protects against
// silently reassigning a booking's channel reference). This is safe under
// design doc §4's single-deployment constraint for the sweeper key; after a
// process restart the in-memory map is empty and this loop falls back to
// checking the original ChannelRef, at worst issuing one extra harmless
// gas-bump re-broadcast for the same nonce.
func (o *Onchain) recheckSweepSent(ctx context.Context, b *core.Booking, chainID int64, token string, nonce uint64, addrs []string, policy core.SweepPolicy) error {
	txHash := o.currentSweepTx(b.UID)
	if txHash == "" {
		txHash = b.ChannelRef
	}
	included, err := o.deps.Reader.TxIncluded(ctx, chainID, txHash)
	if err != nil {
		return fmt.Errorf("sweep: tx included: %w", err)
	}
	if included {
		if _, err := o.deps.Booker.Transition(ctx, core.TransitionInput{
			BookingUID:     b.UID,
			ToStatus:       "confirmed",
			ChannelRef:     txHash,
			Source:         onchainSource,
			IdempotencyKey: sweepConfirmedKey(b.UID, txHash),
		}); err != nil {
			return fmt.Errorf("sweep: transition confirmed: %w", err)
		}
		o.forgetSweepTx(b.UID)
		return nil
	}

	if time.Since(b.UpdatedAt) < o.sweepStuckAfter {
		return nil
	}
	bumps := o.sweepBumpCount(b.UID)
	if bumps >= o.maxSweepBumps {
		if _, err := o.deps.Booker.Transition(ctx, core.TransitionInput{
			BookingUID:     b.UID,
			ToStatus:       "failed",
			ChannelRef:     b.ChannelRef,
			Source:         onchainSource,
			IdempotencyKey: sweepFailedKey(b.UID, b.ChannelRef),
		}); err != nil {
			return fmt.Errorf("sweep: transition failed: %w", err)
		}
		o.log().Error("service: onchain: sweep stuck: exceeded max gas-bump retries", "booking_uid", b.UID, "bumps", bumps)
		o.forgetSweepTx(b.UID)
		return nil
	}

	gasPrice, err := o.deps.Sweeper.GasPrice(ctx, chainID)
	if err != nil {
		return fmt.Errorf("sweep: gas price: %w", err)
	}
	if gasPrice.GreaterThan(policy.GasCeiling) {
		return nil // wait for gas to come back down rather than bump into a ceiling breach
	}
	targets, err := o.sweepTargets(ctx, addrs)
	if err != nil {
		return fmt.Errorf("sweep: resolve targets for gas-bump: %w", err)
	}
	newTxHash, err := o.deps.Sweeper.BatchSweep(ctx, chainID, token, targets, nonce)
	if err != nil {
		return fmt.Errorf("sweep: gas-bump rebroadcast: %w", err)
	}
	o.trackSweepTx(b.UID, newTxHash)
	o.bumpSweep(b.UID)
	return nil
}

func (o *Onchain) trackSweepTx(bookingUID, txHash string) {
	o.sweepMu.Lock()
	defer o.sweepMu.Unlock()
	o.sweepTx[bookingUID] = txHash
}

func (o *Onchain) currentSweepTx(bookingUID string) string {
	o.sweepMu.Lock()
	defer o.sweepMu.Unlock()
	return o.sweepTx[bookingUID]
}

func (o *Onchain) forgetSweepTx(bookingUID string) {
	o.sweepMu.Lock()
	defer o.sweepMu.Unlock()
	delete(o.sweepTx, bookingUID)
	delete(o.sweepBump, bookingUID)
}

func (o *Onchain) sweepBumpCount(bookingUID string) int {
	o.sweepMu.Lock()
	defer o.sweepMu.Unlock()
	return o.sweepBump[bookingUID]
}

func (o *Onchain) bumpSweep(bookingUID string) {
	o.sweepMu.Lock()
	defer o.sweepMu.Unlock()
	o.sweepBump[bookingUID]++
}

// --- lifecycle ---

// Run starts every background job Onchain owns (watcher, pending/confirming
// recheck, deep-reorg recheck, and -- if sweep policies were configured --
// one sweep loop per policy) and blocks until ctx is cancelled. Jobs whose
// dependencies were not wired are skipped with a log line rather than
// failing the whole subsystem (design doc: onchain is opt-in a la carte).
func (o *Onchain) Run(ctx context.Context) error {
	if err := o.deps.validateCore(); err != nil {
		return fmt.Errorf("service: onchain: run: %w", err)
	}
	if err := o.validateAutoCreditCeilings(); err != nil {
		return fmt.Errorf("service: onchain: run: %w", err)
	}
	if err := o.validateReconcileFailureLimits(); err != nil {
		return fmt.Errorf("service: onchain: run: %w", err)
	}

	g, ctx := errgroup.WithContext(ctx)

	if o.deps.Reader != nil {
		g.Go(func() error {
			return o.runLoop(ctx, "onchain_registration_rescan", o.registrationRescanInterval, o.runRegistrationRescansOnce)
		})
		for chainID := range o.chains {
			chainID := chainID
			g.Go(func() error {
				return o.runLoop(ctx, "onchain_watch", o.watchInterval, func(ctx context.Context) {
					if err := o.scanChainOnce(ctx, chainID); err != nil {
						o.log().Error("service: onchain: watcher tick failed", "chain_id", chainID, "error", err)
					}
				})
			})
		}
		g.Go(func() error {
			return o.runLoop(ctx, "onchain_recheck", o.recheckInterval, o.recheckPendingDeposits)
		})
		g.Go(func() error {
			return o.runLoop(ctx, "onchain_reorg_recheck", o.reorgRecheckInterval, o.recheckConfirmedDeposits)
		})
	} else {
		o.log().Info("service: onchain: no ChainReader configured, watcher/recheck/reorg jobs skipped (webhook-only ingestion)")
	}

	if o.deps.Scanner != nil && o.deps.Sweeper != nil && len(o.sweepPolicies) > 0 {
		for _, policy := range o.sweepPolicies {
			policy := policy
			if err := policy.Validate(); err != nil {
				return fmt.Errorf("service: onchain: run: invalid sweep policy: %w", err)
			}
			job := newSweepLockedJob(policy, o, o.pool)
			g.Go(func() error {
				return o.runLoop(ctx, "onchain_sweep", policy.Interval, job)
			})
		}
	}

	return g.Wait()
}

// runLoop mirrors Worker.runLoop's ticker + ctx.Done() exit convention.
func (o *Onchain) runLoop(ctx context.Context, name string, interval time.Duration, fn func(context.Context)) error {
	if interval <= 0 {
		o.log().Warn("service: onchain: skipping job: interval is non-positive", "job", name)
		<-ctx.Done()
		return nil
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	o.log().Info("service: onchain: started", "job", name, "interval", interval.String())
	for {
		select {
		case <-ctx.Done():
			o.log().Info("service: onchain: stopped", "job", name)
			return nil
		case <-ticker.C:
			fn(ctx)
		}
	}
}

// newSweepLockedJob wraps one policy's sweepTick in a per-chain advisory
// lock, so the same chain's sweep never runs concurrently across replicas
// (design doc §4 "全局单飞"). The lock is scoped to the chain, NOT to
// (chain,token): every token on a given chain shares the same signer EOA
// (core.Sweeper.NextNonce is per-(chainID, signerAddress), see
// chains/evm/sweeper.go), so two tokens' sweepTicks racing on that one
// nonce sequence would both observe the same "next" nonce, broadcast two
// txs at the same nonce, and have one silently supersede the other (M2:
// the stuck loser then gas-bumps into ErrConflict/failed for no on-chain
// reason). Serializing per-chain closes that race; per-token locking would
// only be safe if the signer used one EOA per token, which it does not. A
// per-chain lock is still sufficient headroom for multi-chain deployments
// (different chains never share a nonce space).
func newSweepLockedJob(policy core.SweepPolicy, o *Onchain, pool *pgxpool.Pool) func(context.Context) {
	lockName := fmt.Sprintf("sweep:%d", policy.ChainID)
	lj := NewLockedJob(lockName, func(ctx context.Context) error {
		return o.sweepTick(ctx, policy)
	}, pool, o.deps.Logger)
	return func(ctx context.Context) {
		if err := lj.Run(ctx); err != nil {
			o.log().Error("service: onchain: sweep job failed", "chain_id", policy.ChainID, "error", err)
		}
	}
}
