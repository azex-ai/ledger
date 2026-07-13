package server_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/channel"
	chanOnchain "github.com/azex-ai/ledger/channel/onchain"
	"github.com/azex-ai/ledger/core"
)

var webhookTestKey = []byte("test-webhook-signing-key-0123456789")

// signedWebhookRequest builds a POST /api/v1/webhooks/{channel} request
// carrying a valid HMAC signature over "<timestamp>.<body>" -- mirrors
// channel/onchain.EVMAdapter.VerifySignature.
func signedWebhookRequest(srv http.Handler, channelName string, key []byte, body []byte) *httptest.ResponseRecorder {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/"+channelName, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Timestamp", ts)
	req.Header.Set("X-Signature", sig)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

// recordingBooker pins whether the legacy booking_uid transition path was
// ever reached, regardless of which branch of handleWebhookCallback ran.
type recordingBooker struct {
	transitionCalled bool
	transitionErr    error
}

func (b *recordingBooker) CreateBooking(ctx context.Context, input core.CreateBookingInput) (*core.Booking, error) {
	return nil, core.ErrNotFound
}

func (b *recordingBooker) Transition(ctx context.Context, input core.TransitionInput) (*core.Event, error) {
	b.transitionCalled = true
	if b.transitionErr != nil {
		return nil, b.transitionErr
	}
	return &core.Event{
		UID: "evt-1", BookingUID: input.BookingUID, ToStatus: input.ToStatus,
		OccurredAt: time.Now(),
	}, nil
}

// mockDepositIngester ---------------------------------------------------

type mockDepositIngester struct {
	fn    func(ctx context.Context, s core.DepositSighting) (*core.Booking, error)
	calls int
}

func (m *mockDepositIngester) IngestDeposit(ctx context.Context, s core.DepositSighting) (*core.Booking, error) {
	m.calls++
	return m.fn(ctx, s)
}

// fakeLegacyAdapter implements channel.Adapter (the classic booking_uid
// transition shape) but deliberately NOT sightingParser -- it stands in for
// a hypothetical non-onchain channel (e.g. a bank webhook) to exercise the
// legacy path in handleWebhookCallback.
type fakeLegacyAdapter struct{ bookingUID string }

func (a fakeLegacyAdapter) Name() string                                          { return "legacy" }
func (a fakeLegacyAdapter) VerifySignature(header http.Header, body []byte) error { return nil }
func (a fakeLegacyAdapter) ParseCallback(header http.Header, body []byte) (*channel.CallbackPayload, error) {
	return &channel.CallbackPayload{
		BookingUID:   a.bookingUID,
		ChannelRef:   "ref-1",
		Status:       "confirmed",
		ActualAmount: decimal.RequireFromString("10"),
	}, nil
}

// --- Sighting path (onchain/evm channel) ---

func TestWebhookOnchain_RoutesToSightingIngestion(t *testing.T) {
	rec := &recordingBooker{}
	var captured core.DepositSighting
	ingester := &mockDepositIngester{fn: func(ctx context.Context, s core.DepositSighting) (*core.Booking, error) {
		captured = s
		return &core.Booking{UID: "bk-onchain-1", ClassificationUID: "cls-1", Status: "pending", CreatedAt: time.Now(), UpdatedAt: time.Now()}, nil
	}}

	srv := newTestServerWith(func(o *testServerOpts) {
		o.channels = map[string]channel.Adapter{"evm": chanOnchain.New(webhookTestKey)}
		o.booker = rec
	})
	srv.SetDepositIngester(ingester)

	body := []byte(`{"chain_id":1,"tx_hash":"0xabc123","txlog_seq":0,"token":"0xusdt","from":"0xfrom","to":"0xTo1234","amount":"12.5","confirmations":3,"block_number":1000}`)
	w := signedWebhookRequest(srv, "evm", webhookTestKey, body)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, ingester.calls)
	assert.False(t, rec.transitionCalled, "onchain webhook must not fall through to the legacy booking_uid transition path")
	assert.Equal(t, int64(1), captured.ChainID)
	assert.Equal(t, "0xabc123", captured.TxHash)
	assert.Equal(t, "0xusdt", captured.Token)
	assert.True(t, decimal.RequireFromString("12.5").Equal(captured.Amount))
	assert.Equal(t, int64(1000), captured.BlockNumber)

	data := parseEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "bk-onchain-1", data["uid"])
}

// TestWebhookOnchain_UnregisteredAddress_ReturnsNoOp is M2's regression:
// IngestDeposit returns (nil, nil) for a sighting to an address/token/chain
// this ledger has no business booking (its own doc comment) -- that must
// surface as a 200 no-op, not a panic (bookingToResponse dereferencing a nil
// *core.Booking's first field) that chi's Recoverer would turn into a 500
// the external scanner retries forever.
func TestWebhookOnchain_UnregisteredAddress_ReturnsNoOp(t *testing.T) {
	rec := &recordingBooker{}
	ingester := &mockDepositIngester{fn: func(ctx context.Context, s core.DepositSighting) (*core.Booking, error) {
		return nil, nil // unregistered address -- IngestDeposit's contract for "nothing to do"
	}}
	srv := newTestServerWith(func(o *testServerOpts) {
		o.channels = map[string]channel.Adapter{"evm": chanOnchain.New(webhookTestKey)}
		o.booker = rec
	})
	srv.SetDepositIngester(ingester)

	body := []byte(`{"chain_id":1,"tx_hash":"0xabc123","txlog_seq":0,"token":"0xusdt","from":"0xfrom","to":"0xUnregistered","amount":"12.5","confirmations":3,"block_number":1000}`)

	assert.NotPanics(t, func() {
		w := signedWebhookRequest(srv, "evm", webhookTestKey, body)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		data := parseEnvelope(t, w.Body.Bytes())
		assert.Equal(t, "ignored", data["status"])
	})
	assert.Equal(t, 1, ingester.calls)
	assert.False(t, rec.transitionCalled)
}

func TestWebhookOnchain_NotEnabledWithoutIngester(t *testing.T) {
	rec := &recordingBooker{}
	srv := newTestServerWith(func(o *testServerOpts) {
		o.channels = map[string]channel.Adapter{"evm": chanOnchain.New(webhookTestKey)}
		o.booker = rec
	})
	// SetDepositIngester intentionally never called.

	body := []byte(`{"chain_id":1,"tx_hash":"0xabc","txlog_seq":0,"token":"0xusdt","from":"0xfrom","to":"0xto","amount":"1","confirmations":1}`)
	w := signedWebhookRequest(srv, "evm", webhookTestKey, body)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.False(t, rec.transitionCalled)
}

func TestWebhookOnchain_InvalidSightingBody(t *testing.T) {
	rec := &recordingBooker{}
	ingester := &mockDepositIngester{fn: func(ctx context.Context, s core.DepositSighting) (*core.Booking, error) {
		t.Fatal("must not reach IngestDeposit with an unparseable body")
		return nil, nil
	}}
	srv := newTestServerWith(func(o *testServerOpts) {
		o.channels = map[string]channel.Adapter{"evm": chanOnchain.New(webhookTestKey)}
		o.booker = rec
	})
	srv.SetDepositIngester(ingester)

	w := signedWebhookRequest(srv, "evm", webhookTestKey, []byte(`not json`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.False(t, rec.transitionCalled)
}

// --- Legacy path (non-sighting adapters) ---

func TestWebhookLegacy_TransitionsMatchingDepositBooking(t *testing.T) {
	rec := &recordingBooker{}
	srv := newTestServerWith(func(o *testServerOpts) {
		o.channels = map[string]channel.Adapter{"legacy": fakeLegacyAdapter{bookingUID: "bk-1"}}
		o.booker = rec
		// mockClassificationStore.GetByCode always returns UID "cls-1"
		// regardless of code -- matching this booking's ClassificationUID.
		o.bookingReader = &mockBookingReader{getFn: func(ctx context.Context, uid string) (*core.Booking, error) {
			return &core.Booking{UID: uid, ClassificationUID: "cls-1", ChannelName: "legacy"}, nil
		}}
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/webhooks/legacy", map[string]any{"anything": "x"})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, rec.transitionCalled)
}

func TestWebhookLegacy_RejectsNonDepositClassification(t *testing.T) {
	rec := &recordingBooker{}
	srv := newTestServerWith(func(o *testServerOpts) {
		o.channels = map[string]channel.Adapter{"legacy": fakeLegacyAdapter{bookingUID: "bk-sweep-1"}}
		o.booker = rec
		// A different classification UID than what GetByCode("deposit")
		// resolves to (mockClassificationStore always returns "cls-1") --
		// simulates a `sweep`-classification booking.
		o.bookingReader = &mockBookingReader{getFn: func(ctx context.Context, uid string) (*core.Booking, error) {
			return &core.Booking{UID: uid, ClassificationUID: "sweep-cls-uid", ChannelName: "legacy"}, nil
		}}
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/webhooks/legacy", map[string]any{"anything": "x"})
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.False(t, rec.transitionCalled, "webhook must not be able to transition a non-deposit-classification booking (design doc §5-5)")
}

func TestWebhookLegacy_RejectsChannelMismatch(t *testing.T) {
	rec := &recordingBooker{}
	srv := newTestServerWith(func(o *testServerOpts) {
		o.channels = map[string]channel.Adapter{"legacy": fakeLegacyAdapter{bookingUID: "bk-1"}}
		o.booker = rec
		o.bookingReader = &mockBookingReader{getFn: func(ctx context.Context, uid string) (*core.Booking, error) {
			return &core.Booking{UID: uid, ClassificationUID: "cls-1", ChannelName: "some-other-channel"}, nil
		}}
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/webhooks/legacy", map[string]any{"anything": "x"})
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.False(t, rec.transitionCalled)
}
