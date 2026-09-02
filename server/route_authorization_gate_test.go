package server

// D-m6 (2026-09-02 deep audit): a gate asserting that every registered route
// carries an authorization middleware, and that the routes exempt from bearer
// auth are exactly the ones somebody decided should be.
//
// The route table is clean today; this is the missing gate, not a missing fix.
// Two things made it worth building anyway.
//
// The first is that the exemption is a string prefix. isUnauthenticatedPath
// skips bearer auth for anything under "/api/v1/holder/", on the understanding
// that everything there is in the holderTokenAuth group. Nothing enforces the
// second half. Register one route under that prefix outside the group -- a
// plausible thing to do, since "/holders/{holder}/..." (plural) and
// "/holder-tokens" both look adjacent and neither matches -- and it is
// unauthenticated, with no test anywhere noticing.
//
// The second is that the infrastructure was already here. chi.Walk has been
// used since the OpenAPI contract gate to assert every route is documented;
// its callback hands over the route's middleware chain and that argument was
// discarded. "Is this route documented" was machine-checked. "Is this route
// authenticated" was not.
//
// Verified in both directions: moving a write route out of the ScopeWrite
// group, and registering a route under /api/v1/holder/ outside the holder
// group, each turn this red.

import (
	"net/http"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Routes that answer without a bearer API key, and why each one is allowed to.
//
// A hardcoded list on purpose, and the only hardcoded list in this file: every
// other property here is derived from the router. Adding an entry is the
// reviewable act -- the same contract grant_coverage_test.go's three-way
// classification uses. A new unauthenticated route fails until someone writes
// down why it is one.
var unauthenticatedRouteAllowlist = map[string]string{
	"GET /api/v1/system/health":       "liveness probe; Kubernetes cannot present an API key",
	"GET /api/v1/system/ready":        "readiness probe; same",
	"POST /api/v1/webhooks/{channel}": "inbound channel callbacks authenticate with the channel adapter's own signature (HMAC), which is why openapi.yaml declares this path security: []",
}

func TestRouteAuthorization_EveryRouteIsGuarded(t *testing.T) {
	s := &Server{router: chi.NewRouter()}
	s.setupRoutes()

	sawScoped, sawHolder := 0, 0

	err := chi.Walk(s.router, func(method, route string, _ http.Handler, middlewares ...func(http.Handler) http.Handler) error {
		route = normalizeWalkRoute(route)
		key := method + " " + route
		chain := middlewareNames(middlewares)

		switch {
		case strings.HasPrefix(route, holderPathPrefix):
			// Checked before the exemption branch, because this prefix IS one of
			// the exemptions: isUnauthenticatedPath skips bearer auth for it on
			// the understanding that holderTokenAuth is there instead. Nothing
			// enforced that understanding, and this is where it gets enforced.
			require.True(t, chainContains(chain, "holderTokenAuth"),
				"route %q is under %q, which isUnauthenticatedPath exempts from bearer auth, but its middleware chain does not include holderTokenAuth -- "+
					"this route is reachable with no credential at all. chain: %v", key, holderPathPrefix, chain)
			sawHolder++

		case isUnauthenticatedPath(route):
			if _, allowed := unauthenticatedRouteAllowlist[key]; !allowed {
				t.Errorf("route %q is exempt from bearer auth by isUnauthenticatedPath and is not in unauthenticatedRouteAllowlist.\n"+
					"Either it needs auth (register it outside the exempt prefixes), or it is a deliberate exemption and needs an entry saying why.\n"+
					"middleware chain: %v", key, chain)
			}

		default:
			require.True(t,
				chainContains(chain, "requireScope") || chainContains(chain, "requireCapability"),
				"route %q carries no scope or capability check: a valid key of ANY scope reaches it. chain: %v", key, chain)
			sawScoped++
		}
		return nil
	})
	require.NoError(t, err, "walk chi routes")

	// Fail-closed sanity. If middlewareNames ever stops resolving (a refactor
	// that wraps the middlewares, a Go change to closure naming), every route
	// in the default branch goes red rather than silently passing -- but the
	// holder branch would go red too, and both counts being zero would mean
	// the walk found nothing at all. Assert we actually classified routes.
	assert.Positive(t, sawScoped, "sanity: the walk must have found scope-guarded routes")
	assert.Positive(t, sawHolder, "sanity: the walk must have found holder-token routes")
}

// TestRouteAuthorization_ExemptPrefixesAgreeAcrossMiddleware pins the other
// half of the finding: two middlewares carve the webhook paths out for two
// different reasons -- authMiddleware because callbacks authenticate by HMAC,
// idempotencyHeaderAliasMiddleware because that HMAC covers the raw body,
// which must therefore not be rewritten -- and they used two independent
// copies of the literal "/api/v1/webhooks/".
//
// Edit one and not the other and you get either a body rewritten under a
// signature that no longer matches it, or an authenticated route that silently
// stops accepting the Idempotency-Key header. Both are silent. They now share
// webhookPathPrefix; this asserts the value still describes the route that
// actually exists, so the constant cannot drift from the router either.
func TestRouteAuthorization_ExemptPrefixesAgreeAcrossMiddleware(t *testing.T) {
	s := &Server{router: chi.NewRouter()}
	s.setupRoutes()

	var webhookRoutes, holderRoutes int
	require.NoError(t, chi.Walk(s.router, func(_, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = normalizeWalkRoute(route)
		if strings.HasPrefix(route, webhookPathPrefix) {
			webhookRoutes++
		}
		if strings.HasPrefix(route, holderPathPrefix) {
			holderRoutes++
		}
		return nil
	}))

	assert.Positive(t, webhookRoutes, "webhookPathPrefix %q matches no registered route -- the auth and idempotency exemptions are both dead", webhookPathPrefix)
	assert.Positive(t, holderRoutes, "holderPathPrefix %q matches no registered route -- the auth exemption is dead", holderPathPrefix)

	// The prefixes must not overlap the surfaces they are NOT meant to cover.
	// "/api/v1/holders/{holder}/..." (plural) and "/api/v1/holder-tokens" are
	// the two near-misses that make the trailing slash load-bearing.
	assert.False(t, isUnauthenticatedPath("/api/v1/holders/42/deposit-address"),
		"the plural /holders/ read surface is API-key authenticated and must not fall under the holder-token exemption")
	assert.False(t, isUnauthenticatedPath("/api/v1/holder-tokens"),
		"minting a holder token must require an API key -- if this prefix matched, anyone could mint one")
	assert.True(t, isUnauthenticatedPath("/api/v1/holder/balances"))
}

// normalizeWalkRoute undoes chi's trailing-slash annotation on subrouter
// mounts so a route reads the way it was registered.
func normalizeWalkRoute(route string) string {
	if len(route) > 1 {
		return strings.TrimSuffix(route, "/")
	}
	return route
}

// middlewareNames resolves each middleware in a chi route's chain to the name
// of the function that produced it (e.g. "...(*Server).requireScope.func1").
//
// Reflection rather than a marker interface because the alternative is
// production code carrying a type whose only purpose is to be recognized by a
// test. The failure mode of getting this wrong is safe: an unresolvable name
// matches nothing, so routes read as unguarded and the gate goes red.
func middlewareNames(middlewares []func(http.Handler) http.Handler) []string {
	names := make([]string, 0, len(middlewares))
	for _, mw := range middlewares {
		fn := runtime.FuncForPC(reflect.ValueOf(mw).Pointer())
		if fn == nil {
			names = append(names, "<unresolvable>")
			continue
		}
		names = append(names, fn.Name())
	}
	return names
}

func chainContains(names []string, want string) bool {
	for _, n := range names {
		if strings.Contains(n, want) {
			return true
		}
	}
	return false
}
