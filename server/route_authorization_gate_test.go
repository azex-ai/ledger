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
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"sort"
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

// --- C-1 (W3 adversarial review of the gates): which scope, not just "a scope" ---
//
// TestRouteAuthorization_EveryRouteIsGuarded above asks whether a route's
// middleware chain contains requireScope/requireCapability at all. The
// reviewer moved PUT /accounts/{holder}/policy -- the only DB-enforced
// freeze / min_balance gate in the repo -- from the admin group into the
// write group and this package, core, and the root package all stayed
// green. "Has an authorization check" was machine-checked; "has the right
// one" was a hand-written four-route sample in server_test.go
// (TestAuth_ScopeEnforcement), and 8 of the 12 scoped route groups' members
// were not in it.
//
// So this derives, per route, the scope and capability the chain ACTUALLY
// enforces -- by running the chain, not by reading its function names -- and
// compares that against docs/openapi.yaml's per-operation x-required-scope /
// x-required-capability declarations.
//
// Two properties worth stating because they are what makes this a gate
// rather than a second copy of the route table:
//
//   - The expectation lives in the spec, not in this file. A consumer
//     needs to know which key class reaches which endpoint; that belongs in
//     the published contract. Changing a route's scope now means changing
//     the spec in the same commit, which is a reviewable diff.
//   - The observed side is behavioural. requireScope's closure carries the
//     required Scope in a captured variable that no amount of reflection
//     can read, so the derivation probes each chain with a synthetic
//     identity at each scope level and records the lowest one that gets
//     through. A refactor that keeps the name and drops the check is
//     therefore visible, which is the failure mode the name-based check
//     could never see.
//
// Fail-closed in both directions: a route the spec does not classify is an
// error, a spec declaration with no matching route is an error, and a chain
// no probe gets through is an error rather than a silent skip.

// allProbeCapabilities is every Capability the middleware layer knows about.
// A new Capability must be added here; the probe cannot enumerate a
// bit-flag type on its own, and leaving it out makes any route gated on it
// derive as "capability required, but no known capability satisfies it",
// which is red.
var allProbeCapabilities = []Capability{CapabilityDepositReview}

// routeRequirement is what a route's middleware chain enforces, as observed.
type routeRequirement struct {
	anonymousReaches bool       // nothing in the chain rejects a request with no identity
	scope            Scope      // lowest scope that gets through; 0 = no scope check
	caps             Capability // capabilities the chain demands; 0 = none
	unreachable      bool       // no probe got through: derivation failed, treat as red
}

func (r routeRequirement) scopeName() string {
	if r.scope == 0 {
		return ""
	}
	return r.scope.String()
}

func (r routeRequirement) capsName() string {
	if r.caps == 0 {
		return ""
	}
	return r.caps.String()
}

// authGuardsOnly keeps the middlewares that make authorization decisions and
// drops the rest, so the probe measures the authorization chain and not, say,
// a rate limiter's verdict. Selection is by resolved function name -- the
// only thing available -- but the VALUE being measured is behavioural, so a
// name that stops resolving drops the guard from the chain and the route
// then reads as reachable-by-anyone, which is red. Fail-closed.
func authGuardsOnly(middlewares []func(http.Handler) http.Handler) []func(http.Handler) http.Handler {
	out := make([]func(http.Handler) http.Handler, 0, len(middlewares))
	for _, mw := range middlewares {
		fn := runtime.FuncForPC(reflect.ValueOf(mw).Pointer())
		if fn == nil {
			continue
		}
		name := fn.Name()
		if strings.Contains(name, "requireScope") || strings.Contains(name, "requireCapability") {
			out = append(out, mw)
		}
	}
	return out
}

// deriveRouteRequirement runs a route's authorization middlewares against
// synthetic identities and reports what they enforce.
func deriveRouteRequirement(middlewares []func(http.Handler) http.Handler) routeRequirement {
	guards := authGuardsOnly(middlewares)

	reaches := func(id *authIdentity) bool {
		reached := false
		var h http.Handler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
		for i := len(guards) - 1; i >= 0; i-- {
			h = guards[i](h)
		}
		req := httptest.NewRequest(http.MethodGet, "/probe", nil)
		if id != nil {
			req = req.WithContext(context.WithValue(req.Context(), authIdentityCtxKey{}, *id))
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		return reached
	}

	var allCaps Capability
	for _, c := range allProbeCapabilities {
		allCaps |= c
	}

	out := routeRequirement{anonymousReaches: reaches(nil), unreachable: true}
	// Scope 0 is "an authenticated key that claims no level"; a route with no
	// requireScope in its chain lets it through, which is how "no scope
	// requirement" is distinguished from "read".
	for _, sc := range []Scope{0, ScopeRead, ScopeWrite, ScopeAdmin} {
		if reaches(&authIdentity{Name: "probe", Scope: sc, Capabilities: allCaps}) {
			out.scope, out.unreachable = sc, false
			break
		}
	}
	// Capabilities are orthogonal to the ladder, so probe them at admin.
	if !out.unreachable && !reaches(&authIdentity{Name: "probe", Scope: ScopeAdmin}) {
		for _, c := range allProbeCapabilities {
			if reaches(&authIdentity{Name: "probe", Scope: ScopeAdmin, Capabilities: c}) {
				out.caps |= c
			}
		}
		if out.caps == 0 {
			// Something in the chain demands a capability the probe does not
			// know about. Not a skip: allProbeCapabilities needs the new bit.
			out.unreachable = true
		}
	}
	return out
}

// specAuthorization is one operation's declared authorization in
// docs/openapi.yaml.
type specAuthorization struct {
	scope       string // x-required-scope: read|write|admin, "" if absent
	capability  string // x-required-capability: deposit_review, "" if absent
	schemes     []string
	securitySet bool // the operation carries its own `security:` key
}

// specRouteAuthorization reads every operation in docs/openapi.yaml and
// returns its declared authorization, keyed "METHOD /path" the way
// enumerateRoutes spells it (i.e. without the /api/v1 server prefix).
func specRouteAuthorization(t *testing.T) map[string]specAuthorization {
	t.Helper()
	out := map[string]specAuthorization{}
	for pathKey, methodsAny := range loadOpenAPIPaths(t) {
		methods, ok := methodsAny.(map[string]any)
		if !ok {
			continue
		}
		for method, opAny := range methods {
			m := strings.ToUpper(method)
			switch m {
			case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			default:
				continue
			}
			op, ok := opAny.(map[string]any)
			if !ok {
				continue
			}
			decl := specAuthorization{}
			decl.scope, _ = op["x-required-scope"].(string)
			decl.capability, _ = op["x-required-capability"].(string)
			if sec, ok := op["security"]; ok {
				decl.securitySet = true
				list, _ := sec.([]any)
				for _, entryAny := range list {
					entry, ok := entryAny.(map[string]any)
					if !ok {
						continue
					}
					for scheme := range entry {
						decl.schemes = append(decl.schemes, scheme)
					}
				}
				sort.Strings(decl.schemes)
			}
			out[m+" "+pathKey] = decl
		}
	}
	require.NotEmpty(t, out, "docs/openapi.yaml declared no operations -- the spec side of this gate read nothing")
	return out
}

func TestRouteAuthorization_RequiredScopeMatchesOpenAPISpec(t *testing.T) {
	spec := specRouteAuthorization(t)

	s := &Server{router: chi.NewRouter(), authEnabled: true}
	s.setupRoutes()

	matched := map[string]bool{}
	var scoped int

	require.NoError(t, chi.Walk(s.router, func(method, route string, _ http.Handler, middlewares ...func(http.Handler) http.Handler) error {
		route = normalizeWalkRoute(route)
		specKey := method + " " + strings.TrimPrefix(route, "/api/v1")
		decl, documented := spec[specKey]
		if !documented {
			// TestOpenAPIContract_EveryRouteIsDocumented owns "route missing
			// from the spec"; not repeating its message, but not skipping
			// either -- an undocumented route has no declared scope, so this
			// gate cannot check it and says so.
			t.Errorf("route %q is not in docs/openapi.yaml, so it declares no x-required-scope and this gate cannot check its authorization level", specKey)
			return nil
		}
		matched[specKey] = true
		got := deriveRouteRequirement(middlewares)

		if strings.HasPrefix(route, holderPathPrefix) {
			// Holder-token surface: no API-key scope applies. The scope check
			// is TestRouteAuthorization_EveryRouteIsGuarded's holder branch;
			// here we only pin that the spec says so too, since a reader of
			// the spec picks a credential from `security`.
			assert.Equal(t, []string{"holderToken"}, decl.schemes,
				"route %q is in the holder-token group but docs/openapi.yaml does not declare `security: [holderToken: []]` for it -- a spec reader would present an API key and get 401", specKey)
			assert.Empty(t, decl.scope, "route %q is holder-token authenticated; x-required-scope does not apply to it", specKey)
			return nil
		}

		if isUnauthenticatedPath(route) {
			assert.True(t, decl.securitySet && len(decl.schemes) == 0,
				"route %q is exempt from bearer auth by isUnauthenticatedPath but docs/openapi.yaml does not declare `security: []` for it", specKey)
			assert.Empty(t, decl.scope, "route %q answers without a credential; x-required-scope does not apply to it", specKey)
			assert.True(t, got.anonymousReaches,
				"route %q is spec'd as unauthenticated yet its middleware chain rejects an anonymous request", specKey)
			return nil
		}

		require.False(t, got.unreachable,
			"route %q: no probe identity got through its authorization chain, so the enforced scope could not be derived. "+
				"Either the chain demands a Capability missing from allProbeCapabilities, or a new guard shape needs teaching to authGuardsOnly. "+
				"This is deliberately an error and not a skip: an undeterminable gate is not a passing one", specKey)
		require.False(t, got.anonymousReaches,
			"route %q lets a request with no authenticated identity through its chain", specKey)

		require.True(t, decl.scope != "" || decl.capability != "",
			"route %q declares neither x-required-scope nor x-required-capability in docs/openapi.yaml. Every API-key route must declare what it needs "+
				"(observed: scope=%q capability=%q) -- a new route defaults to unclassified, which is red, the same contract "+
				"postgres/grant_coverage_test.go's table classification uses", specKey, got.scopeName(), got.capsName())
		assert.Equal(t, decl.scope, got.scopeName(),
			"route %q: docs/openapi.yaml declares x-required-scope: %s but the router enforces %q. "+
				"A scope DOWNGRADE (e.g. moving PUT /accounts/{holder}/policy out of the admin group) is exactly what this gate exists to catch -- "+
				"if the change is intended, the spec is the place to say so", specKey, decl.scope, got.scopeName())
		assert.Equal(t, decl.capability, got.capsName(),
			"route %q: docs/openapi.yaml declares x-required-capability: %q but the router enforces %q", specKey, decl.capability, got.capsName())
		scoped++
		return nil
	}))

	// Stale spec declarations: an x-required-scope on an operation no router
	// serves means somebody deleted or renamed a route and left the contract
	// promising it.
	var stale []string
	for key, decl := range spec {
		if matched[key] || (decl.scope == "" && decl.capability == "") {
			continue
		}
		stale = append(stale, key)
	}
	sort.Strings(stale)
	assert.Empty(t, stale, "docs/openapi.yaml declares x-required-scope for operation(s) the chi router does not serve: %v", stale)

	assert.Positive(t, scoped, "sanity: no API-key route was classified, so this gate checked nothing")
}
