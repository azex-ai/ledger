package delivery

// Pin for D-M6 (2026-09-02 deep audit): the outbound HMAC key can live outside
// the database, and a deployment still keeping it inside knows that it does.
//
// webhook_subscribers.secret is the last key this schema stores. Migration 007
// revoked exactly that column from ledger_ro, with the reasoning that reading
// it "hands a read-only credential the ability to forge signed event
// deliveries to any subscriber". The identical sentence is true of ledger_app,
// which is the credential the whole threat model assumes is leaked -- so the
// blast radius of a leaked application DB credential runs past this ledger and
// into every downstream system that trusts its events. Migration 014 noted it
// as knowingly not closed and left it there.
//
// Revoking the column is the contract step and belongs to a deployment that
// has moved its keys (deployment.md). This is the expand step, and the pin
// that matters for it is behavioural: with a signer installed, the stored
// secret is not what authenticates the delivery.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

type recordingSigner struct {
	calls  []string // subscriber names
	inputs [][]byte
	mac    []byte
	err    error
}

func (s *recordingSigner) Sign(_ context.Context, subscriberName string, signingInput []byte) ([]byte, error) {
	s.calls = append(s.calls, subscriberName)
	s.inputs = append(s.inputs, append([]byte(nil), signingInput...))
	if s.err != nil {
		return nil, s.err
	}
	return s.mac, nil
}

type collectingLogger struct{ warnings []string }

func (l *collectingLogger) Debug(string, ...any) {}
func (l *collectingLogger) Info(string, ...any)  {}
func (l *collectingLogger) Error(string, ...any) {}
func (l *collectingLogger) Warn(msg string, _ ...any) {
	l.warnings = append(l.warnings, msg)
}

func TestWebhookSigner_ReplacesTheStoredSecret(t *testing.T) {
	ctx := context.Background()
	var gotSignature string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSignature = r.Header.Get("X-Ledger-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	logger := &collectingLogger{}
	signer := &recordingSigner{mac: []byte{0xde, 0xad, 0xbe, 0xef}}
	d := NewWebhookDeliverer(nil, nil, logger, nil).SetSigner(signer)

	// The subscriber row still carries a secret -- a deployment mid-migration
	// has not blanked the column yet. The assertion is that it is not what
	// signs the delivery.
	sub := WebhookSubscriber{ID: 1, Name: "acme", URL: srv.URL, Secret: "the-database-secret", IsActive: true}
	evt := PendingEvent{InternalID: 1, Event: core.Event{UID: "evt-signer-1"}}

	status, err := d.sendHTTP(ctx, evt, sub)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, status)

	require.Equal(t, []string{"acme"}, signer.calls, "the port must be what signs, and it must be told which key to use by name")
	assert.Contains(t, gotSignature, "v1="+hex.EncodeToString(signer.mac))

	// And specifically NOT the stored secret: computing the old signature and
	// confirming it is absent is the difference between "a signer was called"
	// and "the database column stopped being authoritative".
	fromColumn := computeSignature(mustMarshal(t, evt), signatureTimestamp(t, gotSignature), sub.Secret)
	assert.NotContains(t, gotSignature, fromColumn,
		"the delivery must not carry a signature the database column could produce")

	// The signing input is the framing the receiver reconstructs, so a port
	// implementation cannot be written against a different one by accident.
	require.Len(t, signer.inputs, 1)
	assert.True(t, strings.HasPrefix(string(signer.inputs[0]), signatureTimestamp(t, gotSignature)+"."),
		"signing input must be <timestamp>.<payload>, the same bytes the pre-port path signed")

	assert.Empty(t, logger.warnings, "a deployment that has moved its key must not be told it has not")
}

func TestWebhookSigner_FallbackWarnsOnceAndStillWorks(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	logger := &collectingLogger{}
	d := NewWebhookDeliverer(nil, nil, logger, nil) // no signer: the pre-port path

	sub := WebhookSubscriber{ID: 1, Name: "acme", URL: srv.URL, Secret: "the-database-secret", IsActive: true}
	evt := PendingEvent{InternalID: 1, Event: core.Event{UID: "evt-fallback-1"}}

	for i := 0; i < 3; i++ {
		status, err := d.sendHTTP(ctx, evt, sub)
		require.NoError(t, err, "the fallback must keep working: this is expand, not contract")
		assert.Equal(t, http.StatusOK, status)
	}

	require.Len(t, logger.warnings, 1, "warn once -- the condition is configuration, not a property of the request")
	assert.Contains(t, logger.warnings[0], "forge signed deliveries",
		"the warning has to say what the exposure is, not just that a column is in use")
	assert.Contains(t, logger.warnings[0], "SetSigner", "and name the fix")
}

func TestWebhookSigner_FailsClosedRatherThanDeliverUnsigned(t *testing.T) {
	ctx := context.Background()
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sub := WebhookSubscriber{ID: 1, Name: "acme", URL: srv.URL, Secret: "the-database-secret", IsActive: true}
	evt := PendingEvent{InternalID: 1, Event: core.Event{UID: "evt-failclosed-1"}}

	t.Run("signer error", func(t *testing.T) {
		reached = false
		d := NewWebhookDeliverer(nil, nil, &collectingLogger{}, nil).
			SetSigner(&recordingSigner{err: errors.New("kms unavailable")})
		_, err := d.sendHTTP(ctx, evt, sub)
		require.Error(t, err)
		assert.False(t, reached, "a delivery that cannot be signed must not be sent -- a receiver that only checks for the header's presence would accept it")
	})

	t.Run("signer returns nothing", func(t *testing.T) {
		// The failure an implementation is most likely to have: returning
		// (nil, nil) on an unknown subscriber name. Without this check that
		// becomes a delivery signed with the empty string.
		reached = false
		d := NewWebhookDeliverer(nil, nil, &collectingLogger{}, nil).
			SetSigner(&recordingSigner{mac: nil})
		_, err := d.sendHTTP(ctx, evt, sub)
		require.ErrorIs(t, err, core.ErrInvalidInput)
		assert.False(t, reached)
	})

	t.Run("no signer and no stored secret", func(t *testing.T) {
		reached = false
		d := NewWebhookDeliverer(nil, nil, &collectingLogger{}, nil)
		_, err := d.sendHTTP(ctx, evt, WebhookSubscriber{ID: 2, Name: "acme", URL: srv.URL, IsActive: true})
		require.ErrorIs(t, err, core.ErrInvalidInput)
		assert.False(t, reached)
	})
}

func mustMarshal(t *testing.T, evt PendingEvent) []byte {
	t.Helper()
	b, err := json.Marshal(evt.Event)
	require.NoError(t, err)
	return b
}

// signatureTimestamp pulls t=<unix> back out of the X-Ledger-Signature header
// so a test can recompute what the old path would have produced.
func signatureTimestamp(t *testing.T, header string) string {
	t.Helper()
	for _, part := range strings.Split(header, ",") {
		if strings.HasPrefix(part, "t=") {
			return strings.TrimPrefix(part, "t=")
		}
	}
	t.Fatalf("no t= in signature header %q", header)
	return ""
}

// Compile-time proof that computeSignature and the port produce the same shape
// of output, so a consumer's receiver-side verification does not have to know
// which path signed.
var _ = func() bool {
	mac := hmac.New(sha256.New, []byte("k"))
	mac.Write([]byte("1.{}"))
	return len(hex.EncodeToString(mac.Sum(nil))) == len(computeSignature([]byte("{}"), "1", "k"))
}()
