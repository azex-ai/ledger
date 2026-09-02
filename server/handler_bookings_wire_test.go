package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// TestBookingWire_NoExpiryEmitsNull is H-M2's pin, taken over the real HTTP
// path (chi router + httpx's encoder, not encoding/json on the struct) so it
// covers the encoder the wire actually uses.
//
// A booking created without an expiry stores 'epoch' and reads back as the
// zero time. bookingResponse.ExpiresAt used to be a plain string left at ""
// in that case, while docs/openapi.yaml declared `format: date-time` and
// listed it in `required` -- so the ordinary path of an unexpiring booking
// produced a value no date parser accepts (`new Date("")` -> Invalid Date,
// `time.Parse(time.RFC3339, "")` -> error) and no generated client can
// model. Reverting ExpiresAt to a bare string makes this red.
func TestBookingWire_NoExpiryEmitsNull(t *testing.T) {
	srv := newTestServer()

	// mockBookingReader's default booking leaves ExpiresAt at the zero time.
	w := doRequest(srv, http.MethodGet, "/api/v1/bookings/bk-1", nil)
	require.Equal(t, http.StatusOK, w.Code)

	// Assert on the raw body: the distinction between a literal null, an
	// absent key and "" disappears once the JSON is decoded into `any`.
	body := w.Body.String()
	assert.Contains(t, body, `"expires_at":null`,
		"a booking with no expiry must serialize expires_at as literal null, never \"\" or an absent key")
	assert.NotContains(t, body, `"expires_at":""`)

	data := parseEnvelope(t, w.Body.Bytes())
	require.Contains(t, data, "expires_at", "expires_at is in the spec's `required` list: the key is always present")
	assert.Nil(t, data["expires_at"])
}

// TestBookingWire_ExpiryEmitsRFC3339UTC is the other half: when there IS an
// expiry the wire value is an RFC3339 instant with a Z offset (api-contract
// §5), not a local-zone rendering.
func TestBookingWire_ExpiryEmitsRFC3339UTC(t *testing.T) {
	expires := time.Date(2026, 9, 2, 12, 0, 0, 0, time.FixedZone("SGT", 8*3600))
	srv := newTestServerWith(func(o *testServerOpts) {
		o.bookingReader = &mockBookingReader{
			getFn: func(_ context.Context, uid string) (*core.Booking, error) {
				return &core.Booking{
					UID: uid, ClassificationUID: "cls-1", AccountHolder: 100,
					CurrencyUID: "cur-1", Amount: decimal.NewFromInt(500), Status: "pending",
					ChannelName: "crypto", IdempotencyKey: "op-1",
					ExpiresAt: expires,
					CreatedAt: time.Now(), UpdatedAt: time.Now(),
				}, nil
			},
		}
	})

	w := doRequest(srv, http.MethodGet, "/api/v1/bookings/bk-1", nil)
	require.Equal(t, http.StatusOK, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	got, ok := data["expires_at"].(string)
	require.True(t, ok, "expires_at should be a string when the booking expires, got %#v", data["expires_at"])
	assert.Equal(t, "2026-09-02T04:00:00Z", got)
	assert.True(t, strings.HasSuffix(got, "Z"), "timestamps cross this API as UTC")
}

// TestBookingWire_MetadataIsStringMap pins the metadata value type the spec
// now declares (additionalProperties: {type: string}) against what the Go
// type can actually hold: map[string]string, never nested JSON.
func TestBookingWire_MetadataIsStringMap(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/bookings/bk-1", nil)
	require.Equal(t, http.StatusOK, w.Code)

	var envelope struct {
		Data struct {
			Metadata map[string]json.RawMessage `json:"metadata"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	for k, v := range envelope.Data.Metadata {
		var s string
		require.NoError(t, json.Unmarshal(v, &s), "metadata[%q] must be a JSON string", k)
	}
}
