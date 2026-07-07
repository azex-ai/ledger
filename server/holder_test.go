package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/server"
)

// stubHolderReader records the holder each call was scoped to and returns
// canned data — the store-side projection is pinned by postgres tests; here
// we pin auth, scoping, and wire shape.
type stubHolderReader struct {
	lastHolder int64
}

func (s *stubHolderReader) ListHolderBalances(_ context.Context, holder int64, currencyUID string) ([]core.HolderBalance, error) {
	s.lastHolder = holder
	return []core.HolderBalance{{
		CurrencyUID:  "cur-uid-1",
		CurrencyCode: "USD",
		Available:    decimal.NewFromInt(75),
		Pending:      decimal.Zero,
		Locked:       decimal.NewFromInt(25),
		Total:        decimal.NewFromInt(100),
	}}, nil
}

func (s *stubHolderReader) ListHolderTransactions(_ context.Context, holder int64, cursor string, limit int32) ([]core.HolderTransaction, string, error) {
	s.lastHolder = holder
	return []core.HolderTransaction{{
		UID:          "j-uid-1",
		Kind:         "deposit_confirm",
		KindLabel:    "Deposit",
		Direction:    core.HolderTransactionIn,
		Amount:       decimal.NewFromInt(100),
		CurrencyUID:  "cur-uid-1",
		CurrencyCode: "USD",
		OccurredAt:   time.Date(2026, 7, 8, 2, 0, 0, 0, time.UTC),
		Memo:         "top up",
	}}, "next-1", nil
}

func (s *stubHolderReader) ListHolderHolds(_ context.Context, holder int64) ([]core.HolderHold, error) {
	s.lastHolder = holder
	return nil, nil
}

const testHolderSecret = "0123456789abcdef0123456789abcdef" // 32 bytes

func TestMintHolderTokenRoundtrip(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	secret := []byte(testHolderSecret)

	token, err := server.MintHolderToken(secret, 42, 15*time.Minute, now)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(token, server.HolderTokenPrefix))

	// Secret too short / zero holder rejected at mint.
	_, err = server.MintHolderToken([]byte("short"), 42, time.Minute, now)
	assert.Error(t, err)
	_, err = server.MintHolderToken(secret, 0, time.Minute, now)
	assert.Error(t, err)
}

// mountedHolderAPI builds the standalone HolderHandler mounted in a bare chi
// router — the library-consumer topology (armatrix path).
func mountedHolderAPI(t *testing.T, stub *stubHolderReader, cfg server.HolderConfig) *httptest.Server {
	t.Helper()
	h, err := server.HolderHandler(cfg, stub)
	require.NoError(t, err)
	r := chi.NewRouter()
	r.Mount("/api/v1", h)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return ts
}

func get(t *testing.T, ts *httptest.Server, path, bearer string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	require.NoError(t, err)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return resp, body
}

func TestHolderHandlerAuth(t *testing.T) {
	// The handler verifies against the real clock (no `now` override is set
	// on the config), so tokens are minted relative to time.Now.
	now := time.Now()
	secret := []byte(testHolderSecret)
	stub := &stubHolderReader{}
	ts := mountedHolderAPI(t, stub, server.HolderConfig{TokenSecret: secret})

	valid, err := server.MintHolderToken(secret, 42, time.Hour, now)
	require.NoError(t, err)
	expired, err := server.MintHolderToken(secret, 42, time.Minute, now.Add(-time.Hour))
	require.NoError(t, err)
	forged, err := server.MintHolderToken([]byte("ffffffffffffffffffffffffffffffff"), 42, time.Hour, now)
	require.NoError(t, err)
	otherHolder, err := server.MintHolderToken(secret, 7, time.Hour, now)
	require.NoError(t, err)

	cases := []struct {
		name     string
		bearer   string
		wantCode float64 // bizcode in envelope
		holder   int64   // expected scoping when authorized
	}{
		{"no token", "", 10101, 0},
		{"api key instead of holder token", "some-api-key", 10101, 0},
		{"expired token", expired, 10101, 0},
		{"forged signature", forged, 10101, 0},
		{"garbage", "lht_not.a.token", 10101, 0},
		{"valid token scopes to its holder", valid, 200, 42},
		{"another token scopes to the other holder", otherHolder, 200, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := get(t, ts, "/api/v1/holder/balances", tc.bearer)
			assert.Equal(t, tc.wantCode, body["code"], "envelope code")
			if tc.wantCode == 200 {
				assert.Equal(t, http.StatusOK, resp.StatusCode)
				assert.Equal(t, tc.holder, stub.lastHolder, "holder comes from the token, never from the request")
			} else {
				assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
			}
		})
	}
}

func TestHolderHandlerWireShape(t *testing.T) {
	now := time.Now()
	secret := []byte(testHolderSecret)
	stub := &stubHolderReader{}
	ts := mountedHolderAPI(t, stub, server.HolderConfig{TokenSecret: secret})
	token, err := server.MintHolderToken(secret, 42, time.Hour, now)
	require.NoError(t, err)

	// Balances: {list} with string amounts + currency code.
	_, body := get(t, ts, "/api/v1/holder/balances", token)
	list := body["data"].(map[string]any)["list"].([]any)
	require.Len(t, list, 1)
	bal := list[0].(map[string]any)
	assert.Equal(t, "USD", bal["currency_code"])
	assert.Equal(t, "75", bal["available"])
	assert.Equal(t, "100", bal["total"])

	// Transactions: {list, next_cursor}, user-language fields only.
	_, body = get(t, ts, "/api/v1/holder/transactions?limit=10", token)
	data := body["data"].(map[string]any)
	assert.Equal(t, "next-1", data["next_cursor"])
	tx := data["list"].([]any)[0].(map[string]any)
	assert.Equal(t, "Deposit", tx["kind_label"])
	assert.Equal(t, "in", tx["direction"])
	assert.Equal(t, "100", tx["amount"])
	assert.Equal(t, "top up", tx["memo"])
	// user-facing-surfaces guard: no double-entry vocabulary in the wire keys.
	raw, _ := json.Marshal(tx)
	for _, word := range []string{"debit", "credit", "entry_type", "classification", "journal_type_uid", "account_holder"} {
		assert.NotContains(t, string(raw), word)
	}

	// Holds: empty list is a list, not null.
	_, body = get(t, ts, "/api/v1/holder/holds", token)
	assert.NotNil(t, body["data"].(map[string]any)["list"])
}

func TestHolderHandlerHasNoAdminRoutes(t *testing.T) {
	stub := &stubHolderReader{}
	ts := mountedHolderAPI(t, stub, server.HolderConfig{TokenSecret: []byte(testHolderSecret)})
	for _, path := range []string{
		"/api/v1/classifications",
		"/api/v1/journals",
		"/api/v1/balances/42",
		"/api/v1/system/balances",
	} {
		resp, err := http.Get(ts.URL + path)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, path)
	}
}

func TestHolderHandlerMint(t *testing.T) {
	stub := &stubHolderReader{}

	// Without MintKeys the endpoint does not exist.
	ts := mountedHolderAPI(t, stub, server.HolderConfig{TokenSecret: []byte(testHolderSecret)})
	resp, err := http.Post(ts.URL+"/api/v1/holder-tokens", "application/json", strings.NewReader(`{"holder":42}`))
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	// With MintKeys: write scope mints, read scope is refused.
	ts2 := mountedHolderAPI(t, stub, server.HolderConfig{
		TokenSecret: []byte(testHolderSecret),
		MintKeys: []server.APIKey{
			{Name: "app", Scope: server.ScopeWrite, Secret: []byte("write-key")},
			{Name: "report", Scope: server.ScopeRead, Secret: []byte("read-key")},
		},
	})
	mint := func(key, body string) (*http.Response, map[string]any) {
		req, err := http.NewRequest(http.MethodPost, ts2.URL+"/api/v1/holder-tokens", strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { resp.Body.Close() })
		var out map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		return resp, out
	}

	_, body := mint("read-key", `{"holder":42}`)
	assert.Equal(t, float64(10150), body["code"], "read scope must not mint")

	_, body = mint("write-key", `{"holder":42}`)
	require.Equal(t, float64(200), body["code"])
	token := body["data"].(map[string]any)["token"].(string)
	assert.True(t, strings.HasPrefix(token, server.HolderTokenPrefix))

	// The minted token authenticates reads.
	respGet, getBody := get(t, ts2, "/api/v1/holder/balances", token)
	assert.Equal(t, http.StatusOK, respGet.StatusCode)
	assert.Equal(t, float64(200), getBody["code"])
	assert.Equal(t, int64(42), stub.lastHolder)

	// TTL over the cap is refused.
	_, body = mint("write-key", fmt.Sprintf(`{"holder":42,"ttl_seconds":%d}`, int64((2*time.Hour)/time.Second)))
	assert.NotEqual(t, float64(200), body["code"])
}

func TestLedgerdHolderSurface(t *testing.T) {
	// Feature off: routes answer 404 (nothing to probe).
	srv := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/holder/balances", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// Feature on: holder token authenticates against the ledgerd router.
	stub := &stubHolderReader{}
	srv2 := newTestServer()
	require.NoError(t, srv2.SetHolderSurface(server.HolderConfig{TokenSecret: []byte(testHolderSecret)}, stub))

	token, err := server.MintHolderToken([]byte(testHolderSecret), 42, time.Hour, time.Now())
	require.NoError(t, err)
	req = httptest.NewRequest(http.MethodGet, "/api/v1/holder/transactions", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	srv2.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, int64(42), stub.lastHolder)

	// Short secret rejected at configuration time.
	assert.Error(t, srv2.SetHolderSurface(server.HolderConfig{TokenSecret: []byte("short")}, stub))
}
