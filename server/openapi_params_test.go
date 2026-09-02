// Package server: openapi_params_test.go
//
// H-M1 (2026-09-02 deep audit): docs/openapi.yaml's query/path `parameters`
// were entirely outside every contract gate. openapi_contract_test.go reads
// `components.schemas` and `paths.*.responses` only -- the word
// "parameters" appears nowhere in it -- so three confirmed drifts lived in
// the spec indefinitely, one of which (GET /snapshots) made a
// spec-generated client fail with 400 on every call: it documents
// `currency`/`from`/`to` while the handler reads
// `currency_uid`/`start`/`end`.
//
// This file closes that direction by deriving BOTH sides from artifacts:
//
//   - spec side: the `parameters` array of every operation in
//     docs/openapi.yaml.
//   - Go side: go/ast over this package's non-test sources, collecting the
//     string literals each handler passes to `r.URL.Query().Get(...)` and
//     `chi.URLParam(r, ...)`, transitively through package-local helper
//     calls (parsePageLimit's "limit" belongs to each of its callers, not
//     to response.go).
//
// The route -> handler mapping also comes from an artifact: setupRoutes'
// own AST. To keep that parse from silently diverging from what chi
// actually serves, TestOpenAPIContract_RouteTableMatchesChiRouter asserts
// the AST-derived route set equals enumerateRoutes' chi.Walk result. If
// someone registers a route in a way this parser cannot see, that test goes
// red instead of the parameter check quietly skipping the route
// (working-agreements §3: not-run must not read as pass).
package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// --- spec side ---

type specParam struct {
	in       string
	name     string
	required bool
	schema   map[string]any
}

// operationParams returns every operation's declared parameters, keyed
// "METHOD /path" exactly as enumerateRoutes and the spec's own path keys
// spell it.
func operationParams(t *testing.T, paths map[string]any) map[string][]specParam {
	t.Helper()
	out := map[string][]specParam{}
	for pathKey, methodsAny := range paths {
		methods, ok := methodsAny.(map[string]any)
		if !ok {
			continue
		}
		for method, opAny := range methods {
			m := strings.ToUpper(method)
			switch m {
			case "GET", "POST", "PUT", "PATCH", "DELETE":
			default:
				continue
			}
			op, ok := opAny.(map[string]any)
			if !ok {
				continue
			}
			key := m + " " + pathKey
			params, _ := op["parameters"].([]any)
			list := make([]specParam, 0, len(params))
			for _, pAny := range params {
				p, ok := pAny.(map[string]any)
				require.True(t, ok, "%s: parameters member is not a mapping", key)
				schema, _ := p["schema"].(map[string]any)
				in, _ := p["in"].(string)
				name, _ := p["name"].(string)
				req, _ := p["required"].(bool)
				list = append(list, specParam{in: in, name: name, required: req, schema: schema})
			}
			out[key] = list
		}
	}
	return out
}

func specParamNames(params []specParam, in string) map[string]bool {
	out := map[string]bool{}
	for _, p := range params {
		if p.in == in {
			out[p.name] = true
		}
	}
	return out
}

// --- Go side ---

// handlerParams is what a handler function reads out of the request line.
type handlerParams struct {
	query map[string]bool
	path  map[string]bool
	calls map[string]bool
}

func newHandlerParams() *handlerParams {
	return &handlerParams{query: map[string]bool{}, path: map[string]bool{}, calls: map[string]bool{}}
}

// parsePackageFiles parses every non-test .go file in the server package.
func parsePackageFiles(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err, "read server package dir")
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// ParseComments matters: config_coverage_test.go reads Config field
		// doc comments through this helper, and without it every field's Doc
		// is nil -- which would make that gate pass vacuously rather than
		// fail loudly (it did, until this flag was added).
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ParseComments|parser.SkipObjectResolution)
		require.NoError(t, err, "parse %s", name)
		files = append(files, f)
	}
	require.NotEmpty(t, files, "server package has no non-test sources")
	return fset, files
}

// funcRequestParams maps every function/method in the package to the request
// parameters its own body reads, plus the package-local functions it calls
// (resolved transitively by resolveParams).
//
// Keys are receiver-qualified ("(*Server).handleListEntries") because the
// package legitimately declares several same-named methods (String).
// Call sites, however, only give a selector name, so resolveParams looks a
// callee up by bare name and unions every declaration matching it. That
// union can only over-approximate a handler's reads (making the gate
// stricter, never blind), and the same-named methods in this package read no
// request parameters at all.
func funcRequestParams(t *testing.T, files []*ast.File) map[string]*handlerParams {
	t.Helper()
	out := map[string]*handlerParams{}
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := qualifiedFuncName(fn)
			_, dup := out[key]
			require.False(t, dup, "server package declares %q twice", key)
			out[key] = collectRequestParams(fn.Body)
		}
	}
	return out
}

// qualifiedFuncName renders a FuncDecl as "name" or "(recv).name".
func qualifiedFuncName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	return "(" + exprTypeName(fn.Recv.List[0].Type) + ")." + fn.Name.Name
}

func exprTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return "*" + exprTypeName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return exprTypeName(t.X)
	default:
		return "?"
	}
}

// bareName strips the receiver qualifier from a funcRequestParams key.
func bareName(key string) string {
	if i := strings.LastIndex(key, ")."); i >= 0 {
		return key[i+2:]
	}
	return key
}

// collectRequestParams walks one function body. Query reads are matched on
// the receiver of .Get(...) being a url.Values -- either an inline
// `...Query().Get("k")` chain or an identifier previously bound to a
// `...Query()` call (`q := r.URL.Query()`), which is how most handlers here
// spell it. That receiver test is what keeps http.Header's identically
// named Get out of the result.
func collectRequestParams(body *ast.BlockStmt) *handlerParams {
	hp := newHandlerParams()
	valuesIdents := map[string]bool{}

	// Pass 1: bind identifiers assigned from a Query() call.
	ast.Inspect(body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, rhs := range assign.Rhs {
			if i >= len(assign.Lhs) || !isQueryCall(rhs) {
				continue
			}
			if id, ok := assign.Lhs[i].(*ast.Ident); ok {
				valuesIdents[id.Name] = true
			}
		}
		return true
	})

	// Pass 2: collect the literals.
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			switch fun.Sel.Name {
			case "Get":
				if len(call.Args) != 1 {
					break
				}
				if !isQueryCall(fun.X) && !isIdentIn(fun.X, valuesIdents) {
					break
				}
				if lit, ok := stringLit(call.Args[0]); ok {
					hp.query[lit] = true
				}
			case "URLParam":
				if len(call.Args) == 2 {
					if lit, ok := stringLit(call.Args[1]); ok {
						hp.path[lit] = true
					}
				}
			default:
				hp.calls[fun.Sel.Name] = true
			}
		case *ast.Ident:
			hp.calls[fun.Name] = true
		}
		return true
	})
	return hp
}

func isQueryCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Query"
}

func isIdentIn(e ast.Expr, set map[string]bool) bool {
	id, ok := e.(*ast.Ident)
	return ok && set[id.Name]
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// resolveParams returns the transitive closure of a function's request
// parameter reads over package-local calls.
func resolveParams(fns map[string]*handlerParams, name string) (query, path map[string]bool) {
	query, path = map[string]bool{}, map[string]bool{}
	seen := map[string]bool{}
	var walk func(string)
	walk = func(n string) {
		if seen[n] {
			return
		}
		seen[n] = true
		for key, hp := range fns {
			if bareName(key) != n {
				continue
			}
			for k := range hp.query {
				query[k] = true
			}
			for k := range hp.path {
				path[k] = true
			}
			for callee := range hp.calls {
				walk(callee)
			}
		}
	}
	walk(name)
	return query, path
}

// --- route table, derived from setupRoutes' AST ---

var routeMethods = map[string]string{
	"Get":    "GET",
	"Post":   "POST",
	"Put":    "PUT",
	"Patch":  "PATCH",
	"Delete": "DELETE",
}

// routeHandlerNames parses setupRoutes and returns "METHOD /path" ->
// handler function name. The /api/v1 prefix comes from the enclosing
// r.Route(...) call and is deliberately dropped: openapi.yaml's path keys
// are server-relative, and enumerateRoutes strips the same prefix.
func routeHandlerNames(t *testing.T, files []*ast.File) map[string]string {
	t.Helper()
	out := map[string]string{}
	var setup *ast.FuncDecl
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Name.Name == "setupRoutes" {
				setup = fn
			}
		}
	}
	require.NotNil(t, setup, "setupRoutes not found in the server package")

	ast.Inspect(setup.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		method, ok := routeMethods[sel.Sel.Name]
		if !ok {
			return true
		}
		route, ok := stringLit(call.Args[0])
		if !ok {
			return true
		}
		handler := handlerNameIn(call.Args[1])
		require.NotEmpty(t, handler, "setupRoutes: %s %s registers a handler expression this gate cannot name; register it as s.handleX (optionally wrapped) so the route can be mapped to its parameter reads", method, route)
		key := method + " " + route
		_, dup := out[key]
		require.False(t, dup, "setupRoutes registers %s twice", key)
		out[key] = handler
		return true
	})
	return out
}

// handlerNameIn finds the single handle* selector inside a handler
// expression -- `s.handleFoo` directly, or wrapped as
// `s.withHolderSurface((*holderSurface).handleFoo)`.
func handlerNameIn(e ast.Expr) string {
	var found []string
	ast.Inspect(e, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if ok && strings.HasPrefix(sel.Sel.Name, "handle") {
			found = append(found, sel.Sel.Name)
		}
		return true
	})
	if len(found) != 1 {
		return ""
	}
	return found[0]
}

// TestOpenAPIContract_RouteTableMatchesChiRouter is the fail-closed guard on
// routeHandlerNames: the AST parse must see exactly the routes chi serves.
// Without it, a route registered in a shape handlerNameIn cannot read (or a
// path built by concatenation) would drop out of the parameter check
// silently.
func TestOpenAPIContract_RouteTableMatchesChiRouter(t *testing.T) {
	_, files := parsePackageFiles(t)
	fromAST := routeHandlerNames(t, files)

	// enumerateRoutes drops the two unauthenticated probes; add them back so
	// the two sets are directly comparable.
	served := enumerateRoutes(t)
	served["GET /system/health"] = true
	served["GET /system/ready"] = true

	var astOnly, chiOnly []string
	for k := range fromAST {
		if !served[k] {
			astOnly = append(astOnly, k)
		}
	}
	for k := range served {
		if _, ok := fromAST[k]; !ok {
			chiOnly = append(chiOnly, k)
		}
	}
	sort.Strings(astOnly)
	sort.Strings(chiOnly)
	require.Empty(t, chiOnly, "route(s) served by chi but invisible to routeHandlerNames' AST parse -- the parameter gate would skip them silently")
	require.Empty(t, astOnly, "route(s) parsed out of setupRoutes but not served by chi -- routeHandlerNames is reading something that is not a route registration")
}

// TestOpenAPIContract_ParamsMatchGoHandlers is H-M1's pin. Both directions
// are real drift: a documented parameter the handler never reads is a client
// that gets 400 (GET /snapshots today), and a parameter the handler reads
// but the spec omits is a required input no generated client can supply
// (GET /balances/{holder} today).
func TestOpenAPIContract_ParamsMatchGoHandlers(t *testing.T) {
	_, files := parsePackageFiles(t)
	fns := funcRequestParams(t, files)
	routes := routeHandlerNames(t, files)
	params := operationParams(t, loadOpenAPIPaths(t))

	// Probes are deliberately undocumented (see enumerateRoutes).
	undocumented := map[string]bool{"GET /system/health": true, "GET /system/ready": true}

	for key, handler := range routes {
		if undocumented[key] {
			continue
		}
		specList, ok := params[key]
		if !ok {
			// EveryRouteIsDocumented owns this failure; do not double-report.
			continue
		}
		t.Run(key, func(t *testing.T) {
			goQuery, goPath := resolveParams(fns, handler)
			specQuery := specParamNames(specList, "query")
			assertParamSetsMatch(t, key+" (query)", specQuery, goQuery)

			// Path parameters have a third artifact to agree with: the path
			// template itself. Check both edges -- spec-declared vs template,
			// and handler-read vs template -- so a handler reading a
			// placeholder that no longer exists is caught too.
			templatePath := pathTemplateParams(key)
			assertParamSetsMatch(t, key+" (path, spec vs URL template)", specParamNames(specList, "path"), templatePath)
			for name := range goPath {
				require.True(t, templatePath[name], "%s: handler %s reads chi.URLParam(%q) but the route template declares no such placeholder", key, handler, name)
			}
		})
	}
}

// pathTemplateParams extracts the {placeholder} names from a "METHOD /path"
// key.
func pathTemplateParams(routeKey string) map[string]bool {
	out := map[string]bool{}
	_, path, _ := strings.Cut(routeKey, " ")
	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			out[strings.TrimSuffix(strings.TrimPrefix(seg, "{"), "}")] = true
		}
	}
	return out
}

func assertParamSetsMatch(t *testing.T, label string, spec, actual map[string]bool) {
	t.Helper()
	var specOnly, actualOnly []string
	for k := range spec {
		if !actual[k] {
			specOnly = append(specOnly, k)
		}
	}
	for k := range actual {
		if !spec[k] {
			actualOnly = append(actualOnly, k)
		}
	}
	sort.Strings(specOnly)
	sort.Strings(actualOnly)
	if len(specOnly) > 0 || len(actualOnly) > 0 {
		t.Errorf("%s: docs/openapi.yaml and the Go handler disagree on parameter names.\n  documented in openapi.yaml but never read: %v\n  read by the handler but not documented: %v",
			label, specOnly, actualOnly)
	}
}

// TestOpenAPIContract_EveryParamHasTypedSchema keeps a parameter from being
// declared with no schema at all, which generates an `unknown`-typed
// argument downstream and would let the name check above pass on a
// parameter no client can serialize correctly.
func TestOpenAPIContract_EveryParamHasTypedSchema(t *testing.T) {
	params := operationParams(t, loadOpenAPIPaths(t))
	schemas := loadOpenAPISchemas(t)

	var bad []string
	for key, list := range params {
		for _, p := range list {
			if p.in != "query" && p.in != "path" {
				continue
			}
			node := p.schema
			if node == nil {
				bad = append(bad, key+" "+p.in+":"+p.name+" (no schema)")
				continue
			}
			if ref, ok := node["$ref"].(string); ok {
				target, ok := schemas[refName(ref)].(map[string]any)
				if !ok {
					bad = append(bad, key+" "+p.in+":"+p.name+" (unresolvable $ref "+ref+")")
					continue
				}
				node = target
			}
			if _, ok := node["type"]; !ok {
				bad = append(bad, key+" "+p.in+":"+p.name+" (schema has no type)")
			}
		}
	}
	sort.Strings(bad)
	require.Empty(t, bad, "parameter(s) declared without a usable schema type")
}
