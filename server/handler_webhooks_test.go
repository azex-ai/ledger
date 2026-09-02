package server_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

// newWebhookEVMAdapter builds the evm channel adapter for tests, failing fast
// if the (sufficiently long) test key is rejected.
func newWebhookEVMAdapter(t *testing.T) channel.Adapter {
	t.Helper()
	a, err := chanOnchain.New(webhookTestKey)
	require.NoError(t, err)
	return a
}

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
type fakeLegacyAdapter struct {
	bookingUID string
	// status is the callback's claimed target state. Parameterized as of
	// G-m5: "confirmed" is now refused on this path (confirmation posts
	// accounting and has exactly one producer), so a test exercising the
	// legitimate legacy flow has to pick a state that is not it, and the
	// test exercising the refusal has to pick one that is. It used to be
	// hardcoded to "confirmed", which meant the happy-path test was
	// asserting the forbidden case succeeded.
	status string
}

func (a fakeLegacyAdapter) Name() string                                          { return "legacy" }
func (a fakeLegacyAdapter) VerifySignature(header http.Header, body []byte) error { return nil }
func (a fakeLegacyAdapter) ParseCallback(header http.Header, body []byte) (*channel.CallbackPayload, error) {
	status := a.status
	if status == "" {
		status = "confirming"
	}
	return &channel.CallbackPayload{
		BookingUID:   a.bookingUID,
		ChannelRef:   "ref-1",
		Status:       status,
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
		o.channels = map[string]channel.Adapter{"evm": newWebhookEVMAdapter(t)}
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
		o.channels = map[string]channel.Adapter{"evm": newWebhookEVMAdapter(t)}
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
		o.channels = map[string]channel.Adapter{"evm": newWebhookEVMAdapter(t)}
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
		o.channels = map[string]channel.Adapter{"evm": newWebhookEVMAdapter(t)}
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

// TestWebhookLegacy_RefusesToConfirmADeposit pins G-m5 (2026-09-02 deep audit).
//
// I-21 says a confirmed deposit's accounting comes from exactly one place --
// service's postDepositConfirmedJournal, which posts the journal in the same
// transaction as the transition. That was true of the service layer and never
// true of the HTTP layer: the legacy ParseCallback branch handed ToStatus
// straight to Booker.Transition, which moves the booking to terminal
// `confirmed` and posts nothing. The result is a deposit every holder-facing
// surface calls settled, with no entries behind it and no journal for
// reconciliation to find missing.
//
// Not reachable through this repository's own adapter -- channel/onchain
// implements SightingParser and is routed to IngestDeposit before this branch
// -- which is exactly why it survived: the path belongs to a consumer's own
// channel.Adapter, and a consumer's adapter is the thing the classification
// confinement immediately above it already exists to defend against.
func TestWebhookLegacy_RefusesToConfirmADeposit(t *testing.T) {
	rec := &recordingBooker{}
	srv := newTestServerWith(func(o *testServerOpts) {
		o.channels = map[string]channel.Adapter{"legacy": fakeLegacyAdapter{bookingUID: "bk-1", status: "confirmed"}}
		o.booker = rec
		o.bookingReader = &mockBookingReader{getFn: func(ctx context.Context, uid string) (*core.Booking, error) {
			// A deposit booking mid-flight: passes both the channel-ownership
			// and classification-confinement checks, so this test fails on the
			// new rule and nothing else.
			return &core.Booking{UID: uid, ClassificationUID: "cls-1", ChannelName: "legacy", Status: "confirming"}, nil
		}}
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/webhooks/legacy", map[string]any{"anything": "x"})
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.False(t, rec.transitionCalled,
		"a channel callback must not move a deposit to a terminal confirmed state: that transition is supposed to carry a journal, and this path posts none")
}

// TestWebhookLegacy_StillAllowsNonTerminalProgress is the other half: the
// refusal above must be about `confirmed` specifically, not about the legacy
// path. A fix that closed the path entirely would break every consumer whose
// channel reports intermediate progress, and would look identical in a test
// that only asserted the 403.
func TestWebhookLegacy_StillAllowsNonTerminalProgress(t *testing.T) {
	rec := &recordingBooker{}
	srv := newTestServerWith(func(o *testServerOpts) {
		o.channels = map[string]channel.Adapter{"legacy": fakeLegacyAdapter{bookingUID: "bk-1", status: "confirming"}}
		o.booker = rec
		o.bookingReader = &mockBookingReader{getFn: func(ctx context.Context, uid string) (*core.Booking, error) {
			return &core.Booking{UID: uid, ClassificationUID: "cls-1", ChannelName: "legacy", Status: "pending"}, nil
		}}
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/webhooks/legacy", map[string]any{"anything": "x"})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, rec.transitionCalled)
}

// TestWebhookCallback_WarnsWhenNonceRecorderMissing pins D-M8's first half
// (2026-09-02 deep audit): "replay protection is off" is observable.
//
// The nonce cache was implemented, migration 002 granted it the DELETE it
// needs, and then nothing wired it -- SetWebhookNonceRecorder had zero callers
// outside its own definition, in the library, the examples, the README and the
// docs alike. So every deployment built from this repository's own
// instructions ran without it, and a callback replayed inside the signature's
// ±5 minute window verified and reached ingestion. Not a bug in the cache: a
// capability that existed in the library and was absent from every path a
// consumer walks, which is the shape this audit round found four times.
//
// What made it survive two rounds is that it was silent. Nothing about the
// running system distinguished "on" from "off", so no operator could discover
// it and no test could notice it. That is what this asserts.
func TestWebhookCallback_WarnsWhenNonceRecorderMissing(t *testing.T) {
	rec := &recordingBooker{}
	srv := newTestServerWith(func(o *testServerOpts) {
		o.channels = map[string]channel.Adapter{"legacy": fakeLegacyAdapter{bookingUID: "bk-1"}}
		o.booker = rec
		o.bookingReader = &mockBookingReader{getFn: func(ctx context.Context, uid string) (*core.Booking, error) {
			return &core.Booking{UID: uid, ClassificationUID: "cls-1", ChannelName: "legacy"}, nil
		}}
	})
	// SetWebhookNonceRecorder deliberately never called -- this is the state
	// every example and README assembly produced.

	logs := captureSlog(t)
	w := doRequest(srv, http.MethodPost, "/api/v1/webhooks/legacy", map[string]any{"anything": "x"})
	require.Equal(t, http.StatusOK, w.Code, "the callback must still be served: this is a degraded mode, not a refusal")

	out := logs.String()
	assert.Contains(t, out, "replay cache is NOT configured",
		"an unprotected callback must say so; before this, 'protection on' and 'protection off' had identical runtime output")
	assert.Contains(t, out, "SetWebhookNonceRecorder",
		"and it must name the call that fixes it -- an operator reading the log has no other pointer")

	// Once per server, not per callback: the recorder cannot appear
	// mid-flight, so a line per inbound webhook would bury the signal. Counted
	// by occurrences of the message rather than by buffer length -- the
	// request logger writes a line per request either way.
	doRequest(srv, http.MethodPost, "/api/v1/webhooks/legacy", map[string]any{"anything": "y"})
	doRequest(srv, http.MethodPost, "/api/v1/webhooks/legacy", map[string]any{"anything": "z"})
	assert.Equal(t, 1, strings.Count(logs.String(), "replay cache is NOT configured"),
		"the warning must not repeat per request")
}

// TestWebhookCallback_ConfiguredRecorderIsQuietAndRejectsReplays is the other
// side. A warning that fires when the cache IS configured would be noise, and
// a cache that is wired but not consulted would pass a test that only looked
// at logs -- so this asserts both the silence and the rejection.
func TestWebhookCallback_ConfiguredRecorderIsQuietAndRejectsReplays(t *testing.T) {
	rec := &recordingBooker{}
	srv := newTestServerWith(func(o *testServerOpts) {
		o.channels = map[string]channel.Adapter{"legacy": fakeLegacyAdapter{bookingUID: "bk-1"}}
		o.booker = rec
		o.bookingReader = &mockBookingReader{getFn: func(ctx context.Context, uid string) (*core.Booking, error) {
			return &core.Booking{UID: uid, ClassificationUID: "cls-1", ChannelName: "legacy"}, nil
		}}
	})
	nonces := &fakeNonceRecorder{seen: map[string]bool{}}
	srv.SetWebhookNonceRecorder(nonces)

	logs := captureSlog(t)
	first := doRequest(srv, http.MethodPost, "/api/v1/webhooks/legacy", map[string]any{"anything": "x"})
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())

	replay := doRequest(srv, http.MethodPost, "/api/v1/webhooks/legacy", map[string]any{"anything": "x"})
	assert.Equal(t, http.StatusConflict, replay.Code,
		"an identical callback inside the signature window verifies; the nonce cache is the only thing that rejects it")

	assert.NotContains(t, logs.String(), "replay cache is NOT configured",
		"a configured recorder must produce no warning, or the warning stops meaning anything")
}

// fakeNonceRecorder is an in-memory server.WebhookNonceRecorder.
type fakeNonceRecorder struct{ seen map[string]bool }

func (f *fakeNonceRecorder) TryRecordNonce(_ context.Context, nonce string) (bool, error) {
	if f.seen[nonce] {
		return false, nil
	}
	f.seen[nonce] = true
	return true, nil
}

// captureSlog redirects the default slog logger into a buffer for the duration
// of a test, restoring it afterwards. The server's startup and degradation
// warnings go to slog.Default() (server.go's own convention), so asserting on
// an observability claim means reading it back from there.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}
