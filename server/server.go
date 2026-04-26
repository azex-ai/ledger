package server

import (
	"log/slog"
	"net/http"
	"os"

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
	booker        core.Booker
	bookingReader core.BookingReader
	eventReader     core.EventReader
	classifications core.ClassificationStore
	journalTypes    core.JournalTypeStore
	templates       core.TemplateStore
	currencies      core.CurrencyStore
	channels        map[string]channel.Adapter // channel name → adapter

	// Services (injected)
	reconciler   core.Reconciler
	snapshotter  core.Snapshotter
	systemRollup *service.SystemRollupService

	// Query helpers (direct sqlcgen access for list queries)
	queries core.QueryProvider
}

// Config holds server configuration.
type Config struct {
	Port string
}

// New creates a new Server with all dependencies.
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
) *Server {
	s := &Server{
		journals:        journals,
		balances:        balances,
		reserver:        reserver,
		booker:        booker,
		bookingReader: bookingReader,
		eventReader:     eventReader,
		classifications: classifications,
		journalTypes:    journalTypes,
		templates:       templates,
		currencies:      currencies,
		channels:        channels,
		reconciler:      reconciler,
		snapshotter:     snapshotter,
		systemRollup:    systemRollup,
		queries:         queries,
	}
	s.router = chi.NewRouter()
	s.router.Use(corsMiddleware)
	s.router.Use(middleware.Logger)
	s.router.Use(middleware.Recoverer)
	s.router.Use(middleware.RequestID)
	s.setupRoutes()
	return s
}

// corsMiddleware handles CORS preflight and response headers.
// The allowed origin is read from CORS_ALLOWED_ORIGIN; defaults to "*" for dev.
func corsMiddleware(next http.Handler) http.Handler {
	allowedOrigin := os.Getenv("CORS_ALLOWED_ORIGIN")
	if allowedOrigin == "" {
		allowedOrigin = "*"
		slog.Warn("CORS_ALLOWED_ORIGIN not set, defaulting to * (allow all origins)")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}
