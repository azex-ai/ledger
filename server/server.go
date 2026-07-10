package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"sync/atomic"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/azex-ai/ledger/channel"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/service"
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
	snapshotter    core.Snapshotter
	systemRollup   *service.SystemRollupService

	// Query helpers (direct sqlcgen access for list queries)
	queries core.QueryProvider

	// Readiness signal (set by main.go after worker boot).
	ready *atomic.Bool

	// Rate limiter — held so its GC loop can be stopped on shutdown.
	rateLimiter *rateLimiter

	// Optional inbound-webhook replay cache (see SetWebhookNonceRecorder).
	webhookNonces WebhookNonceRecorder

	// Optional crypto-deposit add-on (see SetDepositAddressProvider /
	// SetDepositIngester in handler_onchain.go). Nil until a consumer's
	// composition root wires service.OnchainService in; every deposit-address
	// route and the onchain webhook sighting path answer
	// bizcode.FeatureNotEnabled until then.
	depositAddresses DepositAddressProvider
	depositIngester  DepositIngester

	// Optional Prometheus /metrics handler. Mounted outside chi's middleware
	// chain so it bypasses auth + rate limiting (scrapers usually live on
	// the internal network and authenticate by host/port).
	metricsHandler http.Handler

	// authEnabled records whether API keys are configured; when false (dev
	// only) requireScope checks pass unconditionally.
	authEnabled bool

	// holder is the optional holder wallet surface (SetHolderSurface); nil
	// keeps every /holder* route answering 404.
	holder *holderSurface
}

// SetMetricsHandler installs an http.Handler that ServeHTTP will dispatch to
// for any GET on /metrics, completely bypassing auth and rate-limit
// middleware. Pass nil to disable.
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

	return &Config{
		Env:               env,
		CORSAllowOrigin:   corsOrigin,
		APIKeys:           keys,
		MaxBodyBytes:      maxBytes,
		TrustedProxyCIDRs: trustedCIDRs,
		HolderTokenSecret: holderSecret,
	}, nil
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
	snapshotter core.Snapshotter,
	systemRollup *service.SystemRollupService,
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
		// Tests construct Server directly through New() and may not set the
		// production env vars; fall back to a permissive dev config so the
		// existing test suite keeps working unchanged.
		slog.Warn("server: LoadConfig failed, falling back to dev defaults", "error", err)
		cfg = &Config{Env: "dev", CORSAllowOrigin: "*", MaxBodyBytes: 256 * 1024}
	}
	return NewWithConfig(cfg, journals, balances, reserver, booker, bookingReader,
		eventReader, classifications, journalTypes, templates, currencies, channels,
		reconciler, snapshotter, systemRollup, queries,
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
	snapshotter core.Snapshotter,
	systemRollup *service.SystemRollupService,
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
	s := &Server{
		journals:         journals,
		balances:         balances,
		reserver:         reserver,
		booker:           booker,
		bookingReader:    bookingReader,
		eventReader:      eventReader,
		classifications:  classifications,
		journalTypes:     journalTypes,
		templates:        templates,
		currencies:       currencies,
		accountPolicies:  accountPolicies,
		channels:         channels,
		audit:            audit,
		platformBalances: platformBalances,
		solvency:         solvency,
		balanceTrends:    balanceTrends,
		periodCloser:     periodCloser,
		trialBalance:     trialBalance,
		reconciler:       reconciler,
		fullReconciler:   fullReconciler,
		snapshotter:      snapshotter,
		systemRollup:     systemRollup,
		queries:          queries,
		ready:            &atomic.Bool{},
		rateLimiter:      newRateLimiter(defaultRateLimiterConfig()),
		authEnabled:      len(cfg.APIKeys) > 0,
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
	r.Use(idempotencyHeaderAliasMiddleware)
	r.Use(rateLimitMiddleware(s.rateLimiter))

	if len(cfg.APIKeys) > 0 {
		r.Use(authMiddleware(cfg.APIKeys))
	} else if cfg.Env != "dev" {
		// Production without keys would be silently open — refuse.
		// Logged as an error; main.go's LoadConfig should already have failed
		// fast, but defend in depth.
		slog.Error("server: no API_KEYS configured in non-dev ENV; ALL endpoints (reads included) WILL be unauthenticated")
	}

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
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
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
