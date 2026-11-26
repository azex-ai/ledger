package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/pkg/httpx"
)

// TestIdempotencyAlias_PreservesLargeInt64 pins that the Idempotency-Key
// header alias leaves every other body field value-for-value intact.
//
// The alias used to re-marshal the body through map[string]any, so JSON
// numbers became float64 and an int64 above 2^53 reached the handler as its
// nearest double (9007199254740993 -> 9007199254740992): the request acted on
// a different holder than the client named. MaxInt64 rounded to 2^63 and
// failed decode outright.
func TestIdempotencyAlias_PreservesLargeInt64(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		header string
		holder int64
	}{
		{"above 2^53 with header", `{"account_holder":9007199254740993,"currency_uid":"c","amount":"1"}`, "k", 9007199254740993},
		{"above 2^53 without header", `{"account_holder":9007199254740993,"currency_uid":"c","amount":"1"}`, "", 9007199254740993},
		{"max int64 with header", `{"account_holder":9223372036854775807,"currency_uid":"c","amount":"1"}`, "k", 9223372036854775807},
		{"negative below -2^53 with header", `{"account_holder":-9007199254740993,"currency_uid":"c","amount":"1","idempotency_key":"k"}`, "k", -9007199254740993},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got createReservationRequest
			var decodeErr error
			h := idempotencyHeaderAliasMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got, decodeErr = httpx.Decode[createReservationRequest](r)
			}))

			req := httptest.NewRequest(http.MethodPost, "/api/v1/reservations", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			if tc.header != "" {
				req.Header.Set("Idempotency-Key", tc.header)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			require.NoError(t, decodeErr)
			assert.Equal(t, tc.holder, got.AccountHolder, "holder must reach the handler unchanged")
			if tc.header != "" {
				assert.Equal(t, tc.header, got.IdempotencyKey)
			}
			assert.Equal(t, "c", got.CurrencyUID)
			assert.Equal(t, "1", got.Amount)
		})
	}
}
