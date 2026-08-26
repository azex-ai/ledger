package httpx

import (
	stdjson "encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/bizcode"
)

// --- snakeCase ---

func TestSnakeCase(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"ID", "id"},
		{"CurrencyID", "currency_id"},
		{"HTTPStatus", "http_status"},
		{"IsActive", "is_active"},
		{"AccountHolder", "account_holder"},
		{"TotalBalance", "total_balance"},
		{"URL", "url"},
		{"HTMLParser", "html_parser"},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			assert.Equal(t, tc.want, snakeCase(tc.input))
		})
	}
}

// --- resolveError ---

func TestResolveError(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantCode   int
		wantStatus int
	}{
		{"ErrNotFound", core.ErrNotFound, 10201, http.StatusNotFound},
		{"ErrInvalidInput", core.ErrInvalidInput, 10001, http.StatusBadRequest},
		{"ErrInsufficientBalance", core.ErrInsufficientBalance, 14001, http.StatusUnprocessableEntity},
		{"ErrDuplicateJournal", core.ErrDuplicateJournal, 14002, http.StatusUnprocessableEntity},
		{"ErrUnbalancedJournal", core.ErrUnbalancedJournal, 14003, http.StatusUnprocessableEntity},
		{"ErrInvalidTransition", core.ErrInvalidTransition, 14004, http.StatusUnprocessableEntity},
		{"ErrPeriodClosed", core.ErrPeriodClosed, 14009, http.StatusUnprocessableEntity},
		{"ErrConflict", core.ErrConflict, 10901, http.StatusConflict},
		{"unknown error", fmt.Errorf("something went wrong"), 19999, http.StatusInternalServerError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ae := resolveError(tc.err)
			require.NotNil(t, ae)
			assert.Equal(t, tc.wantCode, ae.Code)
			assert.Equal(t, tc.wantStatus, ae.HTTPStatus())
		})
	}
}

func TestResolveError_WrappedSentinel(t *testing.T) {
	wrapped := fmt.Errorf("store: get account: %w", core.ErrNotFound)
	ae := resolveError(wrapped)
	require.NotNil(t, ae)
	assert.Equal(t, 10201, ae.Code)
	assert.Equal(t, http.StatusNotFound, ae.HTTPStatus())
}

func TestResolveError_AlreadyAppError(t *testing.T) {
	original := bizcode.New(14001, "custom message")
	ae := resolveError(original)
	// Must return the same pointer — not re-wrapped.
	assert.Same(t, original, ae)
}

func TestResolveError_WrappedAppError(t *testing.T) {
	original := bizcode.New(10201, "not found")
	wrapped := fmt.Errorf("handler: %w", original)
	ae := resolveError(wrapped)
	require.NotNil(t, ae)
	assert.Equal(t, 10201, ae.Code)
}

// TestResolveError_UnauthorizedJournal pins the fix for the finding at
// consumer-surface.md:160 -- core.ErrUnauthorizedJournal, tamper detection's
// only signal, must never fall through to the default 19999 (which both
// mislabels it as an unclassified 500 and, via Retryable's default, tells
// clients to retry a rejection that will never change).
func TestResolveError_UnauthorizedJournal(t *testing.T) {
	ae := resolveError(core.ErrUnauthorizedJournal)
	require.NotNil(t, ae)
	assert.NotEqual(t, 19999, ae.Code, "must not fall through to the unclassified-internal-error default")
	assert.Equal(t, 14010, ae.Code)
	assert.Equal(t, http.StatusUnprocessableEntity, ae.HTTPStatus())
	assert.False(t, bizcode.Retryable(ae.Code), "an unauthorized journal is a permanent rejection, not a transient one")
}

func TestResolveError_UnauthorizedJournal_Wrapped(t *testing.T) {
	wrapped := fmt.Errorf("core: verify journal auth: journal has no stored digest: %w", core.ErrUnauthorizedJournal)
	ae := resolveError(wrapped)
	require.NotNil(t, ae)
	assert.Equal(t, 14010, ae.Code)
}

// TestResolveError_AgreesWithCoreIsRetryable is the structural pin required
// by contracts §W1-C: library-mode consumers (core.IsRetryable) and HTTP-mode
// consumers (bizcode.Retryable, applied to whatever code resolveError picks)
// must reach the SAME retry verdict for the SAME underlying error. Two
// independently-maintained classification tables that happen to agree today
// is exactly the failure shape working-agreements.md §5 warns about
// (default values duplicated across layers) -- this test makes future
// disagreement a compile-time-adjacent (test-time) failure instead of a
// silent drift.
func TestResolveError_AgreesWithCoreIsRetryable(t *testing.T) {
	cases := []error{
		core.ErrNotFound,
		core.ErrInvalidInput,
		core.ErrInsufficientBalance,
		core.ErrDuplicateJournal,
		core.ErrUnbalancedJournal,
		core.ErrInvalidTransition,
		core.ErrConflict,
		core.ErrPrecisionExceeded,
		core.ErrAccountFrozen,
		core.ErrAccountClosed,
		core.ErrPeriodClosed,
		core.ErrUnauthorizedJournal,
		core.ErrRollupPending,
		core.ErrAttestorUnavailable,
		core.ErrTransient,
		fmt.Errorf("unclassified dependency hiccup"),
	}

	for _, err := range cases {
		t.Run(err.Error(), func(t *testing.T) {
			ae := resolveError(err)
			require.NotNil(t, ae)
			wantRetryable := core.IsRetryable(err)
			gotRetryable := bizcode.Retryable(ae.Code)
			assert.Equal(t, wantRetryable, gotRetryable,
				"core.IsRetryable(%v)=%v but bizcode.Retryable(resolveError(%v).Code=%d)=%v -- library and HTTP modes disagree on the same error",
				err, wantRetryable, err, ae.Code, gotRetryable)
		})
	}
}

// --- OK / Created response format ---

type successEnvelope struct {
	Code    int                `json:"code"`
	Message *ErrorMessage      `json:"message"`
	Data    stdjson.RawMessage `json:"data"`
}

func TestOK(t *testing.T) {
	type payload struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}

	w := httptest.NewRecorder()
	OK(w, payload{ID: 1, Name: "test"})

	resp := w.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var env successEnvelope
	require.NoError(t, stdjson.Unmarshal(body, &env))
	assert.Equal(t, 200, env.Code)
	assert.Nil(t, env.Message)

	var data payload
	require.NoError(t, stdjson.Unmarshal(env.Data, &data))
	assert.Equal(t, 1, data.ID)
	assert.Equal(t, "test", data.Name)
}

func TestCreated(t *testing.T) {
	type payload struct {
		ID int `json:"id"`
	}

	w := httptest.NewRecorder()
	Created(w, payload{ID: 42})

	resp := w.Result()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var env successEnvelope
	require.NoError(t, stdjson.Unmarshal(body, &env))
	assert.Equal(t, 200, env.Code)
	assert.Nil(t, env.Message)

	var data payload
	require.NoError(t, stdjson.Unmarshal(env.Data, &data))
	assert.Equal(t, 42, data.ID)
}

// --- Error response format ---

type errorEnvelope struct {
	Code    int           `json:"code"`
	Message *ErrorMessage `json:"message"`
	Data    any           `json:"data"`
}

func TestError_DomainSentinel(t *testing.T) {
	w := httptest.NewRecorder()
	Error(w, core.ErrNotFound)

	resp := w.Result()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var env errorEnvelope
	require.NoError(t, stdjson.Unmarshal(body, &env))
	assert.Equal(t, 10201, env.Code)
	require.NotNil(t, env.Message)
	assert.NotEmpty(t, env.Message.Text)
	assert.Nil(t, env.Data)
}

func TestError_UnknownError(t *testing.T) {
	w := httptest.NewRecorder()
	Error(w, fmt.Errorf("unexpected db failure"))

	resp := w.Result()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var env errorEnvelope
	require.NoError(t, stdjson.Unmarshal(body, &env))
	assert.Equal(t, 19999, env.Code)
	require.NotNil(t, env.Message)
	assert.Nil(t, env.Data)
}

func TestError_AppError(t *testing.T) {
	w := httptest.NewRecorder()
	Error(w, bizcode.New(10901, "state conflict"))

	resp := w.Result()
	assert.Equal(t, http.StatusConflict, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var env errorEnvelope
	require.NoError(t, stdjson.Unmarshal(body, &env))
	assert.Equal(t, 10901, env.Code)
	require.NotNil(t, env.Message)
	assert.Nil(t, env.Data)
}

func TestError_RateLimited_UsesCanonicalEnvelope(t *testing.T) {
	w := httptest.NewRecorder()
	Error(w, bizcode.New(10401, "rate limit exceeded"))

	body, err := io.ReadAll(w.Result().Body)
	require.NoError(t, err)

	var env errorEnvelope
	require.NoError(t, stdjson.Unmarshal(body, &env))
	require.NotNil(t, env.Message)
	assert.Nil(t, env.Data)
}

// --- Decode ---

func TestDecode_Valid(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}

	body := strings.NewReader(`{"name":"alice","age":30}`)
	r := httptest.NewRequest(http.MethodPost, "/", body)
	r.Header.Set("Content-Type", "application/json")

	got, err := Decode[payload](r)
	require.NoError(t, err)
	assert.Equal(t, "alice", got.Name)
	assert.Equal(t, 30, got.Age)
}

func TestDecode_InvalidJSON(t *testing.T) {
	body := strings.NewReader(`{not valid json`)
	r := httptest.NewRequest(http.MethodPost, "/", body)

	_, err := Decode[map[string]any](r)
	require.Error(t, err)

	var ae *bizcode.AppError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, 10001, ae.Code)
}

func TestDecode_EmptyBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))

	_, err := Decode[map[string]any](r)
	require.Error(t, err)

	var ae *bizcode.AppError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, 10001, ae.Code)
}
