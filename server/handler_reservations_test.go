package server_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// TestCreateReservation_RequireVerifiedBalancePassesThrough pins
// structure.md's Major: before this fix, createReservationRequest had no
// field for RequireVerifiedBalance, so nothing on the HTTP surface could
// ever set core.ReserveInput.RequireVerifiedBalance -- the tamper-evident
// verification gate (core.ReserveInput's own doc comment) was reachable
// only from Go library callers, never from behind this HTTP API.
func TestCreateReservation_RequireVerifiedBalancePassesThrough(t *testing.T) {
	var got core.ReserveInput
	srv := newTestServerWith(func(o *testServerOpts) {
		o.reserver = &mockReserver{
			reserveFn: func(_ context.Context, input core.ReserveInput) (*core.Reservation, error) {
				got = input
				return &core.Reservation{UID: "rsv-1", AccountHolder: input.AccountHolder, CurrencyUID: input.CurrencyUID, ReservedAmount: input.Amount, Status: core.ReservationStatusActive, IdempotencyKey: input.IdempotencyKey}, nil
			},
		}
	})

	body := map[string]any{
		"account_holder":           int64(42),
		"currency_uid":             "cur-1",
		"amount":                   "10",
		"idempotency_key":          "rvb-test-1",
		"require_verified_balance": true,
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/reservations", body)
	require.Equal(t, http.StatusCreated, w.Code)
	assert.True(t, got.RequireVerifiedBalance)

	// Off by default -- a caller who never mentions the field sees no
	// behavior change (core.ReserveInput's doc comment).
	body2 := map[string]any{
		"account_holder":  int64(42),
		"currency_uid":    "cur-1",
		"amount":          "10",
		"idempotency_key": "rvb-test-2",
	}
	w2 := doRequest(srv, http.MethodPost, "/api/v1/reservations", body2)
	require.Equal(t, http.StatusCreated, w2.Code)
	assert.False(t, got.RequireVerifiedBalance)
}
