package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// TestCreateBooking_RefusesSystemIdempotencyKeys pins that POST /bookings
// cannot claim a key in a namespace the ledger derives its own keys in.
//
// The sweep saga creates its booking under sweep-<chain>-<token>-<nonce>,
// all public values, in the same table-wide UNIQUE namespace POST /bookings
// writes to. A write-scope caller that created any booking under that key
// first made every later sweep tick fail with ErrConflict -- collection for
// the (chain, token) stalled with nothing alerting. Deposit ingest
// (deposit-<chain>-<tx>-<seq>) has the same shape.
func TestCreateBooking_RefusesSystemIdempotencyKeys(t *testing.T) {
	for _, tc := range []struct {
		name, bodyKey, headerKey string
	}{
		{"sweep prefix in body", "sweep-1-0xtoken-7", ""},
		{"deposit prefix in body", "deposit-1-0xab-0", ""},
		{"webhook prefix in body", "webhook-bk-1-confirmed", ""},
		{"sweep prefix via header alias", "", "sweep-1-0xtoken-7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			srv := newTestServerWith(func(o *testServerOpts) {
				o.booker = &mockBooker{createFn: func(context.Context, core.CreateBookingInput) (*core.Booking, error) {
					called = true
					return &core.Booking{UID: "bk-1"}, nil
				}}
			})
			body := map[string]any{
				"classification_code": "deposit",
				"account_holder":      1,
				"currency_uid":        "cur-1",
				"amount":              "1",
				"channel_name":        "manual",
			}
			if tc.bodyKey != "" {
				body["idempotency_key"] = tc.bodyKey
			}
			var w *httptest.ResponseRecorder
			if tc.headerKey != "" {
				w = doRequestWithHeader(srv, http.MethodPost, "/api/v1/bookings", body, "Idempotency-Key", tc.headerKey)
			} else {
				w = doRequest(srv, http.MethodPost, "/api/v1/bookings", body)
			}
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.False(t, called, "a reserved key must be refused before reaching the booker")
		})
	}

	t.Run("ordinary key accepted", func(t *testing.T) {
		var got string
		srv := newTestServerWith(func(o *testServerOpts) {
			o.booker = &mockBooker{createFn: func(_ context.Context, in core.CreateBookingInput) (*core.Booking, error) {
				got = in.IdempotencyKey
				return &core.Booking{UID: "bk-1", Status: "pending", IdempotencyKey: in.IdempotencyKey}, nil
			}}
		})
		ordinary := "6f1c2a52-8f0e-4d43-9d7e-0b8f4f1e2c11" // a client UUID
		w := doRequest(srv, http.MethodPost, "/api/v1/bookings", map[string]any{
			"classification_code": "deposit",
			"account_holder":      1,
			"currency_uid":        "cur-1",
			"amount":              "1",
			"channel_name":        "manual",
			"idempotency_key":     ordinary,
		})
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		assert.Equal(t, ordinary, got)
	})
}
