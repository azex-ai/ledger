// Package server: middleware_auth.go
// Bearer-token API key authentication for mutating HTTP methods.
package server

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/azex-ai/ledger/pkg/bizcode"
	"github.com/azex-ai/ledger/pkg/httpx"
)

// authMiddleware enforces bearer-token API key auth on mutating requests.
// Mutating methods (POST/PUT/PATCH/DELETE) require Authorization: Bearer <key>
// matching one of the configured keys; constant-time compared via crypto/subtle.
// GET/HEAD/OPTIONS pass through unauthenticated.
// Future: per-key holder scoping for read endpoints.
func authMiddleware(keys [][]byte) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isMutating(r.Method) {
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

// isMutating reports whether the HTTP method is a state-changing one.
// OPTIONS is handled by the CORS middleware before auth runs.
func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
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
