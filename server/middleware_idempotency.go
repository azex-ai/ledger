// Package server: middleware_idempotency.go
// Idempotency-Key header alias (api-contract §9).
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/azex-ai/ledger/pkg/httpx"
)

// idempotencyHeaderAliasMiddleware lets clients pass the idempotency key as an
// `Idempotency-Key` header instead of (or in addition to) the body field.
//
// The body field stays the source of truth — the ledger's "same key + same
// payload replays, same key + different payload conflicts" comparison is
// bound to the persisted body. The header is an alias at the HTTP boundary:
//
//   - header set, body field absent/empty → the header value is injected into
//     the body before decoding (top-level "idempotency_key"; for template
//     execution, the nested "params" object)
//   - header and body both set and equal → pass through
//   - header and body disagree → 400, never a silent pick
//
// Webhook callbacks are exempt: their HMAC signature covers the raw body,
// which therefore must not be rewritten (and channel payloads carry no
// idempotency_key field — replay protection is the nonce cache).
func idempotencyHeaderAliasMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" || r.Method != http.MethodPost ||
			strings.HasPrefix(r.URL.Path, "/api/v1/webhooks/") {
			next.ServeHTTP(w, r)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			httpx.Error(w, httpx.ErrBadRequest("read body failed"))
			return
		}
		_ = r.Body.Close()

		var m map[string]any
		if len(bytes.TrimSpace(body)) == 0 {
			m = map[string]any{}
		} else if err := json.Unmarshal(body, &m); err != nil {
			// Not a JSON object — let the handler produce its own decode error.
			r.Body = io.NopCloser(bytes.NewReader(body))
			next.ServeHTTP(w, r)
			return
		}

		// Template execution nests the key under "params"; everything else
		// uses the top-level field.
		target := m
		if params, ok := m["params"].(map[string]any); ok {
			target = params
		}

		if existing, _ := target["idempotency_key"].(string); existing != "" {
			if existing != key {
				httpx.Error(w, httpx.ErrBadRequest("Idempotency-Key header and body idempotency_key disagree"))
				return
			}
		} else {
			target["idempotency_key"] = key
		}

		rewritten, err := json.Marshal(m)
		if err != nil {
			httpx.Error(w, httpx.ErrBadRequest("invalid request body"))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(rewritten))
		r.ContentLength = int64(len(rewritten))
		next.ServeHTTP(w, r)
	})
}
