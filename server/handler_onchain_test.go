package server_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// --- Mock DepositAddressProvider ---

type mockDepositAddressProvider struct {
	ensureFn func(ctx context.Context, holder int64) (*core.DepositAddress, error)
	getFn    func(ctx context.Context, holder int64) (*core.DepositAddress, error)
}

func (m *mockDepositAddressProvider) EnsureDepositAddress(ctx context.Context, holder int64) (*core.DepositAddress, error) {
	return m.ensureFn(ctx, holder)
}

func (m *mockDepositAddressProvider) GetDepositAddress(ctx context.Context, holder int64) (*core.DepositAddress, error) {
	return m.getFn(ctx, holder)
}

func TestEnsureDepositAddress(t *testing.T) {
	srv := newTestServer()

	fixedAddr := &core.DepositAddress{
		UID:           "addr-uid-1",
		AccountHolder: 1001,
		Address:       "0xB3e7eA5de7C24b4e89b1AC454f02a42DBAE0BFc0",
		Factory:       "0x6CE5E7A510C693E1E4FC032d8De0c394C9C1A323",
		InitHash:      "0x2ef28d391fa40901fc8c61168ece13f5247e49e87925cd7f617262b9231b9ece",
		CreatedAt:     time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC),
	}
	calls := 0
	srv.SetDepositAddressProvider(&mockDepositAddressProvider{
		ensureFn: func(ctx context.Context, holder int64) (*core.DepositAddress, error) {
			calls++
			assert.Equal(t, int64(1001), holder)
			return fixedAddr, nil
		},
	})

	w := doRequest(srv, http.MethodPost, "/api/v1/holders/1001/deposit-address", nil)
	require.Equal(t, http.StatusCreated, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "addr-uid-1", data["uid"])
	assert.Equal(t, "0xB3e7eA5de7C24b4e89b1AC454f02a42DBAE0BFc0", data["address"])
	assert.Equal(t, float64(1001), data["account_holder"])
	assert.Equal(t, "2026-07-11T00:00:00Z", data["created_at"])

	// Internal derivation fingerprint stays off the wire
	// (user-facing-surfaces.md) -- factory/init_hash are audit-only detail.
	assert.NotContains(t, data, "factory")
	assert.NotContains(t, data, "init_hash")

	// Idempotent per the wire contract: repeated calls resolve to the same
	// address (the provider itself owns the idempotency; this pins that the
	// handler is a thin passthrough that doesn't second-guess it).
	w2 := doRequest(srv, http.MethodPost, "/api/v1/holders/1001/deposit-address", nil)
	data2 := parseEnvelope(t, w2.Body.Bytes())
	assert.Equal(t, data["address"], data2["address"])
	assert.Equal(t, 2, calls)
}

func TestEnsureDepositAddress_NotEnabled(t *testing.T) {
	srv := newTestServer() // SetDepositAddressProvider never called
	w := doRequest(srv, http.MethodPost, "/api/v1/holders/1001/deposit-address", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestEnsureDepositAddress_InvalidHolder(t *testing.T) {
	srv := newTestServer()
	srv.SetDepositAddressProvider(&mockDepositAddressProvider{
		ensureFn: func(ctx context.Context, holder int64) (*core.DepositAddress, error) {
			t.Fatal("must not reach the provider on a malformed holder path param")
			return nil, nil
		},
	})
	w := doRequest(srv, http.MethodPost, "/api/v1/holders/not-a-number/deposit-address", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetDepositAddress(t *testing.T) {
	srv := newTestServer()
	fixedAddr := &core.DepositAddress{
		UID: "addr-uid-1", AccountHolder: 1001,
		Address: "0xB3e7eA5de7C24b4e89b1AC454f02a42DBAE0BFc0", CreatedAt: time.Now(),
	}
	srv.SetDepositAddressProvider(&mockDepositAddressProvider{
		getFn: func(ctx context.Context, holder int64) (*core.DepositAddress, error) {
			assert.Equal(t, int64(1001), holder)
			return fixedAddr, nil
		},
	})

	w := doRequest(srv, http.MethodGet, "/api/v1/holders/1001/deposit-address", nil)
	require.Equal(t, http.StatusOK, w.Code)
	data := parseEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "addr-uid-1", data["uid"])
}

func TestGetDepositAddress_NotFound(t *testing.T) {
	srv := newTestServer()
	srv.SetDepositAddressProvider(&mockDepositAddressProvider{
		getFn: func(ctx context.Context, holder int64) (*core.DepositAddress, error) {
			return nil, core.ErrNotFound
		},
	})
	w := doRequest(srv, http.MethodGet, "/api/v1/holders/1001/deposit-address", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGetDepositAddress_NotEnabled(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/holders/1001/deposit-address", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}
