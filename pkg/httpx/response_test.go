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

// --- OK / Created response format ---

type successEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
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
	assert.Equal(t, 0, env.Code)
	assert.Equal(t, "ok", env.Message)

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
	assert.Equal(t, 0, env.Code)
	assert.Equal(t, "created", env.Message)

	var data payload
	require.NoError(t, stdjson.Unmarshal(env.Data, &data))
	assert.Equal(t, 42, data.ID)
}

// --- Error response format ---

type errorEnvelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
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
	assert.NotEmpty(t, env.Message)
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
