// Package server: handler_holder.go
// Holder-scoped wallet surface: balances, translated transactions, active
// holds, and (idempotent, self-scoped) deposit-address issuance for the
// token-bound holder
// (docs/plans/2026-07-08-holder-scoped-wallet-surface.md §3.2/§3.4).
//
// No endpoint accepts a holder parameter — the holder comes exclusively from
// the verified token, so a holder can only ever reach its own data. The
// deposit-address routes are the one write in this surface: they never move
// funds, only provision the caller's own CREATE2 receiving address (the same
// DepositAddressProvider the admin API-key surface uses at
// /holders/{holder}/deposit-address — see handler_onchain.go).
package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/bizcode"
	"github.com/azex-ai/ledger/pkg/httpx"
)

// HolderConfig configures the holder wallet surface.
type HolderConfig struct {
	// TokenSecret signs holder tokens (min 32 bytes). Required.
	TokenSecret []byte
	// TokenDefaultTTL is used when a mint request omits ttl_seconds
	// (default 15m). TokenMaxTTL caps requested TTLs (default 1h).
	TokenDefaultTTL time.Duration
	TokenMaxTTL     time.Duration
	// MintKeys enables POST /holder-tokens on the standalone HolderHandler,
	// authenticated by these API keys at write scope. Leave empty when the
	// embedding host mints in-process via MintHolderToken — an
	// unauthenticated HTTP mint would let anyone impersonate any holder.
	MintKeys []APIKey

	// now overrides time.Now in tests.
	now func() time.Time
}

func (c HolderConfig) withDefaults() HolderConfig {
	if c.TokenDefaultTTL <= 0 {
		c.TokenDefaultTTL = defaultHolderTokenTTL
	}
	if c.TokenMaxTTL <= 0 {
		c.TokenMaxTTL = defaultHolderTokenMax
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c
}

// holderSurface bundles the config + reader behind both exposure modes.
type holderSurface struct {
	cfg     HolderConfig
	holders core.HolderReader
}

// HolderHandler returns a self-contained, mountable sub-router serving the
// holder wallet read surface — and nothing else (no admin routes). Library
// consumers mount it into their own HTTP server:
//
//	h, _ := server.HolderHandler(server.HolderConfig{TokenSecret: secret}, svc.HolderReader())
//	r.Mount("/api/v1", h)
//
// Routes (relative): GET /holder/balances, GET /holder/transactions,
// GET /holder/holds — holder-token auth; POST /holder-tokens — only when
// cfg.MintKeys is set (in-process minting via MintHolderToken otherwise).
// Cross-cutting concerns (CORS, rate limiting) are the host's middleware.
func HolderHandler(cfg HolderConfig, holders core.HolderReader) (http.Handler, error) {
	cfg = cfg.withDefaults()
	if len(cfg.TokenSecret) < minHolderSecretLen {
		return nil, fmt.Errorf("server: holder handler: token secret must be at least %d bytes", minHolderSecretLen)
	}
	if holders == nil {
		return nil, fmt.Errorf("server: holder handler: nil HolderReader")
	}
	hs := &holderSurface{cfg: cfg, holders: holders}

	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(holderAuthMiddleware(cfg.TokenSecret, cfg.now))
		r.Get("/holder/balances", hs.handleHolderBalances)
		r.Get("/holder/transactions", hs.handleHolderTransactions)
		r.Get("/holder/holds", hs.handleHolderHolds)
	})
	if len(cfg.MintKeys) > 0 {
		r.Group(func(r chi.Router) {
			r.Use(authMiddleware(cfg.MintKeys))
			r.Post("/holder-tokens", hs.requireMintScope(hs.handleMintHolderToken))
		})
	}
	return r, nil
}

// requireMintScope gates minting on write scope for the standalone handler
// (the ledgerd exposure reuses the server's requireScope group instead).
func (hs *holderSurface) requireMintScope(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			httpx.Error(w, bizcode.New(10101, "unauthenticated"))
			return
		}
		if !id.Scope.allows(ScopeWrite) {
			httpx.Error(w, bizcode.New(10150, "minting holder tokens requires write scope"))
			return
		}
		next(w, r)
	}
}

// ---- Wire types (api-contract.md: envelope, snake_case, string amounts) ----

type holderBalanceResponse struct {
	CurrencyUID  string `json:"currency_uid"`
	CurrencyCode string `json:"currency_code"`
	Available    string `json:"available"`
	Pending      string `json:"pending"`
	Locked       string `json:"locked"`
	Total        string `json:"total"`
}

type holderTransactionResponse struct {
	UID           string `json:"uid"`
	Kind          string `json:"kind"`
	KindLabel     string `json:"kind_label"`
	Direction     string `json:"direction"`
	Amount        string `json:"amount"`
	CurrencyUID   string `json:"currency_uid"`
	CurrencyCode  string `json:"currency_code"`
	OccurredAt    string `json:"occurred_at"`
	ReversalOfUID string `json:"reversal_of_uid"`
	Memo          string `json:"memo"`
}

type holderTransactionsPage struct {
	List       []holderTransactionResponse `json:"list"`
	NextCursor string                      `json:"next_cursor"`
}

type holderHoldResponse struct {
	UID          string `json:"uid"`
	Amount       string `json:"amount"`
	CurrencyUID  string `json:"currency_uid"`
	CurrencyCode string `json:"currency_code"`
	CreatedAt    string `json:"created_at"`
	ExpiresAt    string `json:"expires_at"`
}

type mintHolderTokenRequest struct {
	Holder     int64 `json:"holder"`
	TTLSeconds int64 `json:"ttl_seconds"`
}

type mintHolderTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

// ---- Handlers ----

func (hs *holderSurface) handleMintHolderToken(w http.ResponseWriter, r *http.Request) {
	req, err := httpx.Decode[mintHolderTokenRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if req.Holder == 0 {
		httpx.Error(w, httpx.ErrBadRequest("holder is required"))
		return
	}
	ttl := hs.cfg.TokenDefaultTTL
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
		if ttl > hs.cfg.TokenMaxTTL {
			httpx.Error(w, httpx.ErrBadRequest(fmt.Sprintf("ttl_seconds exceeds the maximum of %d", int64(hs.cfg.TokenMaxTTL/time.Second))))
			return
		}
	}
	now := hs.cfg.now()
	token, err := MintHolderToken(hs.cfg.TokenSecret, req.Holder, ttl, now)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	// Audit trail: which key minted for which holder.
	if id, ok := identityFrom(r.Context()); ok {
		slog.Info("holder token minted", "key", id.Name, "holder", req.Holder, "ttl", ttl.String())
	}
	httpx.OK(w, mintHolderTokenResponse{
		Token:     token,
		ExpiresAt: now.Add(ttl).UTC().Format(time.RFC3339),
	})
}

func (hs *holderSurface) handleHolderBalances(w http.ResponseWriter, r *http.Request) {
	holder, ok := holderFrom(r.Context())
	if !ok {
		httpx.Error(w, bizcode.New(10101, "unauthenticated"))
		return
	}
	balances, err := hs.holders.ListHolderBalances(r.Context(), holder, r.URL.Query().Get("currency_uid"))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	out := make([]holderBalanceResponse, len(balances))
	for i, b := range balances {
		out[i] = holderBalanceResponse{
			CurrencyUID:  b.CurrencyUID,
			CurrencyCode: b.CurrencyCode,
			Available:    b.Available.String(),
			Pending:      b.Pending.String(),
			Locked:       b.Locked.String(),
			Total:        b.Total.String(),
		}
	}
	httpx.OK(w, map[string]any{"list": out})
}

func (hs *holderSurface) handleHolderTransactions(w http.ResponseWriter, r *http.Request) {
	holder, ok := holderFrom(r.Context())
	if !ok {
		httpx.Error(w, bizcode.New(10101, "unauthenticated"))
		return
	}
	limit := int32(0)
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n <= 0 {
			httpx.Error(w, httpx.ErrBadRequest("invalid limit"))
			return
		}
		limit = int32(n)
	}
	items, next, err := hs.holders.ListHolderTransactions(r.Context(), holder, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	out := make([]holderTransactionResponse, len(items))
	for i, it := range items {
		out[i] = holderTransactionResponse{
			UID:           it.UID,
			Kind:          it.Kind,
			KindLabel:     it.KindLabel,
			Direction:     string(it.Direction),
			Amount:        it.Amount.String(),
			CurrencyUID:   it.CurrencyUID,
			CurrencyCode:  it.CurrencyCode,
			OccurredAt:    it.OccurredAt.UTC().Format(time.RFC3339),
			ReversalOfUID: it.ReversalOfUID,
			Memo:          it.Memo,
		}
	}
	httpx.OK(w, holderTransactionsPage{List: out, NextCursor: next})
}

func (hs *holderSurface) handleHolderHolds(w http.ResponseWriter, r *http.Request) {
	holder, ok := holderFrom(r.Context())
	if !ok {
		httpx.Error(w, bizcode.New(10101, "unauthenticated"))
		return
	}
	holds, err := hs.holders.ListHolderHolds(r.Context(), holder)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	out := make([]holderHoldResponse, len(holds))
	for i, h := range holds {
		out[i] = holderHoldResponse{
			UID:          h.UID,
			Amount:       h.Amount.String(),
			CurrencyUID:  h.CurrencyUID,
			CurrencyCode: h.CurrencyCode,
			CreatedAt:    h.CreatedAt.UTC().Format(time.RFC3339),
			ExpiresAt:    h.ExpiresAt.UTC().Format(time.RFC3339),
		}
	}
	httpx.OK(w, map[string]any{"list": out})
}

// handleHolderGetDepositAddress looks up the token-bound holder's
// already-registered deposit address without creating one (404 if absent).
// Reuses the same DepositAddressProvider as the admin
// GET /holders/{holder}/deposit-address route (handler_onchain.go) — the
// holder here is a Server method (not a holderSurface method) because the
// provider lives on Server, wired once via SetDepositAddressProvider for
// both surfaces.
func (s *Server) handleHolderGetDepositAddress(w http.ResponseWriter, r *http.Request) {
	holder, ok := holderFrom(r.Context())
	if !ok {
		httpx.Error(w, bizcode.New(10101, "unauthenticated"))
		return
	}
	if s.depositAddresses == nil {
		httpx.Error(w, bizcode.FeatureNotEnabled)
		return
	}
	addr, err := s.depositAddresses.GetDepositAddress(r.Context(), holder)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, depositAddressToResponse(addr))
}

// handleHolderEnsureDepositAddress issues (idempotently) the token-bound
// holder's CREATE2 deposit address, deriving + registering it on first call.
// See handleHolderGetDepositAddress for the provider-sharing rationale.
func (s *Server) handleHolderEnsureDepositAddress(w http.ResponseWriter, r *http.Request) {
	holder, ok := holderFrom(r.Context())
	if !ok {
		httpx.Error(w, bizcode.New(10101, "unauthenticated"))
		return
	}
	if s.depositAddresses == nil {
		httpx.Error(w, bizcode.FeatureNotEnabled)
		return
	}
	addr, err := s.depositAddresses.EnsureDepositAddress(r.Context(), holder)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.Created(w, depositAddressToResponse(addr))
}

// ---- ledgerd (service mode) integration ----

// SetHolderSurface enables the holder wallet surface on a running-to-be
// Server: GET /api/v1/holder/* (holder-token auth) and POST
// /api/v1/holder-tokens (write scope). Call before Serve — the routes are
// pre-registered and answer 404 until this configures them.
func (s *Server) SetHolderSurface(cfg HolderConfig, holders core.HolderReader) error {
	cfg = cfg.withDefaults()
	if len(cfg.TokenSecret) < minHolderSecretLen {
		return fmt.Errorf("server: holder surface: token secret must be at least %d bytes", minHolderSecretLen)
	}
	if holders == nil {
		return fmt.Errorf("server: holder surface: nil HolderReader")
	}
	s.holder = &holderSurface{cfg: cfg, holders: holders}
	return nil
}

// withHolderSurface adapts a holderSurface method to a Server route,
// answering 404 while the surface is unconfigured (feature off) so probing
// reveals nothing.
func (s *Server) withHolderSurface(h func(*holderSurface, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hs := s.holder
		if hs == nil {
			httpx.Error(w, httpx.ErrNotFound("not found"))
			return
		}
		h(hs, w, r)
	}
}

// holderTokenAuth is the Server-level wrapper around holderAuthMiddleware
// that resolves the (late-bound) surface config per request.
func (s *Server) holderTokenAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hs := s.holder
		if hs == nil {
			httpx.Error(w, httpx.ErrNotFound("not found"))
			return
		}
		holderAuthMiddleware(hs.cfg.TokenSecret, hs.cfg.now)(next).ServeHTTP(w, r)
	})
}
