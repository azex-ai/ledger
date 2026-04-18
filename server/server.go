package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

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
	depositor       core.Depositor
	withdrawer      core.Withdrawer
	classifications core.ClassificationStore
	journalTypes    core.JournalTypeStore
	templates       core.TemplateStore
	currencies      core.CurrencyStore

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
	depositor core.Depositor,
	withdrawer core.Withdrawer,
	classifications core.ClassificationStore,
	journalTypes core.JournalTypeStore,
	templates core.TemplateStore,
	currencies core.CurrencyStore,
	reconciler core.Reconciler,
	snapshotter core.Snapshotter,
	systemRollup *service.SystemRollupService,
	queries core.QueryProvider,
) *Server {
	s := &Server{
		journals:        journals,
		balances:        balances,
		reserver:        reserver,
		depositor:       depositor,
		withdrawer:      withdrawer,
		classifications: classifications,
		journalTypes:    journalTypes,
		templates:       templates,
		currencies:      currencies,
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
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
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
