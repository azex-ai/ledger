// Package server: middleware_auth.go
// Bearer-token API key authentication for every endpoint (reads included),
// with per-key scopes (read < write < admin) and a per-key name that is
// attached to the request log line for auditability.
package server

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"

	"github.com/azex-ai/ledger/pkg/bizcode"
	"github.com/azex-ai/ledger/pkg/httpx"
)

// Scope is the privilege level carried by an API key. Levels are ordered:
// a key at a given level implies every lower level (admin ⊇ write ⊇ read).
type Scope uint8

const (
	// ScopeRead grants the query surface: balances, journals, entries,
	// events, audit, platform analytics, metadata listings.
	ScopeRead Scope = iota + 1
	// ScopeWrite grants read plus the business write surface: posting
	// journals, reversals, reservations, bookings.
	ScopeWrite
	// ScopeAdmin grants everything, including configuration mutations
	// (classifications, journal types, templates, currencies), account
	// policies, reconciliation triggers, and period close.
	ScopeAdmin
)

// ParseScope maps the wire form ("read" / "write" / "admin") to a Scope.
func ParseScope(s string) (Scope, error) {
	switch s {
	case "read":
		return ScopeRead, nil
	case "write":
		return ScopeWrite, nil
	case "admin":
		return ScopeAdmin, nil
	default:
		return 0, fmt.Errorf("server: unknown api key scope %q (want read|write|admin)", s)
	}
}

func (s Scope) String() string {
	switch s {
	case ScopeRead:
		return "read"
	case ScopeWrite:
		return "write"
	case ScopeAdmin:
		return "admin"
	default:
		return "unknown"
	}
}

// allows reports whether a key at scope s may perform an action that
// requires the given scope.
func (s Scope) allows(required Scope) bool { return s >= required }

// APIKey is one configured credential: a stable name (logged for audit,
// never secret), a scope, and the secret bearer token.
type APIKey struct {
	Name   string
	Scope  Scope
	Secret []byte
}

// authIdentity is the resolved identity of an authenticated request,
// stored in the request context by authMiddleware.
type authIdentity struct {
	Name  string
	Scope Scope
}

type authIdentityCtxKey struct{}

// identityFrom extracts the authenticated key identity, if any.
func identityFrom(ctx context.Context) (authIdentity, bool) {
	id, ok := ctx.Value(authIdentityCtxKey{}).(authIdentity)
	return id, ok
}

// unauthenticatedPaths are liveness/readiness probes that must answer without
// credentials (Kubernetes probes cannot present API keys). Everything else —
// reads included — requires a key: holder is a guessable int64, so an open
// GET surface hands any network-reachable caller every holder's balances and
// transaction history.
var unauthenticatedPaths = map[string]struct{}{
	"/api/v1/system/health": {},
	"/api/v1/system/ready":  {},
}

// webhookPathPrefix carves the inbound channel-callback surface out of
// bearer auth: callbacks originate from external systems (chain scanners,
// PSPs) that cannot hold our API keys — their authentication is the
// channel adapter's own signature verification (e.g. HMAC in
// channel/onchain). This matches the OpenAPI contract, which declares the
// webhook path `security: []`.
const webhookPathPrefix = "/api/v1/webhooks/"

func isUnauthenticatedPath(path string) bool {
	if _, ok := unauthenticatedPaths[path]; ok {
		return true
	}
	return strings.HasPrefix(path, webhookPathPrefix)
}

// authMiddleware enforces bearer-token API key auth on every request except
// the probe endpoints above. All methods — reads included — require
// Authorization: Bearer <key> matching one of the configured keys;
// constant-time compared via crypto/subtle. The matched key's name and scope
// are stored in the request context for scope checks (requireScope) and the
// audit log line. (OPTIONS is handled by the CORS middleware before auth runs.)
func authMiddleware(keys []APIKey) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isUnauthenticatedPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			provided, ok := extractBearer(r.Header.Get("Authorization"))
			if !ok {
				httpx.Error(w, bizcode.New(10101, "missing or malformed Authorization header"))
				return
			}

			key, ok := matchAnyKey(provided, keys)
			if !ok {
				httpx.Error(w, bizcode.New(10101, "invalid api key"))
				return
			}

			id := authIdentity{Name: key.Name, Scope: key.Scope}
			if h, hok := authLogFrom(r.Context()); hok {
				h.name, h.scope = id.Name, id.Scope.String()
			}
			ctx := context.WithValue(r.Context(), authIdentityCtxKey{}, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// requireScope gates a route group on the authenticated key's scope. When
// auth is disabled (no API keys configured — dev only), every scope check
// passes; when auth is enabled, a request that somehow reaches a scoped
// route without an identity is rejected (defense in depth — the auth
// middleware should already have 401'd it).
func (s *Server) requireScope(required Scope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !s.authEnabled {
				next.ServeHTTP(w, r)
				return
			}
			id, ok := identityFrom(r.Context())
			if !ok {
				httpx.Error(w, bizcode.New(10101, "unauthenticated"))
				return
			}
			if !id.Scope.allows(required) {
				httpx.Error(w, bizcode.New(10150, fmt.Sprintf("api key %q (scope %s) lacks required scope %s", id.Name, id.Scope, required)))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// extractBearer parses a "Bearer <token>" header. Returns the raw token bytes.
func extractBearer(header string) ([]byte, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) {
		return nil, false
	}
	if !strings.EqualFold(header[:len(prefix)], prefix) {
		return nil, false
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return nil, false
	}
	return []byte(token), true
}

// matchAnyKey returns the configured key matching provided, comparing in
// constant time. Every key is compared to avoid early-exit timing leaks.
func matchAnyKey(provided []byte, keys []APIKey) (APIKey, bool) {
	matched := -1
	for i := range keys {
		if subtle.ConstantTimeCompare(provided, keys[i].Secret) == 1 {
			matched = i
		}
	}
	if matched < 0 {
		return APIKey{}, false
	}
	return keys[matched], true
}

// parseAPIKeys parses the API_KEYS env value: comma-separated
// name:scope:secret triples, e.g.
//
//	API_KEYS="ops:admin:s3cr3t,app:write:t0k3n,report:read:r34d"
//
// Names must be unique (they identify the caller in audit logs); scopes are
// read|write|admin; secrets must be non-empty and must not contain ':' or ','.
func parseAPIKeys(raw string) ([]APIKey, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	seen := make(map[string]struct{})
	parts := strings.Split(raw, ",")
	out := make([]APIKey, 0, len(parts))
	for _, p := range parts {
		entry := strings.TrimSpace(p)
		if entry == "" {
			continue
		}
		fields := strings.Split(entry, ":")
		if len(fields) != 3 {
			return nil, fmt.Errorf("server: malformed API_KEYS entry (want name:scope:secret, got %d fields)", len(fields))
		}
		name := strings.TrimSpace(fields[0])
		scope, err := ParseScope(strings.TrimSpace(fields[1]))
		if err != nil {
			return nil, err
		}
		secret := strings.TrimSpace(fields[2])
		if name == "" || secret == "" {
			return nil, fmt.Errorf("server: API_KEYS entry has empty name or secret")
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("server: duplicate API_KEYS name %q", name)
		}
		seen[name] = struct{}{}
		out = append(out, APIKey{Name: name, Scope: scope, Secret: []byte(secret)})
	}
	return out, nil
}
