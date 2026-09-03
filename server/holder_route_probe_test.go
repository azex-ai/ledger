package server

// P-1 (2026-09-03 independent review): TestRouteAuthorization_EveryRouteIsGuarded
// checks its two branches with two different standards.
//
// The scoped branch is a BEHAVIOUR probe: deriveRouteRequirement builds the
// route's real middleware chain, drives synthetic identities through it, and
// reports what actually got past. C-1 built that deliberately, because "the
// chain contains an authorization middleware" is not the same claim as "the
// chain contains the RIGHT one".
//
// The holder branch is still `chainContains(chain, "holderTokenAuth")` -- a
// match on the resolved function NAME. The reviewer replaced
// holderTokenAuth's body with `next.ServeHTTP(w, r)` and the gate stayed
// green; the net result was red only because three unrelated handler tests
// happened to fail. That is the pre-C-1 standard, still in place on the
// branch that guards a user's own balance, statement and deposit address.
//
// This file gives the holder branch the same standard. It drives every
// route under the holder prefix with no credential, with a forged token,
// and with a valid one, through the router itself -- so what is measured is
// the routing and middleware the server actually assembles, not a name.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// probeHolderReader is a HolderReader that records whether it was reached.
// Reaching it means the request got past authorization, which is the only
// thing this file measures -- the responses themselves are other tests'
// business.
type probeHolderReader struct {
	reached bool
}

func (p *probeHolderReader) ListHolderBalances(context.Context, int64, string) ([]core.HolderBalance, error) {
	p.reached = true
	return nil, nil
}

func (p *probeHolderReader) ListHolderTransactions(context.Context, int64, string, int32) ([]core.HolderTransaction, string, error) {
	p.reached = true
	return nil, "", nil
}

func (p *probeHolderReader) ListHolderHolds(context.Context, int64, string, int32) ([]core.HolderHold, string, error) {
	p.reached = true
	return nil, "", nil
}

// TestHolderRoutes_RejectEveryRequestWithoutAValidToken drives each holder
// route three ways and asserts the store is reached only for the third.
//
// The valid-token case is not decoration. Without it, a chain that rejects
// everything -- an unconfigured surface answering 404, a typo in the route,
// a middleware that always denies -- would satisfy the two negative cases
// and prove nothing about the guard.
func TestHolderRoutes_RejectEveryRequestWithoutAValidToken(t *testing.T) {
	secret := []byte("holder-route-probe-secret-not-a-real-secret")
	const holder = int64(4242)

	store := &probeHolderReader{}
	s := &Server{router: chi.NewRouter()}
	s.setupRoutes()
	require.NoError(t, s.SetHolderSurface(HolderConfig{TokenSecret: secret}, store))

	valid, err := MintHolderToken(secret, holder, time.Hour, time.Now())
	require.NoError(t, err)

	forged, err := MintHolderToken([]byte("a-different-secret-of-adequate-length!!"), holder, time.Hour, time.Now())
	require.NoError(t, err)

	probed := 0
	err = chi.Walk(s.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = normalizeWalkRoute(route)
		if !strings.HasPrefix(route, holderPathPrefix) {
			return nil
		}
		probed++

		// Concrete values for the path parameters; which ones exist varies
		// by route and the placeholder is harmless where it does not.
		target := strings.NewReplacer(
			"{currency_uid}", "11111111-1111-1111-1111-111111111111",
			"{chain_id}", "1",
			"{holder}", "4242",
		).Replace(route)

		// Measured as "did the request get past authorization", not "did it
		// reach the store": two of these routes need an AddressRegistry the
		// probe does not wire, so they answer 503 once authorized. 503 is a
		// pass here and 401 is a rejection, which is exactly the
		// distinction being asserted. store.reached is checked as well
		// where it applies, so a rejection also has to mean nothing
		// downstream ran.
		serve := func(auth string) (status int, reachedStore bool) {
			store.reached = false
			req := httptest.NewRequest(method, target, http.NoBody)
			if auth != "" {
				req.Header.Set("Authorization", "Bearer "+auth)
			}
			rec := httptest.NewRecorder()
			s.router.ServeHTTP(rec, req)
			return rec.Code, store.reached
		}

		status, reached := serve("")
		assert.Equalf(t, http.StatusUnauthorized, status,
			"%s %s answered %d with NO Authorization header at all. isUnauthenticatedPath exempts this prefix from "+
				"bearer auth on the understanding that the holder token stands in its place; if that guard is gone, "+
				"the route serves one holder's balance, statement and deposit address to anybody who asks (P-1)",
			method, route, status)
		assert.Falsef(t, reached, "%s %s reached the holder store with no credential at all", method, route)

		status, reached = serve(forged)
		assert.Equalf(t, http.StatusUnauthorized, status,
			"%s %s answered %d for a token signed with a DIFFERENT secret. The signature is the only thing binding a "+
				"token to this deployment: without it, anyone who knows the token format can name any holder they like",
			method, route, status)
		assert.Falsef(t, reached, "%s %s reached the holder store with a forged token", method, route)

		status, _ = serve(valid)
		assert.NotEqualf(t, http.StatusUnauthorized, status,
			"%s %s answered 401 for a VALIDLY signed token. The two rejections above only mean something if the route "+
				"is reachable when it should be -- a chain that refuses everything, or a route that does not exist, "+
				"would pass them and prove nothing", method, route)

		return nil
	})
	require.NoError(t, err, "walk chi routes")

	require.Positivef(t, probed,
		"no route under %q was probed -- either the prefix changed or the walk found nothing, and a probe that "+
			"exercises no route reads as a pass", holderPathPrefix)
}
