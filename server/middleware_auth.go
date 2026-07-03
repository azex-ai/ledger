// Package server: middleware_auth.go
// Bearer-token API key authentication for every endpoint (reads included).
package server

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/azex-ai/ledger/pkg/bizcode"
	"github.com/azex-ai/ledger/pkg/httpx"
)

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
// constant-time compared via crypto/subtle. (OPTIONS is handled by the CORS
// middleware before auth runs.)
// Future: per-key holder scoping so one key sees only its own holders.
func authMiddleware(keys [][]byte) func(http.Handler) http.Handler {
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

			if !matchAnyKey(provided, keys) {
				httpx.Error(w, bizcode.New(10101, "invalid api key"))
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

// matchAnyKey returns true if provided matches any configured key in
// constant time. We compare against every key to avoid early-exit timing leaks.
func matchAnyKey(provided []byte, keys [][]byte) bool {
	matched := 0
	for _, k := range keys {
		if subtle.ConstantTimeCompare(provided, k) == 1 {
			matched = 1
		}
	}
	return matched == 1
}

// parseAPIKeys splits a comma-separated env value into trimmed, non-empty keys.
func parseAPIKeys(raw string) [][]byte {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([][]byte, 0, len(parts))
	for _, p := range parts {
		k := strings.TrimSpace(p)
		if k == "" {
			continue
		}
		out = append(out, []byte(k))
	}
	return out
}
