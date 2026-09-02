package delivery

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// TestWebhookPayload_TimesAreUTC is H-M4's pin.
//
// pkg/httpx had a jsoniter extension whose entire reason to exist is "a
// deployment with TZ=Asia/Singapore must not emit +08:00 on an _at field"
// (pgx decodes timestamptz into time.Local and the stdlib marshaller keeps
// that offset). Outbound webhooks marshalled core.Event with encoding/json,
// so the same event went out as `...Z` over HTTP and `...+08:00` to a
// subscriber -- a mechanism implemented correctly and then not connected to
// the second exit. Both now go through internal/wirejson.
//
// Reverting sendHTTP to encoding/json makes this red.
func TestWebhookPayload_TimesAreUTC(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// An event whose OccurredAt carries a +08:00 offset, exactly as pgx
	// hands it back on a TZ=Asia/Singapore deployment.
	sgt := time.FixedZone("SGT", 8*3600)
	event := core.Event{
		UID:                "evt-tz-1",
		ClassificationCode: "deposit",
		BookingUID:         "bk-1",
		AccountHolder:      1,
		CurrencyUID:        "cur-1",
		FromStatus:         "pending",
		ToStatus:           "confirmed",
		Amount:             decimal.RequireFromString("100.5"),
		SettledAmount:      decimal.Zero,
		Metadata:           map[string]string{"k": "v"},
		OccurredAt:         time.Date(2026, 9, 2, 12, 0, 0, 0, sgt),
	}

	poller := &mockEventPoller{events: []PendingEvent{{InternalID: 1, Event: event}}}
	lister := &mockSubscriberLister{subs: []WebhookSubscriber{{ID: 1, Name: "sub", URL: srv.URL, Secret: "sec"}}}
	deliverer := NewWebhookDeliverer(poller, lister, core.NopLogger(), core.NopMetrics())

	delivered, err := deliverer.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, 1, delivered)
	require.NotEmpty(t, body, "subscriber received no body")

	var payload map[string]any
	require.NoError(t, json.Unmarshal(body, &payload), "payload: %s", body)

	occurredAt, ok := payload["occurred_at"].(string)
	require.True(t, ok, "occurred_at must be a string, got %#v", payload["occurred_at"])
	assert.True(t, strings.HasSuffix(occurredAt, "Z"),
		"outbound timestamps are RFC3339 UTC (api-contract.md §5); got %q", occurredAt)
	assert.Equal(t, "2026-09-02T04:00:00Z", occurredAt, "the instant must be preserved, only the zone normalized")

	// Same rules as the HTTP surface for the rest of the payload: string
	// amounts, snake_case names, no internal delivery bookkeeping.
	assert.Equal(t, "100.5", payload["amount"])
	assert.Equal(t, "0", payload["settled_amount"])
	for _, leaked := range []string{"attempts", "max_attempts", "next_attempt_at", "internal_id", "InternalID"} {
		assert.NotContains(t, payload, leaked, "delivery bookkeeping must not reach a subscriber")
	}
}

// TestWebhookPayload_MatchesDocumentedHeaders pins the three delivery
// headers docs/openapi.yaml's OutboundEvent description now documents. They
// were undocumented, and a receiver cannot verify a signature it does not
// know the shape of.
func TestWebhookPayload_MatchesDocumentedHeaders(t *testing.T) {
	var got http.Header
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	poller := &mockEventPoller{events: []PendingEvent{{InternalID: 1, Event: core.Event{UID: "evt-hdr-1", ToStatus: "confirmed"}}}}
	lister := &mockSubscriberLister{subs: []WebhookSubscriber{{ID: 1, Name: "sub", URL: srv.URL, Secret: "top-secret"}}}
	deliverer := NewWebhookDeliverer(poller, lister, core.NopLogger(), core.NopMetrics())

	_, err := deliverer.ProcessBatch(context.Background(), 10)
	require.NoError(t, err)

	assert.Equal(t, "application/json", got.Get("Content-Type"))
	assert.Equal(t, "evt-hdr-1", got.Get("X-Ledger-Event-UID"))
	timestamp := got.Get("X-Ledger-Timestamp")
	require.NotEmpty(t, timestamp)

	sig := got.Get("X-Ledger-Signature")
	require.True(t, strings.HasPrefix(sig, "t="+timestamp+",v1="),
		"signature header is t=<timestamp>,v1=<hex>; got %q", sig)
	assert.Equal(t, "t="+timestamp+",v1="+computeSignature(body, timestamp, "top-secret"), sig,
		"the signature covers <timestamp>.<raw body> with the subscriber's secret")
}
