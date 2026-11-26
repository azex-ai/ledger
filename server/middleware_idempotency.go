// Package server: middleware_idempotency.go
// Idempotency-Key header alias (api-contract §9).
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/httpx"
)

// idempotencyHeaderAliasMiddleware lets clients pass the idempotency key as an
// `Idempotency-Key` header instead of (or in addition to) the body field, and
// it is the one place a caller-supplied key in a system-derived namespace
// (core.IsSystemIdempotencyKey) is refused.
//
// The body field stays the source of truth — the ledger's "same key + same
// payload replays, same key + different payload conflicts" comparison is
// bound to the persisted body. The header is an alias at the HTTP boundary
// (POST only):
//
//   - header set, body field absent/empty → the header value is injected into
//     the body before decoding, always at the top level
//   - header and body both set and equal → pass through
//   - header and body disagree → 400, never a silent pick
//
// Reserved namespaces: idempotency keys are table-wide UNIQUE, and the
// deposit add-on, the webhook handler, the expiration worker, the
// pending-timeout sweeper and ReverseJournal derive their keys from public
// or enumerable values (deposit-confirmed-<booking uid>,
// expire-booking-<uid>, sweep-<chain>-<token>-<nonce>,
// pending:expire:journal=<id>, reversal:<journal uid>:<reason>, ...). A caller that
// wrote anything under such a key first -- a transition on its own booking,
// a journal -- would make the system's own write conflict forever. So every
// mutating request (POST/PUT/PATCH/DELETE) carrying a reserved key, in the
// header or in any top-level body member named idempotency_key
// (case-insensitively, as the handlers' decoder matches it), gets the normal
// 400 field error. The internal paths (webhook handler, dead-letter replay,
// onchain service, expiration worker) call the stores directly and never
// pass through here.
//
// Webhook callbacks are exempt: their HMAC signature covers the raw body,
// which therefore must not be rewritten (and channel payloads carry no
// idempotency_key field — replay protection is the nonce cache; the key the
// webhook handler writes under is derived server-side).
func idempotencyHeaderAliasMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// webhookPathPrefix, not a second copy of the literal: this exemption
		// and authMiddleware's have to name the same set of paths, and they
		// were two independent strings. Editing one and not the other yields
		// either an unauthenticated route whose body gets rewritten (breaking
		// the HMAC the exemption exists to protect) or an authenticated one
		// that skips the alias -- neither of which any test would have caught.
		if !isMutatingMethod(r.Method) || strings.HasPrefix(r.URL.Path, webhookPathPrefix) {
			next.ServeHTTP(w, r)
			return
		}

		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if core.IsSystemIdempotencyKey(key) {
			httpx.Error(w, errReservedIdempotencyKey())
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			httpx.Error(w, httpx.ErrBadRequest("read body failed"))
			return
		}
		_ = r.Body.Close()
		// passThrough hands the handler the original bytes, untouched.
		passThrough := func() {
			r.Body = io.NopCloser(bytes.NewReader(body))
			next.ServeHTTP(w, r)
		}

		// json.RawMessage values, not map[string]any: an untyped decode turns
		// every JSON number into a float64, so an int64 above 2^53 (a holder
		// id, an actor id) came back out of json.Marshal as its nearest
		// double -- the request reached the handler naming a different
		// holder than the client sent. Raw values are re-emitted unchanged;
		// only the idempotency_key member is ever touched.
		//
		// Decoded exactly as httpx.Decode does -- a json.Decoder reading the
		// FIRST JSON value only, trailing bytes unread -- not json.Unmarshal,
		// which fails on anything after the value. With Unmarshal,
		// `{"idempotency_key":"deposit-confirmed-<uid>"} x` failed here,
		// passed through as "not a JSON object", and the handler's decoder
		// still read the reserved key out of the first value. The member
		// checked below must be the member the handler will see.
		var m map[string]json.RawMessage
		if len(bytes.TrimSpace(body)) == 0 {
			m = map[string]json.RawMessage{}
		} else if err := json.NewDecoder(bytes.NewReader(body)).Decode(&m); err != nil {
			// First value is not a JSON object (or not JSON at all) — let the
			// handler produce its own decode error.
			passThrough()
			return
		}
		// A literal `null` body unmarshals into a nil map with no error and
		// is treated like an empty body; the header injection below used to
		// panic on the nil map.
		if m == nil {
			m = map[string]json.RawMessage{}
		}

		for name, raw := range m {
			if !strings.EqualFold(name, "idempotency_key") {
				continue
			}
			var v string
			if json.Unmarshal(raw, &v) == nil && core.IsSystemIdempotencyKey(v) {
				httpx.Error(w, errReservedIdempotencyKey())
				return
			}
		}

		if key == "" || r.Method != http.MethodPost {
			// No alias to apply.
			passThrough()
			return
		}

		// Always the top level (H-m10). This used to redirect the injection
		// into a nested "params" object whenever the body happened to have
		// one, for a template-execution shape that no longer exists:
		// postTemplateRequest carries idempotency_key flat, and no route on
		// this surface reads a nested params.idempotency_key. The branch was
		// unconditional, so ANY body with a top-level object named "params"
		// got the header buried where its handler does not look, and the
		// request failed with "idempotency_key is required" -- a mine armed
		// by a field-name collision. A future batch endpoint that genuinely
		// nests the key gets an explicit per-route rule, not a guess from
		// the body's shape.
		target := m

		var existing string
		if raw, ok := target["idempotency_key"]; ok {
			if err := json.Unmarshal(raw, &existing); err != nil {
				// A non-string body value counts as absent and is replaced
				// by the header, as the untyped type assertion did before.
				existing = ""
			}
		}
		if existing != "" {
			if existing != key {
				httpx.Error(w, httpx.ErrBadRequest("Idempotency-Key header and body idempotency_key disagree"))
				return
			}
		} else {
			encodedKey, err := json.Marshal(key)
			if err != nil {
				httpx.Error(w, httpx.ErrBadRequest("invalid Idempotency-Key header"))
				return
			}
			target["idempotency_key"] = encodedKey
		}

		// The rewrite re-emits the first JSON value only; any bytes after it
		// are dropped. The handler's decoder never reads past the first
		// value either, so it decodes the same members it would have from
		// the original body, plus the injected key.
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

func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

func errReservedIdempotencyKey() error {
	return httpx.ErrField("idempotency_key", "the prefixes "+
		strconv.Quote(core.IdempotencyPrefixDeposit)+", "+
		strconv.Quote(core.IdempotencyPrefixSweep)+", "+
		strconv.Quote(core.IdempotencyPrefixWebhook)+", "+
		strconv.Quote(core.IdempotencyPrefixExpire)+", "+
		strconv.Quote(core.IdempotencyPrefixPendingExpire)+" and "+
		strconv.Quote(core.IdempotencyPrefixReversal)+
		" are reserved for keys the ledger derives itself")
}
