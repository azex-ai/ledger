package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/azex-ai/ledger/channel"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/presets"
)

// Server is the HTTP API server for the ledger.
type Server struct {
	router chi.Router

	// Stores (injected)
	journals        core.JournalWriter
	balances        core.BalanceReader
	reserver        core.Reserver
	booker          core.Booker
	bookingReader   core.BookingReader
	eventReader     core.EventReader
	classifications core.ClassificationStore
	journalTypes    core.JournalTypeStore
	templates       core.TemplateStore
	currencies      core.CurrencyStore
	accountPolicies core.AccountPolicyStore
	channels        map[string]channel.Adapter // channel name → adapter

	// Read-only audit / platform analytics. These were previously only
	// reachable through cmd/ledger-cli; wiring them here exposes the same
	// facade accessors over HTTP.
	audit            core.AuditQuerier
	platformBalances core.PlatformBalanceReader
	solvency         core.SolvencyChecker
	balanceTrends    core.BalanceTrendReader
	periodCloser     core.PeriodCloser
	trialBalance     core.TrialBalanceReader

	// Services (injected)
	reconciler     core.Reconciler
	fullReconciler core.FullReconciler

	// Query helpers (direct sqlcgen access for list queries)
	queries core.QueryProvider

	// Readiness signal (set by main.go after worker boot).
	ready *atomic.Bool

	// Rate limiter — held so its GC loop can be stopped on shutdown.
	rateLimiter *rateLimiter

	// Optional inbound-webhook replay cache (see SetWebhookNonceRecorder).
	// webhookNonceWarnOnce fires the one-time "replay protection is off"
	// warning from the handler rather than from newServer, because the
	// recorder is late-bound.
	webhookNonces        WebhookNonceRecorder
	webhookNonceWarnOnce sync.Once

	// Optional crypto-deposit add-on (see SetDepositAddressProvider /
	// SetDepositIngester in handler_onchain.go). Nil until a consumer's
	// composition root wires service.OnchainService in; every deposit-address
	// route and the onchain webhook sighting path answer
	// bizcode.FeatureNotEnabled until then.
	depositAddresses DepositAddressProvider
	depositIngester  DepositIngester

	// Optional crypto-deposit human-review add-on (see SetDepositReviewer in
	// handler_deposit_reviews.go). Nil until a consumer's composition root
	// wires service.OnchainService in; every /deposits/reviews* route
	// answers bizcode.FeatureNotEnabled until then.
	depositReviewer DepositReviewer

	// Optional Prometheus /metrics handler. Mounted outside chi's middleware
	// chain so it bypasses auth + rate limiting (scrapers usually live on
	// the internal network and authenticate by host/port).
	metricsHandler http.Handler

	// authEnabled records whether API keys are configured; when false (dev
	// only) requireScope checks pass unconditionally.
	authEnabled bool

	// devCreditEnabled gates POST /dev/credits, which mints holder balance
	// against presets.DevCreditClassificationCode with no custodied asset
	// behind it. False by default; Config.Validate refuses to set it outside
	// ENV=dev (see handler_devcredit.go).
	devCreditEnabled bool

	// holder is the optional holder wallet surface (SetHolderSurface); nil
	// keeps every /holder* route answering 404.
	holder *holderSurface

	// protectedTemplateCodes is presets.ProtectedTemplateCodes() plus
	// Config.ProtectedTemplateCodes, minus Config.AllowGenericTemplatePost,
	// indexed for O(1) lookup by handlePostTemplate. See those fields' doc
	// comments.
	protectedTemplateCodes map[string]bool

	// allowGenericTemplatePost indexes Config.AllowGenericTemplatePost as
	// given, before it is subtracted from protectedTemplateCodes above. It is
	// kept separately because it is also the opt-out for handlePostTemplate's
	// structural is_system-leg rule, which has no code list to subtract from
	// (D-C1).
	allowGenericTemplatePost map[string]bool

	// allowSystemClassificationPost mirrors Config.AllowSystemClassificationPost:
	// when false (the default), handlePostJournal refuses a handwritten journal
	// that touches an is_system classification.
	allowSystemClassificationPost bool
}

// SetMetricsHandler installs an http.Handler that ServeHTTP will dispatch to
// for any GET on /metrics, completely bypassing auth and rate-limit
// middleware. Pass nil to disable.
//
// SECURITY: because /metrics skips the middleware chain, the handler you pass
// is exposed with NO authentication or rate limiting. The exported series
// (checkpoint_age, balance_drift, reserved_amount by currency, chain_cursor_lag,
// …) are aggregate operational intelligence with no per-holder dimension, but
// they are still business intelligence. If this port is reachable by anyone
// beyond your Prometheus scraper, wrap the handler with your own auth before
// passing it here (or keep /metrics on a network the scraper alone can reach).
func (s *Server) SetMetricsHandler(h http.Handler) { s.metricsHandler = h }

// Config holds server configuration loaded from environment.
type Config struct {
	Env             string // "dev" or "production"; controls fail-fast behavior
	CORSAllowOrigin string // exact origin to allow; empty in dev = "*"
	APIKeys         []APIKey
	MaxBodyBytes    int64 // request body cap; default 256 KB
	// TrustedProxyCIDRs lists the CIDR ranges of trusted edge proxies. When
	// non-empty, client-IP extraction from X-Forwarded-For / X-Real-IP /
	// True-Client-IP is enabled, but ONLY for requests whose socket peer is
	// inside one of these ranges — a direct caller (peer outside the ranges)
	// can never spoof its IP past the rate limiter. Empty = headers never
	// trusted (r.RemoteAddr stays the socket peer).
	TrustedProxyCIDRs []netip.Prefix
	// HolderTokenSecret (HOLDER_TOKEN_SECRET) enables the holder wallet
	// surface when set (min 32 bytes; boot fails on a shorter value).
	// Empty = surface disabled, /holder* routes answer 404.
	HolderTokenSecret []byte
	// DevCreditEnabled (DEV_CREDIT_ENABLED) turns on POST /dev/credits, the
	// developer-mode facility that credits a holder without a matching
	// custodied asset — a simulated top-up. False by default, and Validate
	// refuses any value but false unless Env is "dev", so no production
	// deployment can enable it by config accident. The accounting side is
	// presets.DevCreditBundle, which must be installed separately.
	DevCreditEnabled bool
	// ProtectedTemplateCodes (PROTECTED_TEMPLATE_CODES, comma-separated)
	// lists entry-template codes that POST /journals/template refuses (403)
	// no matter how the caller got a write-scope key — structure.md's
	// finding: unlike POST /dev/credits (which is hardcoded to one narrow
	// template and gated behind DevCreditEnabled), the generic template
	// endpoint has no allowlist at all, so a write-scope key can post a
	// journal indistinguishable from a real verified deposit by naming its
	// template code directly.
	//
	// This list is the SECOND of that endpoint's two layers, not its primary
	// guard: the endpoint also refuses, structurally, any template with a leg
	// on an is_system classification, whoever defined it (D-C1, 2026-09-02
	// audit — the four-code list left dev_credit reachable, and covered no
	// deployment-defined system-side template at all). A name list is still
	// worth keeping alongside it because it answers before the template table
	// is read, and therefore also when that table cannot be read or the rows
	// do not exist yet. Both layers share AllowGenericTemplatePost as their
	// only opt-out.
	//
	// The effective protected set is presets.ProtectedTemplateCodes()
	// (deposit_confirm, deposit_confirm_pending, deposit_release_pending,
	// deposit_record_overage — every template a deployment's own
	// verified-deposit orchestration posts via PostAuthorized, never over
	// this endpoint — plus dev_credit, which mints spendable balance with
	// nothing behind it) PLUS this field, MINUS AllowGenericTemplatePost
	// below.
	// This field is additive: list a deployment's own system-only template
	// codes here, on top of the library default — it does not need to (and
	// should not need to) repeat the library's own codes to get them
	// protected.
	//
	// The library defaults this protection on, rather than requiring every
	// deployment to remember to enable it, because it already knows which
	// codes its own deposit bundles install (InstallDefaultTemplatePresets,
	// DepositBundle, InstallPendingBundle) — the earlier "empty by default"
	// design left the finding open in every deployment that didn't
	// separately opt in. A deployment that genuinely wants one of the
	// default-protected codes reachable through this generic endpoint sets
	// AllowGenericTemplatePost, rather than this list being empty by
	// omission.
	ProtectedTemplateCodes []string
	// AllowGenericTemplatePost (ALLOW_GENERIC_TEMPLATE_POST, comma-separated)
	// is the single, per-code opt-out for BOTH of POST /journals/template's
	// guards: it removes codes from the effective protected set computed
	// above (applied last, so it can opt a library-default code such as
	// "deposit_confirm" back in), and it exempts the named code from the
	// structural is_system-leg rule. A deployment with its own reason to
	// post one of these through the generic endpoint (e.g. a reviewed,
	// admin-scope-only internal tool) names it here. Empty by default: the
	// library defaults land closed, and only a deployment that explicitly
	// names a code here weakens that.
	AllowGenericTemplatePost []string
	// AllowSystemClassificationPost (ALLOW_SYSTEM_CLASSIFICATION_POST) opts a
	// deployment out of the default guard on POST /journals: by default the
	// handwritten-journal endpoint refuses any entry that touches a
	// classification flagged is_system (custodial, suspense, equity, ...).
	//
	// This is the handwritten-path counterpart to ProtectedTemplateCodes
	// (M-2, 2026-08-29 review). Without it, a leaked or over-scoped write-scope
	// key could hand-post `DR main_wallet(user) / CR custodial(system)` -- a
	// journal byte-for-byte identical to a verified on-chain deposit -- while
	// the template blacklist only guards the named-template endpoint. Because
	// the ledger exists to control who can mint accounting after a DB breach,
	// this guard lands closed by default; a deployment whose own flows legitimately
	// hand-post system-side journals over HTTP (rather than through a template
	// or server-side orchestration) sets this true after a reviewed decision.
	// The library's own deposit/transfer/fx/capital flows go through templates,
	// not this endpoint, so the default does not constrain them.
	AllowSystemClassificationPost bool
}

// Validate rejects configurations that would expose a production server or
// weaken its request-boundary protections. Dev mode deliberately permits an
// unauthenticated server for local tests and examples.
func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("server: config is required")
	}
	if c.Env == "" {
		return fmt.Errorf("server: ENV is required")
	}
	if c.Env != "dev" {
		if c.CORSAllowOrigin == "" || c.CORSAllowOrigin == "*" {
			return fmt.Errorf("server: exact CORS_ALLOWED_ORIGIN is required when ENV=%q", c.Env)
		}
		if len(c.APIKeys) == 0 {
			return fmt.Errorf("server: API_KEYS is required when ENV=%q", c.Env)
		}
	}
	if c.DevCreditEnabled && c.Env != "dev" {
		// Deliberately fatal rather than a warning: this switch lets a caller
		// create holder balance out of nothing, which shows up as a solvency
		// shortfall that no custodied asset can close.
		return fmt.Errorf("server: DevCreditEnabled requires ENV=dev (got %q)", c.Env)
	}
	if c.MaxBodyBytes <= 0 {
		return fmt.Errorf("server: MaxBodyBytes must be positive")
	}
	return nil
}

// parseCommaSeparated splits a comma-separated env value into a trimmed,
// empty-entry-filtered slice. Returns nil for an empty/whitespace-only
// input, matching the pre-existing behavior of the inline parsing this
// replaces (PROTECTED_TEMPLATE_CODES had its own copy; this is now shared
// with ALLOW_GENERIC_TEMPLATE_POST).
func parseCommaSeparated(raw string) []string {
	var out []string
	for _, entry := range strings.Split(raw, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			out = append(out, entry)
		}
	}
	return out
}

// LoadConfig reads server config from env. Returns an error in production
// when CORS_ALLOWED_ORIGIN is unset — we refuse to ship with wildcard CORS.
func LoadConfig() (*Config, error) {
	env := os.Getenv("ENV")
	if env == "" {
		env = "production"
	}

	corsOrigin := os.Getenv("CORS_ALLOWED_ORIGIN")
	if env != "dev" && corsOrigin == "" {
		return nil, fmt.Errorf("server: CORS_ALLOWED_ORIGIN is required when ENV=%q (refusing to default to *)", env)
	}

	var holderSecret []byte
	if v := os.Getenv("HOLDER_TOKEN_SECRET"); v != "" {
		if len(v) < 32 {
			return nil, fmt.Errorf("server: HOLDER_TOKEN_SECRET must be at least 32 bytes (got %d)", len(v))
		}
		holderSecret = []byte(v)
	}

	maxBytes := int64(256 * 1024)
	if v := os.Getenv("MAX_BODY_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("server: invalid MAX_BODY_BYTES %q: must be a positive integer", v)
		}
		maxBytes = n
	}

	keys, err := parseAPIKeys(os.Getenv("API_KEYS"))
	if err != nil {
		return nil, err
	}

	trustedCIDRs, err := parseTrustedProxyCIDRs(os.Getenv("TRUSTED_PROXY_CIDRS"))
	if err != nil {
		return nil, fmt.Errorf("server: invalid TRUSTED_PROXY_CIDRS: %w", err)
	}

	devCredit := os.Getenv("DEV_CREDIT_ENABLED") == "true"

	protectedTemplateCodes := parseCommaSeparated(os.Getenv("PROTECTED_TEMPLATE_CODES"))
	allowGenericTemplatePost := parseCommaSeparated(os.Getenv("ALLOW_GENERIC_TEMPLATE_POST"))

	cfg := &Config{
		Env:                      env,
		CORSAllowOrigin:          corsOrigin,
		APIKeys:                  keys,
		MaxBodyBytes:             maxBytes,
		TrustedProxyCIDRs:        trustedCIDRs,
		HolderTokenSecret:        holderSecret,
		DevCreditEnabled:         devCredit,
		ProtectedTemplateCodes:   protectedTemplateCodes,
		AllowGenericTemplatePost: allowGenericTemplatePost,
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// New creates a new Server with all dependencies. Configuration is read from
// the environment via LoadConfig — call NewWithConfig if you need custom config.
func New(
	journals core.JournalWriter,
	balances core.BalanceReader,
	reserver core.Reserver,
	booker core.Booker,
	bookingReader core.BookingReader,
	eventReader core.EventReader,
	classifications core.ClassificationStore,
	journalTypes core.JournalTypeStore,
	templates core.TemplateStore,
	currencies core.CurrencyStore,
	channels map[string]channel.Adapter,
	reconciler core.Reconciler,
	queries core.QueryProvider,
	audit core.AuditQuerier,
	platformBalances core.PlatformBalanceReader,
	solvency core.SolvencyChecker,
	balanceTrends core.BalanceTrendReader,
	fullReconciler core.FullReconciler,
	accountPolicies core.AccountPolicyStore,
	periodCloser core.PeriodCloser,
	trialBalance core.TrialBalanceReader,
) *Server {
	cfg, err := LoadConfig()
	if err != nil {
		panic(fmt.Sprintf("server: load config: %v", err))
	}
	return NewWithConfig(cfg, journals, balances, reserver, booker, bookingReader,
		eventReader, classifications, journalTypes, templates, currencies, channels,
		reconciler, queries,
		audit, platformBalances, solvency, balanceTrends, fullReconciler,
		accountPolicies, periodCloser, trialBalance)
}

// NewWithConfig creates a Server using an explicit config, skipping env-var loading.
func NewWithConfig(
	cfg *Config,
	journals core.JournalWriter,
	balances core.BalanceReader,
	reserver core.Reserver,
	booker core.Booker,
	bookingReader core.BookingReader,
	eventReader core.EventReader,
	classifications core.ClassificationStore,
	journalTypes core.JournalTypeStore,
	templates core.TemplateStore,
	currencies core.CurrencyStore,
	channels map[string]channel.Adapter,
	reconciler core.Reconciler,
	queries core.QueryProvider,
	audit core.AuditQuerier,
	platformBalances core.PlatformBalanceReader,
	solvency core.SolvencyChecker,
	balanceTrends core.BalanceTrendReader,
	fullReconciler core.FullReconciler,
	accountPolicies core.AccountPolicyStore,
	periodCloser core.PeriodCloser,
	trialBalance core.TrialBalanceReader,
) *Server {
	deps := Deps{
		Journals:         journals,
		Balances:         balances,
		Reserver:         reserver,
		Booker:           booker,
		BookingReader:    bookingReader,
		EventReader:      eventReader,
		Classifications:  classifications,
		JournalTypes:     journalTypes,
		Templates:        templates,
		Currencies:       currencies,
		Channels:         channels,
		Reconciler:       reconciler,
		Queries:          queries,
		Audit:            audit,
		PlatformBalances: platformBalances,
		Solvency:         solvency,
		BalanceTrends:    balanceTrends,
		FullReconciler:   fullReconciler,
		AccountPolicies:  accountPolicies,
		PeriodCloser:     periodCloser,
		TrialBalance:     trialBalance,
	}
	if err := cfg.Validate(); err != nil {
		panic(fmt.Sprintf("server: invalid config: %v", err))
	}
	return newServer(cfg, deps)
}

// Deps bundles every dependency NewWithConfig/New take as positional
// parameters. Prefer NewFromDeps(cfg, deps) for new composition roots: the
// twenty-one same-shaped interface parameters in a fixed positional order
// (New/NewWithConfig) have no compiler help catching an accidental
// transposition -- interfaces don't carry field names, so two swapped
// arguments of matching interface shape compile clean and fail at runtime
// instead (structure.md's Minor). The server does not consume a Snapshotter
// or SystemRollup service (those drive the background Worker, assembled
// separately); they were dropped from these constructors in the 2026-08-29
// review (MJ-5) so the HTTP layer depends only on core interfaces.
type Deps struct {
	Journals         core.JournalWriter
	Balances         core.BalanceReader
	Reserver         core.Reserver
	Booker           core.Booker
	BookingReader    core.BookingReader
	EventReader      core.EventReader
	Classifications  core.ClassificationStore
	JournalTypes     core.JournalTypeStore
	Templates        core.TemplateStore
	Currencies       core.CurrencyStore
	Channels         map[string]channel.Adapter
	Reconciler       core.Reconciler
	Queries          core.QueryProvider
	Audit            core.AuditQuerier
	PlatformBalances core.PlatformBalanceReader
	Solvency         core.SolvencyChecker
	BalanceTrends    core.BalanceTrendReader
	FullReconciler   core.FullReconciler
	AccountPolicies  core.AccountPolicyStore
	PeriodCloser     core.PeriodCloser
	TrialBalance     core.TrialBalanceReader
}

// NewFromDeps is NewWithConfig taking a Deps struct instead of twenty-three
// positional parameters, and returning an error instead of panicking on an
// invalid config -- the caller decides how to fail (os.Exit, log.Fatal,
// propagate up its own composition root), rather than every composition
// root needing its own recover() to turn NewWithConfig's panic into a
// graceful exit.
func NewFromDeps(cfg *Config, deps Deps) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("server: invalid config: %w", err)
	}
	return newServer(cfg, deps), nil
}

// newServer is the shared builder behind NewWithConfig and NewFromDeps.
// Callers must validate cfg first.
func newServer(cfg *Config, deps Deps) *Server {
	defaultProtected := presets.ProtectedTemplateCodes()
	protectedTemplateCodes := make(map[string]bool, len(defaultProtected)+len(cfg.ProtectedTemplateCodes))
	for _, code := range defaultProtected {
		protectedTemplateCodes[code] = true
	}
	for _, code := range cfg.ProtectedTemplateCodes {
		protectedTemplateCodes[code] = true
	}
	allowGenericTemplatePost := make(map[string]bool, len(cfg.AllowGenericTemplatePost))
	for _, code := range cfg.AllowGenericTemplatePost {
		allowGenericTemplatePost[code] = true
		delete(protectedTemplateCodes, code)
	}
	s := &Server{
		journals:                 deps.Journals,
		balances:                 deps.Balances,
		reserver:                 deps.Reserver,
		booker:                   deps.Booker,
		bookingReader:            deps.BookingReader,
		eventReader:              deps.EventReader,
		classifications:          deps.Classifications,
		journalTypes:             deps.JournalTypes,
		templates:                deps.Templates,
		currencies:               deps.Currencies,
		accountPolicies:          deps.AccountPolicies,
		channels:                 deps.Channels,
		audit:                    deps.Audit,
		platformBalances:         deps.PlatformBalances,
		solvency:                 deps.Solvency,
		balanceTrends:            deps.BalanceTrends,
		periodCloser:             deps.PeriodCloser,
		trialBalance:             deps.TrialBalance,
		reconciler:               deps.Reconciler,
		fullReconciler:           deps.FullReconciler,
		queries:                  deps.Queries,
		ready:                    &atomic.Bool{},
		rateLimiter:              newRateLimiter(defaultRateLimiterConfig()),
		authEnabled:              len(cfg.APIKeys) > 0,
		devCreditEnabled:         cfg.DevCreditEnabled,
		protectedTemplateCodes:   protectedTemplateCodes,
		allowGenericTemplatePost: allowGenericTemplatePost,

		allowSystemClassificationPost: cfg.AllowSystemClassificationPost,
	}
	if cfg.DevCreditEnabled {
		slog.Warn("server: developer credit endpoint is ENABLED — POST /api/v1/dev/credits mints holder balance with no custodied asset behind it")
	}
	if cfg.AllowSystemClassificationPost {
		slog.Warn("server: POST /journals may post to is_system classifications — the deposit-forgery guard is OPTED OUT (Config.AllowSystemClassificationPost)")
	}
	if !s.authEnabled {
		slog.Warn("server: API key authentication is DISABLED — every endpoint is open to any caller that can reach this port (set Config.APIKeys to require a bearer key)")
	}
	if cfg.CORSAllowOrigin == "" || cfg.CORSAllowOrigin == "*" {
		slog.Warn("server: CORS is wide open (Access-Control-Allow-Origin: *) — set Config.CORSAllowOrigin to a specific origin in production")
	}

	r := chi.NewRouter()
	// Order matters: RequestID first so every later log/error has it; Recoverer
	// before our logger so panics still produce a 500 line; CORS before
	// auth/body-limit so OPTIONS preflight short-circuits without a key; body
	// limit before rate limit before auth so we reject hostile traffic cheaply.
	r.Use(middleware.RequestID)
	if len(cfg.TrustedProxyCIDRs) > 0 {
		// Deliberately NOT chi's middleware.RealIP: that trusted
		// client-controlled headers unconditionally (GHSA-3fxj-6jh8-hvhx).
		// trustedProxyRealIP only rewrites when the socket peer is a trusted
		// proxy, so direct callers cannot spoof their IP.
		slog.Info("server: trusting proxy headers for client IP", "trusted_proxy_cidrs", len(cfg.TrustedProxyCIDRs))
		r.Use(trustedProxyRealIP(cfg.TrustedProxyCIDRs))
	}
	r.Use(middleware.Recoverer)
	r.Use(requestLoggerMiddleware)
	r.Use(corsMiddleware(cfg))
	r.Use(bodyLimitMiddleware(cfg.MaxBodyBytes))
	r.Use(rateLimitMiddleware(s.rateLimiter))

	if len(cfg.APIKeys) > 0 {
		r.Use(authMiddleware(cfg.APIKeys))
	}

	// After auth, not before: the alias reads the whole body, unmarshals and
	// re-marshals it to lift Idempotency-Key into the payload. That is business
	// work on an authenticated request, not hostile-traffic triage, so it must
	// sit behind rate-limit + auth (the webhook path stays exempt inside the
	// middleware itself). Placing it before auth let an unauthenticated caller
	// force a full parse/re-serialize on every POST.
	r.Use(idempotencyHeaderAliasMiddleware)

	s.router = r
	s.setupRoutes()
	return s
}

// SetReady marks the service as ready (e.g. after migrations + worker boot).
func (s *Server) SetReady(ready bool) { s.ready.Store(ready) }

// IsReady reports whether the readiness flag is set.
func (s *Server) IsReady() bool { return s.ready.Load() }

// StartRateLimiterGC launches the per-IP bucket GC loop in a goroutine; it
// returns immediately and exits when stop is closed. Call this once after New().
func (s *Server) StartRateLimiterGC(stop <-chan struct{}) {
	go s.rateLimiter.gcLoop(stop)
}

// corsMiddleware applies CORS headers and handles preflight. In production
// (ENV != "dev") cfg.CORSAllowOrigin must be a single explicit origin —
// LoadConfig fails fast when it's empty. In dev we fall back to "*", but only
// without credentials (the spec forbids "*"+credentials together).
func corsMiddleware(cfg *Config) func(http.Handler) http.Handler {
	origin := cfg.CORSAllowOrigin
	if origin == "" {
		// LoadConfig only allows this in dev mode.
		origin = "*"
	}
	allowCredentials := origin != "*"

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Idempotency-Key")
			if allowCredentials {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ServeHTTP implements http.Handler. /metrics is dispatched to the optional
// Prometheus handler before any chi middleware (auth, rate limit, body limit)
// runs — Prometheus scrapers should not present API keys.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.metricsHandler != nil && r.Method == http.MethodGet && r.URL.Path == "/metrics" {
		s.metricsHandler.ServeHTTP(w, r)
		return
	}
	s.router.ServeHTTP(w, r)
}
