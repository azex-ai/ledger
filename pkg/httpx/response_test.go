package httpx

import (
	stdjson "encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

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

// coreSentinels binds every sentinel core/errors.go declares to its value.
// The BINDING has to be written out (Go has no runtime symbol lookup), but
// the LIST does not: TestCoreSentinels_AreAllBound below parses
// core/errors.go with go/ast and fails if this map has drifted from it, in
// either direction. That is the part E-M9 needed -- the previous version of
// the agreement pin held a hand-copied slice which had never been updated
// for core.ErrUnknownAuthKey, so the one error whose two classifications
// disagreed was precisely the one the pin could not see.
var coreSentinels = map[string]error{
	"ErrNotFound":            core.ErrNotFound,
	"ErrInvalidInput":        core.ErrInvalidInput,
	"ErrInsufficientBalance": core.ErrInsufficientBalance,
	"ErrDuplicateJournal":    core.ErrDuplicateJournal,
	"ErrUnbalancedJournal":   core.ErrUnbalancedJournal,
	"ErrInvalidTransition":   core.ErrInvalidTransition,
	"ErrConflict":            core.ErrConflict,
	"ErrPrecisionExceeded":   core.ErrPrecisionExceeded,
	"ErrAccountFrozen":       core.ErrAccountFrozen,
	"ErrAccountClosed":       core.ErrAccountClosed,
	"ErrPeriodClosed":        core.ErrPeriodClosed,
	"ErrAttestorUnavailable": core.ErrAttestorUnavailable,
	"ErrUnauthorizedJournal": core.ErrUnauthorizedJournal,
	"ErrUnknownAuthKey":      core.ErrUnknownAuthKey,
	"ErrRollupPending":       core.ErrRollupPending,
	"ErrTransient":           core.ErrTransient,
}

// declaredCoreSentinels parses core/errors.go and returns every top-level
// Err* var it declares.
func declaredCoreSentinels(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "../../core/errors.go", nil, parser.SkipObjectResolution)
	require.NoError(t, err, "parse core/errors.go")

	out := map[string]bool{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range vs.Names {
				if strings.HasPrefix(name.Name, "Err") && name.IsExported() {
					out[name.Name] = true
				}
			}
		}
	}
	require.NotEmpty(t, out, "no Err* sentinels found in core/errors.go -- the parse is broken, not the package")
	return out
}

// TestCoreSentinels_AreAllBound makes the two tests below complete by
// construction: a sentinel added to core/errors.go and forgotten here goes
// red, instead of quietly never being classified.
func TestCoreSentinels_AreAllBound(t *testing.T) {
	declared := declaredCoreSentinels(t)

	var unbound, stale []string
	for name := range declared {
		if _, ok := coreSentinels[name]; !ok {
			unbound = append(unbound, name)
		}
	}
	for name := range coreSentinels {
		if !declared[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(unbound)
	sort.Strings(stale)
	require.Empty(t, unbound, "core/errors.go declares sentinel(s) this package never classifies -- add them to coreSentinels (and to resolveError)")
	require.Empty(t, stale, "coreSentinels names sentinel(s) core/errors.go no longer declares -- delete them")
}

// TestResolveError_MapsEverySentinelExplicitly is E-M9's whole-set rule:
// every core sentinel must have an explicit branch in resolveError. Falling
// through to 19999 is not a mapping -- it is an unclassified internal error,
// whose band bizcode.Retryable calls retryable, which is the opposite of
// what core.IsRetryable says for every domain sentinel.
func TestResolveError_MapsEverySentinelExplicitly(t *testing.T) {
	declaredCoreSentinels(t) // fail early if the parse broke

	for name, err := range coreSentinels {
		t.Run(name, func(t *testing.T) {
			ae := resolveError(err)
			require.NotNil(t, ae)
			assert.NotEqual(t, 19999, ae.Code,
				"core.%s falls through resolveError to the unclassified-internal default; give it an explicit case", name)
		})
	}
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
//
// The sentinel set is derived (see coreSentinels / TestCoreSentinels_AreAllBound).
// Each sentinel is checked bare and wrapped, since every real caller wraps.
func TestResolveError_AgreesWithCoreIsRetryable(t *testing.T) {
	check := func(t *testing.T, err error) {
		t.Helper()
		ae := resolveError(err)
		require.NotNil(t, ae)
		wantRetryable := core.IsRetryable(err)
		gotRetryable := bizcode.Retryable(ae.Code)
		assert.Equal(t, wantRetryable, gotRetryable,
			"core.IsRetryable(%v)=%v but bizcode.Retryable(resolveError(%v).Code=%d)=%v -- library and HTTP modes disagree on the same error",
			err, wantRetryable, err, ae.Code, gotRetryable)
	}

	for name, err := range coreSentinels {
		t.Run(name, func(t *testing.T) { check(t, err) })
		t.Run(name+"/wrapped", func(t *testing.T) {
			check(t, fmt.Errorf("postgres: some operation: %w", err))
		})
	}

	t.Run("unclassified", func(t *testing.T) {
		check(t, fmt.Errorf("unclassified dependency hiccup"))
	})
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

// TestOK_TimeSerializesAsUTCRFC3339 pins MJ-3: every time.Time in a response
// serializes as RFC3339 in UTC (trailing Z), regardless of the time's own
// Location. Without the utcTimeExtension, a value in a +08:00 zone would emit
// a +08:00 offset, and the process TZ would leak into the wire contract.
func TestOK_TimeSerializesAsUTCRFC3339(t *testing.T) {
	type payload struct {
		CreatedAt time.Time `json:"created_at"`
	}
	// A time carrying a non-UTC offset -- the exact shape pgx hands back when
	// the process runs in Asia/Singapore.
	sgt := time.FixedZone("SGT", 8*3600)
	ts := time.Date(2026, 4, 29, 18, 0, 0, 0, sgt)

	w := httptest.NewRecorder()
	OK(w, payload{CreatedAt: ts})

	body, err := io.ReadAll(w.Result().Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), `"created_at":"2026-04-29T10:00:00Z"`,
		"time must serialize as UTC RFC3339 (Z), never a local offset; got %s", body)
	assert.NotContains(t, string(body), "+08:00", "no local offset may reach the wire")
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
