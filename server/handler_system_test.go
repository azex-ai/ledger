package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// failingHealthQueries overrides GetHealthMetrics only; every other
// core.QueryProvider method is promoted from the embedded mockQueryProvider.
type failingHealthQueries struct{ *mockQueryProvider }

func (failingHealthQueries) GetHealthMetrics(context.Context) (*core.HealthMetrics, error) {
	return nil, errors.New("db down")
}

// TestHealthEndpoint_FailurePathUsesEnvelope pins structure.md's Minor:
// before this fix, a DB-down /system/health response was a hand-written
// {"status":"degraded","db":"down"} body with no code/message/data --
// api-contract.md §1's "every REST response, no exceptions" envelope had a
// silent carve-out for exactly the response a monitoring system reads to
// decide whether the service is alive.
func TestHealthEndpoint_FailurePathUsesEnvelope(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.queries = failingHealthQueries{&mockQueryProvider{}}
	})

	w := doRequest(srv, http.MethodGet, "/api/v1/system/health", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	var env map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, float64(18101), env["code"], "envelope code, not a bare {\"status\":...} body")
	require.NotNil(t, env["message"])
	assert.Nil(t, env["data"])
}

// TestReadyEndpoint_FailurePathUsesEnvelope is TestHealthEndpoint_FailurePathUsesEnvelope's
// sibling for /system/ready: newTestServer never calls SetReady(true), so
// this exercises the not-ready path by construction.
func TestReadyEndpoint_FailurePathUsesEnvelope(t *testing.T) {
	srv := newTestServer()

	w := doRequest(srv, http.MethodGet, "/api/v1/system/ready", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	var env map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, float64(18101), env["code"])
	require.NotNil(t, env["message"])
	assert.Nil(t, env["data"])
}
