package server_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/pkg/slogadapter"
	"github.com/azex-ai/ledger/server"
)

// errorEnvelope is the failure half of api-contract.md §1: message and data
// are mutually exclusive, and message is an object with text plus optional
// per-field detail.
type errorEnvelope struct {
	Code    int `json:"code"`
	Message struct {
		Text   string            `json:"text"`
		Fields map[string]string `json:"fields"`
	} `json:"message"`
	Data any `json:"data"`
}

func decodeErrorEnvelope(t *testing.T, body []byte) errorEnvelope {
	t.Helper()
	var env errorEnvelope
	require.NoError(t, json.Unmarshal(body, &env), "body: %s", body)
	return env
}

// TestErrorWire_FieldValidationPopulatesMessageFields is J-8's server-side
// pin. api-contract.md §1 has carried message.fields since the envelope was
// redefined, ErrorMessage has had the Go field, the TS client has had the
// type -- and nothing on the server ever wrote it, so the whole path was
// shape without substance. A caller could not tell which field of its
// request was rejected: message.text is the static per-code display string.
//
// This exercises the real HTTP path: a booking with an unparseable amount,
// which fails in parseWireAmount -- the single choke point every handler's
// wire amounts pass through, which is why ErrField is attached there.
func TestErrorWire_FieldValidationPopulatesMessageFields(t *testing.T) {
	srv := newTestServer()

	w := doRequest(srv, http.MethodPost, "/api/v1/bookings", map[string]any{
		"classification_code": "deposit",
		"account_holder":      1,
		"currency_uid":        "cur-1",
		"amount":              "12,34",
		"idempotency_key":     "k-1",
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	env := decodeErrorEnvelope(t, w.Body.Bytes())
	assert.Equal(t, 10001, env.Code)
	assert.Nil(t, env.Data, "api-contract §1: message and data are mutually exclusive")
	assert.NotEmpty(t, env.Message.Text, "the per-code display message is always present")
	require.Contains(t, env.Message.Fields, "amount",
		"message.fields must name the field that failed validation -- that is what a form maps onto an input")
	assert.NotEmpty(t, env.Message.Fields["amount"])

	// The user-facing text must not carry the underlying library's error
	// string (user-facing-surfaces.md: what happened and what to do, never
	// the mechanism).
	raw := w.Body.String()
	for _, leak := range []string{"strconv", "shopspring", "pgx", "SQLSTATE", "bizcode"} {
		assert.NotContains(t, strings.ToLower(raw), strings.ToLower(leak),
			"internal machinery must stay in the log, not the wire")
	}
}

// TestErrorWire_MissingRequiredFieldNamesIt covers the other shape of
// field-level rejection: a required body field left out.
func TestErrorWire_MissingRequiredFieldNamesIt(t *testing.T) {
	srv := newTestServer()

	w := doRequest(srv, http.MethodPost, "/api/v1/reservations/res-1/settle", map[string]any{
		"actual_amount": "10",
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	env := decodeErrorEnvelope(t, w.Body.Bytes())
	require.Contains(t, env.Message.Fields, "idempotency_key")
}

// TestErrorWire_FieldsAbsentWhenNotFieldScoped keeps the fix from turning
// into noise: an error that is not attributable to one request field must
// not invent a fields object (api-contract.md §8 -- consumers distinguish
// absent from empty).
func TestErrorWire_FieldsAbsentWhenNotFieldScoped(t *testing.T) {
	srv := newTestServer()

	// A malformed path parameter: rejected, but not attributable to a body
	// field.
	w := doRequest(srv, http.MethodGet, "/api/v1/balances/not-a-number", nil)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	var envelope map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	message, ok := envelope["message"].(map[string]any)
	require.True(t, ok, "message must be an object on failure")
	require.Contains(t, message, "text")
	assert.NotContains(t, message, "fields", "fields is omitted, not an empty object, when no field is at fault")
}

// TestErrorLogLevel_4xxIsNotError is I-N15's pin. Every httpx.Error used to
// log at Error level, so a scanner throwing bad uids at the API could fill
// the Error stream and bury the 5xx lines that mean the server itself is
// broken. A 4xx is the caller's problem: Info. A real 5xx stays Error.
func TestErrorLogLevel_4xxIsNotError(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	srv := newTestServer()

	// A 400: a holder path parameter that is not a number.
	w := doRequest(srv, http.MethodGet, "/api/v1/balances/not-a-number", nil)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "log line: %s", line)
		assert.NotEqual(t, "ERROR", rec["level"],
			"a 4xx must not log at Error level -- real 5xx lines get buried behind it: %s", line)
	}
}

// TestRequestLog_UsesInjectedLogger is the other half of I-N15: the access
// log went to the package-level slog, so a consumer of this library could
// not route or format the HTTP layer's logs even though every other layer
// takes a core.Logger.
func TestRequestLog_UsesInjectedLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := slogadapter.New(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	srv := newTestServerWith(func(o *testServerOpts) { o.config.Logger = logger })

	w := doRequest(srv, http.MethodGet, "/api/v1/bookings/bk-1", nil)
	require.Equal(t, http.StatusOK, w.Code)

	assert.Contains(t, buf.String(), "http request",
		"the access log must go through Config.Logger, not the package-level slog")
	assert.NotContains(t, buf.String(), "holder=",
		"holder ids stay out of the HTTP logs (the same reason query strings are dropped)")
}

// TestHolderTokenMint_DoesNotLogHolderID pins I-N15's third part: the mint
// audit line recorded the holder id by hand, two files away from the comment
// in middleware_logger.go explaining that query strings are dropped
// *because* they may contain holder ids.
func TestHolderTokenMint_DoesNotLogHolderID(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// The mint audit line only fires for an authenticated caller (it exists
	// to attribute the mint to a key), so this needs API keys configured.
	srv := newTestServerWith(func(o *testServerOpts) {
		o.config.APIKeys = []server.APIKey{{Name: "minter", Scope: server.ScopeWrite, Secret: []byte("mint-key-secret")}}
	})
	require.NoError(t, srv.SetHolderSurface(
		server.HolderConfig{TokenSecret: []byte(testHolderSecret)}, &stubHolderReader{}))

	w := doRequestWithHeader(srv, http.MethodPost, "/api/v1/holder-tokens",
		map[string]any{"holder": 987654321}, "Authorization", "Bearer mint-key-secret")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Contains(t, buf.String(), "holder token minted")
	assert.NotContains(t, buf.String(), "987654321",
		"the mint audit line attributes the API key, not the subject holder id")
}
