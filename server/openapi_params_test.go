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

	"github.com/stretchr/testify/assert"
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
//
// required and goType are M-8 (W3 adversarial review of the gates): the
// parameter gate compared NAME SETS only. The reviewer flipped
// GET /snapshots' currency_uid from required to optional and retyped it from
// {string, uuid} to {integer, int64} -- the spec's `required` field was
// parsed into specParam and then never asserted on, and
// EveryParamHasTypedSchema only checked that SOME type key was present. A
// client generated from that spec omits a parameter the handler refuses
// without, and sends an integer where it reads a uuid string: exactly the
// user-visible failure H-M1 was about, reachable again from the other side.
type handlerParams struct {
	query map[string]bool
	path  map[string]bool
	calls map[string]bool
	// required holds the parameters the handler itself refuses to proceed
	// without: it tests the value and answers ErrBadRequest.
	required map[string]bool
	// goType maps a parameter to the JSON Schema type its Go parse implies
	// ("integer" for strconv.ParseInt, "number" for ParseFloat, "boolean"
	// for ParseBool, "string" otherwise).
	goType map[string]string
}

func newHandlerParams() *handlerParams {
	return &handlerParams{
		query:    map[string]bool{},
		path:     map[string]bool{},
		calls:    map[string]bool{},
		required: map[string]bool{},
		goType:   map[string]string{},
	}
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
	helpers := parseHelperTypes(files)
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
			out[key] = collectRequestParams(fn.Body, helpers)
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
func collectRequestParams(body *ast.BlockStmt, helpers map[string]string) *handlerParams {
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

	// Pass 3: bind identifiers to the parameter they were read from, so a
	// later test of that identifier can be attributed back to the parameter.
	// Both shapes this package uses are covered:
	//   uid := q.Get("currency_uid")
	//   holder, err := strconv.ParseInt(q.Get("holder"), 10, 64)
	paramOfIdent := map[string]string{}
	ast.Inspect(body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) == 0 || len(assign.Rhs) != 1 {
			return true
		}
		names := paramNamesIn(assign.Rhs[0], valuesIdents)
		if len(names) != 1 {
			return true
		}
		if id, ok := assign.Lhs[0].(*ast.Ident); ok && id.Name != "_" {
			paramOfIdent[id.Name] = names[0]
		}
		return true
	})

	// Pass 4: a parameter is REQUIRED when the handler tests it for ABSENCE
	// and directly refuses.
	//
	// Both halves are load-bearing, and getting either wrong reads a handler
	// backwards:
	//
	//   - absence, not validity. `if h := q.Get("holder"); h != "" { ...
	//     ErrBadRequest ... }` (handleListReservations) refuses an
	//     unparseable holder while treating a missing one as "no filter" --
	//     an optional parameter with a validated value, not a required one.
	//     So only an equality against "" or 0 counts, never a `!=` presence
	//     test.
	//   - directly, meaning the refusal is a statement of that if's own
	//     block rather than nested in a further condition, which is what
	//     keeps the outer `if present { if invalid { refuse } }` shape from
	//     reading as a rejection of absence.
	//   - and only at the handler's TOP level. handleListAuditJournals
	//     refuses `holder == 0 || currencyUID == ""` inside a switch case
	//     that already established one of them was supplied -- "provide both
	//     or neither", which makes each one optional on its own. A nested
	//     rejection reads as "not required" here, and goes red against a
	//     spec that marks it required: the direction that asks a human.
	for _, stmt := range body.List {
		ifStmt, ok := stmt.(*ast.IfStmt)
		if !ok || !directlyRefusesWithBadRequest(ifStmt.Body) {
			continue
		}
		for _, name := range absenceTestedParams(ifStmt.Cond, valuesIdents, paramOfIdent) {
			hp.required[name] = true
		}
	}

	// Pass 5: the JSON Schema type each parameter's Go parse implies --
	// directly (strconv), through a package-local parse helper
	// (parseIDParam, which is how every {holder} path parameter is read),
	// or through a comparison against "true"/"false" (how this package
	// spells a boolean query flag).
	assign := func(arg ast.Expr, jsonType string) {
		for _, name := range paramNamesIn(arg, valuesIdents) {
			hp.goType[name] = jsonType
		}
		if id, ok := arg.(*ast.Ident); ok {
			if param, ok := paramOfIdent[id.Name]; ok {
				hp.goType[param] = jsonType
			}
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CallExpr:
			jsonType := ""
			switch fun := v.Fun.(type) {
			case *ast.SelectorExpr:
				if pkg, ok := fun.X.(*ast.Ident); ok && pkg.Name == "strconv" {
					jsonType = strconvJSONType(fun.Sel.Name)
				}
			case *ast.Ident:
				jsonType = helpers[fun.Name]
			}
			if jsonType == "" || len(v.Args) == 0 {
				return true
			}
			for _, arg := range v.Args {
				assign(arg, jsonType)
			}
		case *ast.BinaryExpr:
			if v.Op != token.EQL && v.Op != token.NEQ {
				return true
			}
			for _, pair := range [][2]ast.Expr{{v.X, v.Y}, {v.Y, v.X}} {
				if lit, ok := stringLit(pair[1]); ok && (lit == "true" || lit == "false") {
					assign(pair[0], "boolean")
				}
			}
		}
		return true
	})
	return hp
}

func strconvJSONType(fn string) string {
	switch fn {
	case "ParseInt", "ParseUint", "Atoi":
		return "integer"
	case "ParseFloat":
		return "number"
	case "ParseBool":
		return "boolean"
	default:
		return ""
	}
}

// parseHelperTypes maps a package-local function to the JSON Schema type its
// own body's strconv call implies, so `parseIDParam(chi.URLParam(r,
// "holder"))` types `holder` as integer the same way an inline
// strconv.ParseInt would. Derived, not listed: a second parse helper needs
// no edit here.
func parseHelperTypes(files []*ast.File) map[string]string {
	out := map[string]string{}
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv != nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "strconv" {
					if jsonType := strconvJSONType(sel.Sel.Name); jsonType != "" {
						out[fn.Name.Name] = jsonType
					}
				}
				return true
			})
		}
	}
	return out
}

// paramNamesIn returns the request-parameter names read anywhere inside expr
// (`q.Get("x")`, `r.URL.Query().Get("x")`, `chi.URLParam(r, "x")`).
func paramNamesIn(node ast.Node, valuesIdents map[string]bool) []string {
	var out []string
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "Get":
			if len(call.Args) == 1 && (isQueryCall(sel.X) || isIdentIn(sel.X, valuesIdents)) {
				if lit, ok := stringLit(call.Args[0]); ok {
					out = append(out, lit)
				}
			}
		case "URLParam":
			if len(call.Args) == 2 {
				if lit, ok := stringLit(call.Args[1]); ok {
					out = append(out, lit)
				}
			}
		}
		return true
	})
	return out
}

// directlyRefusesWithBadRequest reports whether a block answers a 4xx as one
// of its OWN statements -- the shape every parameter rejection in this
// package takes:
//
//	if uid == "" {
//	    httpx.Error(w, httpx.ErrBadRequest("currency_uid is required"))
//	    return
//	}
//
// Nested statements do not count: see pass 4's note.
func directlyRefusesWithBadRequest(block *ast.BlockStmt) bool {
	if block == nil {
		return false
	}
	for _, stmt := range block.List {
		expr, ok := stmt.(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := expr.X.(*ast.CallExpr)
		if !ok {
			continue
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "httpx" {
			switch sel.Sel.Name {
			case "ErrBadRequest", "Error":
				return true
			}
		}
	}
	return false
}

// absenceTestedParams returns the parameters an if-condition tests for
// ABSENCE: `p == ""`, `p == 0` for a value parsed out of one (a holder of 0
// is how this package spells "not supplied", since 0 is not a valid account
// holder), or `p.IsZero()` for a time parsed out of one.
func absenceTestedParams(cond ast.Expr, valuesIdents map[string]bool, paramOfIdent map[string]string) []string {
	var out []string
	add := func(e ast.Expr) {
		out = append(out, paramNamesIn(e, valuesIdents)...)
		if id, ok := e.(*ast.Ident); ok {
			if param, ok := paramOfIdent[id.Name]; ok {
				out = append(out, param)
			}
		}
	}
	ast.Inspect(cond, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.BinaryExpr:
			if v.Op != token.EQL {
				return true
			}
			for _, pair := range [][2]ast.Expr{{v.X, v.Y}, {v.Y, v.X}} {
				if isEmptyStringOrZero(pair[1]) {
					add(pair[0])
				}
			}
		case *ast.CallExpr:
			if sel, ok := v.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "IsZero" && len(v.Args) == 0 {
				add(sel.X)
			}
		}
		return true
	})
	return out
}

func isEmptyStringOrZero(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok {
		return false
	}
	switch lit.Kind {
	case token.STRING:
		s, err := strconv.Unquote(lit.Value)
		return err == nil && s == ""
	case token.INT:
		return lit.Value == "0"
	default:
		return false
	}
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
	resolved := resolveHandlerParams(fns, name)
	return resolved.query, resolved.path
}

// resolveHandlerParams is resolveParams plus the required set and the
// implied types, unioned over the same transitive closure.
func resolveHandlerParams(fns map[string]*handlerParams, name string) *handlerParams {
	out := newHandlerParams()
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
				out.query[k] = true
			}
			for k := range hp.path {
				out.path[k] = true
			}
			for k := range hp.required {
				out.required[k] = true
			}
			for k, v := range hp.goType {
				out.goType[k] = v
			}
			for callee := range hp.calls {
				walk(callee)
			}
		}
	}
	walk(name)
	return out
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

// TestOpenAPIContract_ParamRequirednessMatchesHandlers is M-8's first half:
// `required` was parsed out of the spec and never asserted on.
//
// A parameter the handler refuses to proceed without must be `required: true`
// in the spec (a generated client that omits it gets 400 on every call), and
// one the handler treats as optional must not be marked required (a client
// that cannot omit it, and a reader who believes a default does not exist).
// The handler side is derived from the AST: a parameter is required when the
// handler tests it and answers ErrBadRequest.
func TestOpenAPIContract_ParamRequirednessMatchesHandlers(t *testing.T) {
	_, files := parsePackageFiles(t)
	fns := funcRequestParams(t, files)
	routes := routeHandlerNames(t, files)
	params := operationParams(t, loadOpenAPIPaths(t))

	for key, handler := range routes {
		specList, ok := params[key]
		if !ok {
			continue // EveryRouteIsDocumented owns that failure
		}
		resolved := resolveHandlerParams(fns, handler)

		t.Run(key, func(t *testing.T) {
			for _, p := range specList {
				switch p.in {
				case "path":
					// OpenAPI requires path parameters to be required, and a
					// handler cannot route without them.
					assert.Truef(t, p.required,
						"%s: path parameter %q must be `required: true` -- OpenAPI has no optional path parameters, and the route does not match without it",
						key, p.name)
				case "query":
					assert.Equalf(t, resolved.required[p.name], p.required,
						"%s: docs/openapi.yaml says query parameter %q is required=%v, but handler %s %s.\n"+
							"A spec-generated client omits what the spec calls optional; if the handler answers 400 without it, every call fails "+
							"(H-M1's user-visible shape). Fix whichever side is wrong -- they are one contract",
						key, p.name, p.required, handler,
						map[bool]string{true: "refuses the request without it", false: "does not require it"}[resolved.required[p.name]])
				}
			}
		})
	}
}

// TestOpenAPIContract_ParamSchemaTypesMatchHandlers is M-8's second half.
// EveryParamHasTypedSchema only asks whether a `type` key exists; it never
// compares that type with what the Go handler does with the value. The
// reviewer retyped a uuid-string parameter to {integer, int64} and it passed.
//
// The Go side is derived from the parse: strconv.ParseInt/Atoi implies
// integer, ParseFloat number, ParseBool boolean, and anything the handler
// uses as-is is a string.
func TestOpenAPIContract_ParamSchemaTypesMatchHandlers(t *testing.T) {
	_, files := parsePackageFiles(t)
	fns := funcRequestParams(t, files)
	routes := routeHandlerNames(t, files)
	params := operationParams(t, loadOpenAPIPaths(t))
	schemas := loadOpenAPISchemas(t)

	for key, handler := range routes {
		specList, ok := params[key]
		if !ok {
			continue
		}
		resolved := resolveHandlerParams(fns, handler)

		t.Run(key, func(t *testing.T) {
			for _, p := range specList {
				if p.in != "query" && p.in != "path" {
					continue
				}
				node := p.schema
				if node == nil {
					continue // EveryParamHasTypedSchema owns that failure
				}
				if ref, ok := node["$ref"].(string); ok {
					target, ok := schemas[refName(ref)].(map[string]any)
					if !ok {
						continue // EveryParamHasTypedSchema owns it
					}
					node = target
				}
				specType, _ := node["type"].(string)
				if specType == "" {
					continue
				}
				want := resolved.goType[p.name]
				if want == "" {
					want = "string" // read and used as-is
				}
				assert.Equalf(t, want, specType,
					"%s: docs/openapi.yaml types %s parameter %q as %q, but handler %s reads it as %q.\n"+
						"A client generated from the spec serializes the documented type: an integer where the handler expects a uuid string "+
						"is a 400 on every call (H-M1), and the reverse silently truncates",
					key, p.in, p.name, specType, handler, want)
			}
		})
	}
}
