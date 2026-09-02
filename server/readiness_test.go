package server_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/server"
)

// TestReady_RequiresHostToSayReady is E-M11's pin.
//
// Readiness was a flag nothing in the library ever set, while docs/api.md and
// README both described GET /system/ready as turning green once "migrations +
// worker have booted". A host that never found SetReady got a probe answering
// 503 forever, and from inside the process "not ready yet" and "nobody will
// ever say ready" are indistinguishable -- so the deployment looked broken
// with no signal saying which of the two it was.
//
// This pins the honest behavior in both directions, so the documented
// sentence and the code cannot drift again.
func TestReady_RequiresHostToSayReady(t *testing.T) {
	srv := newTestServer()

	// Nothing called SetReady: 503, standard envelope, code 18101 (not the
	// {"status":"starting"} body an earlier revision of the docs showed).
	w := doRequest(srv, http.MethodGet, "/api/v1/system/ready", nil)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	env := decodeErrorEnvelope(t, w.Body.Bytes())
	assert.Equal(t, 18101, env.Code)
	assert.Nil(t, env.Data)

	srv.SetReady(true)
	w = doRequest(srv, http.MethodGet, "/api/v1/system/ready", nil)
	require.Equal(t, http.StatusOK, w.Code)

	srv.SetReady(false)
	w = doRequest(srv, http.MethodGet, "/api/v1/system/ready", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code,
		"readiness must be able to go back to false -- a draining pod says so through this probe")
}

// TestReady_ProbeAnswersInsteadOfFlag pins Deps.ReadyProbe: the point of the
// field is that "who decides ready" becomes a construction-time question
// instead of a setter someone has to remember, so the probe must actually
// take precedence over the flag's zero value.
func TestReady_ProbeAnswersInsteadOfFlag(t *testing.T) {
	ready := false
	srv, err := server.NewFromDeps(
		&server.Config{Env: "dev", CORSAllowOrigin: "*", MaxBodyBytes: 256 * 1024},
		func() server.Deps {
			deps := depsFromMocks()
			deps.ReadyProbe = func() bool { return ready }
			return deps
		}(),
	)
	require.NoError(t, err)

	w := doRequest(srv, http.MethodGet, "/api/v1/system/ready", nil)
	require.Equal(t, http.StatusServiceUnavailable, w.Code, "the probe says not ready")

	ready = true
	w = doRequest(srv, http.MethodGet, "/api/v1/system/ready", nil)
	require.Equal(t, http.StatusOK, w.Code,
		"the probe is consulted per request -- SetReady was never called here")
	assert.True(t, srv.IsReady())
}
