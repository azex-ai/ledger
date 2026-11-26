package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// TestIdempotencyMiddleware_RefusesSystemKeysOnEveryWriteRoute pins that a
// caller cannot claim an idempotency key in a namespace the ledger derives
// its own keys in, on any write route -- not only POST /bookings.
//
// Idempotency keys are table-wide UNIQUE and the system derives them from
// public values: a write-scope caller that POSTed a transition on its OWN
// booking under deposit-confirmed-<victim uid>, or a journal under
// deposit-confirm-<victim uid>, made the real deposit's confirmation conflict
// forever; expire-booking-<uid> and sweep-<chain>-<token>-<nonce> have the
// same shape. The refusal lives once, in idempotencyHeaderAliasMiddleware,
// for the body member and for the Idempotency-Key header alike.
func TestIdempotencyMiddleware_RefusesSystemKeysOnEveryWriteRoute(t *testing.T) {
	newSrv := func(reached *bool) http.Handler {
		return newTestServerWith(func(o *testServerOpts) {
			o.booker = &mockBooker{
				createFn: func(_ context.Context, in core.CreateBookingInput) (*core.Booking, error) {
					*reached = true
					return &core.Booking{UID: "bk-1", Status: "pending", IdempotencyKey: in.IdempotencyKey}, nil
				},
				transitionFn: func(_ context.Context, in core.TransitionInput) (*core.Event, error) {
					*reached = true
					return &core.Event{UID: "evt-1", BookingUID: in.BookingUID, ToStatus: in.ToStatus, OccurredAt: time.Now()}, nil
				},
			}
			o.journals = &mockJournalWriter{postFn: func(_ context.Context, in core.JournalInput) (*core.Journal, error) {
				*reached = true
				return &core.Journal{UID: "j-1", IdempotencyKey: in.IdempotencyKey}, nil
			}}
		})
	}
	routes := []struct {
		name, path string
		body       func() map[string]any
	}{
		{"create booking", "/api/v1/bookings", func() map[string]any {
			return map[string]any{"classification_code": "deposit", "account_holder": 1, "currency_uid": "cur-1", "amount": "1", "channel_name": "manual"}
		}},
		{"transition", "/api/v1/bookings/bk-own/transition", func() map[string]any {
			return map[string]any{"to_status": "confirmed"}
		}},
		{"post journal", "/api/v1/journals", func() map[string]any {
			return map[string]any{"journal_type_uid": "jt-1", "entries": []map[string]any{
				{"account_holder": 1, "currency_uid": "cur-1", "classification_uid": "cls-1", "entry_type": "debit", "amount": "5"},
				{"account_holder": -1, "currency_uid": "cur-1", "classification_uid": "cls-1", "entry_type": "credit", "amount": "5"},
			}}
		}},
	}
	reserved := []string{
		"deposit-confirmed-bk-victim",
		"deposit-confirm-bk-victim",
		"sweep-1-0xtoken-7",
		"webhook-bk-victim-confirmed-ref",
		"expire-booking-bk-victim",
	}

	for _, rt := range routes {
		for _, key := range reserved {
			for _, via := range []string{"body", "header", "case-variant body member"} {
				t.Run(rt.name+"/"+via+"/"+key, func(t *testing.T) {
					reached := false
					srv := newSrv(&reached)
					body := rt.body()
					var w *httptest.ResponseRecorder
					switch via {
					case "body":
						body["idempotency_key"] = key
						w = doRequest(srv, http.MethodPost, rt.path, body)
					case "header":
						w = doRequestWithHeader(srv, http.MethodPost, rt.path, body, "Idempotency-Key", key)
					default:
						// encoding/json matches struct fields case-insensitively,
						// so this member reaches the handler as the key.
						body["Idempotency_Key"] = key
						w = doRequest(srv, http.MethodPost, rt.path, body)
					}
					require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
					env := decodeErrorEnvelope(t, w.Body.Bytes())
					assert.Contains(t, env.Message.Fields["idempotency_key"], "reserved")
					assert.False(t, reached, "a reserved key must be refused before reaching the store")
				})
			}
		}

		t.Run(rt.name+"/ordinary UUID passes", func(t *testing.T) {
			ordinary := "6f1c2a52-8f0e-4d43-9d7e-0b8f4f1e2c11"
			reached := false
			srv := newSrv(&reached)
			body := rt.body()
			body["idempotency_key"] = ordinary
			w := doRequest(srv, http.MethodPost, rt.path, body)
			assert.Less(t, w.Code, 300, w.Body.String())
			assert.True(t, reached, "body key")

			reached = false
			w = doRequestWithHeader(srv, http.MethodPost, rt.path, rt.body(), "Idempotency-Key", ordinary)
			assert.Less(t, w.Code, 300, w.Body.String())
			assert.True(t, reached, "header key")
		})
	}

	t.Run("webhook route is not affected", func(t *testing.T) {
		srv := newTestServer()
		w := doRequestWithHeader(srv, http.MethodPost, "/api/v1/webhooks/nochannel",
			map[string]any{"idempotency_key": "deposit-confirmed-bk-1"}, "Idempotency-Key", "sweep-1-0xt-1")
		// Reaches the webhook handler, which answers for the unknown channel.
		assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	})
}

// TestIdempotencyMiddleware_NullBodyWithHeader pins that a literal null body
// with an Idempotency-Key header no longer panics: json.Unmarshal leaves the
// map nil and the header injection assigned into it (500 via Recoverer).
// /api/v1/holder/ is exempt from API-key auth, so that was reachable without
// a key. A null body is now treated like an empty one: the handler sees only
// the injected key and answers with its own error.
func TestIdempotencyMiddleware_NullBodyWithHeader(t *testing.T) {
	var got core.JournalInput
	srv := newTestServerWith(func(o *testServerOpts) {
		o.journals = &mockJournalWriter{postFn: func(_ context.Context, in core.JournalInput) (*core.Journal, error) {
			got = in
			return nil, in.Validate()
		}}
	})
	for _, path := range []string{"/api/v1/journals", "/api/v1/holder/deposit-address"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("null"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "k-1")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		assert.NotEqual(t, http.StatusInternalServerError, w.Code, "%s: %s", path, w.Body.String())
		assert.GreaterOrEqual(t, w.Code, 400, path)
	}
	assert.Equal(t, "k-1", got.IdempotencyKey, "the header key is injected into the null-turned-empty body")
}

// TestIdempotencyMiddleware_TrailingBytesDoNotBypassReservedKeyCheck pins the
// parser agreement between the middleware and httpx.Decode. The handlers
// decode with a json.Decoder that reads only the FIRST JSON value; the
// middleware used json.Unmarshal, which rejects trailing data, so a body like
// `{"idempotency_key":"deposit-confirmed-<victim>"} x` failed the middleware's
// parse, passed through unchecked as "not a JSON object", and the handler
// still decoded the reserved key from the first value.
func TestIdempotencyMiddleware_TrailingBytesDoNotBypassReservedKeyCheck(t *testing.T) {
	newSrv := func(reached *bool, gotKey *string) http.Handler {
		return newTestServerWith(func(o *testServerOpts) {
			o.booker = &mockBooker{
				createFn: func(_ context.Context, in core.CreateBookingInput) (*core.Booking, error) {
					*reached, *gotKey = true, in.IdempotencyKey
					return &core.Booking{UID: "bk-1", Status: "pending", IdempotencyKey: in.IdempotencyKey}, nil
				},
				transitionFn: func(_ context.Context, in core.TransitionInput) (*core.Event, error) {
					*reached, *gotKey = true, in.IdempotencyKey
					return &core.Event{UID: "evt-1", BookingUID: in.BookingUID, ToStatus: in.ToStatus, OccurredAt: time.Now()}, nil
				},
			}
			o.journals = &mockJournalWriter{postFn: func(_ context.Context, in core.JournalInput) (*core.Journal, error) {
				*reached, *gotKey = true, in.IdempotencyKey
				return &core.Journal{UID: "j-1", IdempotencyKey: in.IdempotencyKey}, nil
			}}
		})
	}
	// Each body is a JSON object with the key spliced in as its last member.
	routes := []struct{ name, path, prefix string }{
		{"create booking", "/api/v1/bookings",
			`{"classification_code":"deposit","account_holder":1,"currency_uid":"cur-1","amount":"1","channel_name":"manual"`},
		{"transition", "/api/v1/bookings/bk-own/transition", `{"to_status":"confirmed"`},
		{"post journal", "/api/v1/journals",
			`{"journal_type_uid":"jt-1","entries":[` +
				`{"account_holder":1,"currency_uid":"cur-1","classification_uid":"cls-1","entry_type":"debit","amount":"5"},` +
				`{"account_holder":-1,"currency_uid":"cur-1","classification_uid":"cls-1","entry_type":"credit","amount":"5"}]`},
	}
	trailers := []string{" x", "}", "{}", ` {"idempotency_key":"ok"}`}
	send := func(srv http.Handler, path, body, header string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if header != "" {
			req.Header.Set("Idempotency-Key", header)
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w
	}

	for _, rt := range routes {
		for _, tr := range trailers {
			t.Run(rt.name+"/reserved key refused/trailer "+strconv.Quote(tr), func(t *testing.T) {
				reached, got := false, ""
				w := send(newSrv(&reached, &got), rt.path,
					rt.prefix+`,"idempotency_key":"deposit-confirmed-bk-victim"}`+tr, "")
				require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
				env := decodeErrorEnvelope(t, w.Body.Bytes())
				assert.Contains(t, env.Message.Fields["idempotency_key"], "reserved")
				assert.False(t, reached, "a reserved key must be refused before reaching the store")
			})

			t.Run(rt.name+"/ordinary body key unchanged/trailer "+strconv.Quote(tr), func(t *testing.T) {
				// No header: the original bytes reach the handler, which
				// decodes the first value as it always did.
				reached, got := false, ""
				w := send(newSrv(&reached, &got), rt.path,
					rt.prefix+`,"idempotency_key":"k-ordinary"}`+tr, "")
				assert.Less(t, w.Code, 300, w.Body.String())
				assert.True(t, reached)
				assert.Equal(t, "k-ordinary", got)
			})

			t.Run(rt.name+"/header key injected, trailer dropped/trailer "+strconv.Quote(tr), func(t *testing.T) {
				// Header present, body member absent: the rewrite re-emits
				// the first value plus the key and drops the trailing bytes,
				// which the handler's decoder never read anyway.
				reached, got := false, ""
				w := send(newSrv(&reached, &got), rt.path, rt.prefix+`}`+tr, "k-header")
				assert.Less(t, w.Code, 300, w.Body.String())
				assert.True(t, reached)
				assert.Equal(t, "k-header", got)
			})
		}
	}
}
