package ledger_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readme_docs_consistency_test.go machine-checks the "数量" and "路径"
// assertions in README.md (and, for the File Layout table it shares the
// exact same failure shape with, CLAUDE.md) against the artifacts they
// describe. Every one of these was previously a hand-typed number or path
// that drifted the moment the thing it described changed (E-m2, E-m3, E-m4,
// E-m5; docs/audits/2026-09-02-deep-audit/TODO.md) -- the fix upstream from
// this test is "stop hand-typing it", but a few genuinely can't be phrased
// without a number (openapi paths/schemas, core.Metrics method count), so
// those get a gate instead.

// --- E-m2: core.Metrics method count -------------------------------------

func TestREADMEMetricsMethodCountMatchesInterface(t *testing.T) {
	count := countInterfaceMethods(t, "core/metrics.go", "Metrics")
	readme := mustReadFile(t, "README.md")
	// Tolerant of markdown line-wrapping between "(N" and "methods":
	// README.md's prose wraps at ~80 cols, so the literal phrase can span a
	// newline in the source even though it reads as one sentence.
	re := regexp.MustCompile(`\(` + strconv.Itoa(count) + `\s+methods`)
	if !re.MatchString(readme) {
		t.Errorf("core.Metrics has %d methods, but README.md does not contain a %q match -- update the Observability section (README.md, not core/metrics.go: the interface being wide is correct, only the README's count can drift)",
			count, re.String())
	}
}

// countInterfaceMethods parses goFile and returns the number of method
// signatures declared directly in the named interface's method set (not
// counting embedded interfaces, of which core.Metrics has none).
func countInterfaceMethods(t *testing.T, goFile, ifaceName string) int {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, goFile, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != ifaceName {
				continue
			}
			it, ok := ts.Type.(*ast.InterfaceType)
			if !ok {
				t.Fatalf("%s.%s is not an interface type", goFile, ifaceName)
			}
			n := 0
			for _, m := range it.Methods.List {
				if _, ok := m.Type.(*ast.FuncType); ok {
					n += len(m.Names)
				}
			}
			return n
		}
	}
	t.Fatalf("interface %s not found in %s", ifaceName, goFile)
	return 0
}

// --- E-m2: openapi.yaml paths/schemas count -------------------------------

func TestREADMEOpenAPICountsMatchSpec(t *testing.T) {
	spec := mustReadFile(t, "docs/openapi.yaml")
	paths := countOpenAPITopLevelKeys(t, spec, "paths:")
	schemas := countOpenAPITopLevelKeys(t, spec, "  schemas:")

	readme := mustReadFile(t, "README.md")
	want := "(" + strconv.Itoa(paths) + " paths, " + strconv.Itoa(schemas) + " schemas)"
	if !strings.Contains(readme, want) {
		t.Errorf("docs/openapi.yaml has %d paths and %d schemas, but README.md does not contain %q -- update the openapi.yaml doc-link line in README.md's Documentation section (this counts the spec; if the count is wrong go check docs/openapi.yaml, which is D-contract's, not this line)",
			paths, schemas, want)
	}
}

// countOpenAPITopLevelKeys counts YAML mapping keys nested exactly one
// indent level under a line equal to header, by counting lines matching
// `^    [A-Za-z0-9_./{}-]+:` (paths are 4-space indented under "paths:";
// schema names are 4-space indented under "  schemas:") until a line with
// less indentation ends the block. This is intentionally a light YAML walk,
// not a full parser -- docs/openapi.yaml's shape is stable and this mirrors
// server/openapi_contract_test.go's existing "cheap structural scan"
// convention for that file rather than adding a YAML library dependency.
func countOpenAPITopLevelKeys(t *testing.T, spec, header string) int {
	t.Helper()
	lines := strings.Split(spec, "\n")
	start := -1
	headerIndent := len(header) - len(strings.TrimLeft(header, " "))
	for i, l := range lines {
		if l == header {
			start = i
			break
		}
	}
	if start == -1 {
		t.Fatalf("openapi.yaml: no line equal to %q", header)
	}
	keyRE := regexp.MustCompile(`^` + strings.Repeat(" ", headerIndent+2) + `[^\s].*:\s*$`)
	count := 0
	for _, l := range lines[start+1:] {
		if strings.TrimSpace(l) == "" {
			continue
		}
		indent := len(l) - len(strings.TrimLeft(l, " "))
		if indent <= headerIndent {
			break // dedented back out of the block
		}
		if keyRE.MatchString(l) {
			count++
		}
	}
	return count
}

// --- E-m3: every path named in README's Architecture tree, and CLAUDE.md's
// File Layout table, actually exists -----------------------------------

func TestREADMEArchitectureTreePathsExist(t *testing.T) {
	readme := mustReadFile(t, "README.md")
	tree := extractFencedBlock(t, readme, "\nledger/\n")
	for _, p := range treePaths(t, tree) {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("README.md's Architecture tree names %q, which does not exist: %v", p, err)
		}
	}
}

// TestCLAUDEMdFileLayoutPathsExist lived here too, and skipped when
// CLAUDE.md was absent. m-2 (W3 adversarial review of the gates): the same
// judgement had two implementations with opposite failure policies -- the
// copy in claude_md_paths_test.go (package ledger) Fatals on a missing
// CLAUDE.md, this one passed -- and `go test -v` printed the same test name
// twice with different verdicts. One decision, one place: the fail-closed
// copy is the one that survives.

// extractMarkdownSection returns the text between a "## heading" line
// (exclusive) and the next "## " line (exclusive), or to EOF.
func extractMarkdownSection(doc, heading string) string {
	idx := strings.Index(doc, heading)
	if idx == -1 {
		return ""
	}
	rest := doc[idx+len(heading):]
	if next := strings.Index(rest, "\n## "); next != -1 {
		return rest[:next]
	}
	return rest
}

// extractFencedBlock returns the content of the first ``` fenced block
// (any language, including none) whose content contains anchor.
func extractFencedBlock(t *testing.T, doc, anchor string) string {
	t.Helper()
	re := regexp.MustCompile("(?s)```[a-z]*\n(.*?)\n```")
	for _, m := range re.FindAllStringSubmatch(doc, -1) {
		if strings.Contains("\n"+m[1]+"\n", anchor) {
			return m[1]
		}
	}
	t.Fatalf("no fenced block contains %q -- this test's anchor is stale", anchor)
	return ""
}

// treePaths walks an indented ASCII directory tree (README's Architecture
// diagram shape: "ledger/" at indent 0, then nested "  dir/" / "    file.go"
// lines, descriptions separated from the name by a run of 2+ spaces, and
// occasional bare comma-separated sibling files on one line) and returns
// every named path, repo-root-relative.
func treePaths(t *testing.T, tree string) []string {
	t.Helper()
	type frame struct {
		indent int
		path   string
	}
	stack := []frame{{indent: -1, path: "."}}
	var paths []string

	for _, raw := range strings.Split(tree, "\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " "))
		content := strings.TrimRight(raw, " ")[indent:]

		// The name field is the leading run of whitespace-delimited tokens
		// that end in a comma (a "fee.go, transfer.go, ..." sibling-file
		// list on one line, no description column), plus the one token
		// after that doesn't. This does not assume any particular gap width
		// between the name and its description -- some rows in this table
		// are aligned with a single space, most with several.
		fields := strings.Fields(content)
		if len(fields) == 0 {
			continue
		}
		var names []string
		for _, tok := range fields {
			if strings.HasSuffix(tok, ",") {
				names = append(names, strings.TrimSuffix(tok, ","))
				continue
			}
			names = append(names, tok)
			break
		}

		if len(names) == 1 && names[0] == "ledger/" {
			stack = []frame{{indent: indent, path: "."}}
			continue
		}

		for len(stack) > 1 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		parent := stack[len(stack)-1].path

		for _, name := range names {
			isDir := strings.HasSuffix(name, "/")
			clean := strings.TrimSuffix(name, "/")
			full := parent + "/" + clean
			paths = append(paths, full)
			if isDir && len(names) == 1 {
				stack = append(stack, frame{indent: indent, path: full})
			}
		}
	}
	sort.Strings(paths)
	return paths
}

// --- E-m4: go.work / go.mod version and module-list consistency ----------

func TestREADMEGoWorkGuideMatchesRepo(t *testing.T) {
	gomod := mustReadFile(t, "go.mod")
	goDirRE := regexp.MustCompile(`(?m)^go (\d+\.\d+\.\d+)$`)
	m := goDirRE.FindStringSubmatch(gomod)
	if m == nil {
		t.Fatal("go.mod: no `go X.Y.Z` directive found")
	}
	wantGo := m[1]

	readme := mustReadFile(t, "README.md")
	section := extractMarkdownSection(readme, "## Local Development with go.work")
	if !strings.Contains(section, "go "+wantGo) {
		t.Errorf("go.mod's go directive is %q, but README.md's \"Local Development with go.work\" section does not mention it -- a stale version there fails a reader's first command (E-m4)", wantGo)
	}

	gowork := mustReadFile(t, "go.work")
	repoModules := extractUseBlockEntries(t, gowork)

	// The README's use block additionally lists "your-consumer-module", the
	// reader's own project -- not a module this repo ships, so it does not
	// appear in go.work and must be excluded before comparing counts.
	var readmeModules []string
	for _, m := range extractUseBlockEntries(t, section) {
		if !strings.Contains(m, "your-consumer-module") {
			readmeModules = append(readmeModules, m)
		}
	}
	if len(readmeModules) != len(repoModules) {
		t.Errorf("go.work's use block lists %d modules %v, but README.md's example use block lists %d %v (excluding the your-consumer-module placeholder) -- keep the README's list in sync (it does not need identical paths, since the README's is written relative to a sibling directory, but the COUNT should match: every module in this repo's go.work needs a consumer's outer workspace to see it too)",
			len(repoModules), repoModules, len(readmeModules), readmeModules)
	}
}

func extractUseBlockEntries(t *testing.T, doc string) []string {
	t.Helper()
	re := regexp.MustCompile(`(?s)use \((.*?)\)`)
	m := re.FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf("no `use ( ... )` block found in:\n%s", doc)
	}
	var entries []string
	for _, line := range strings.Split(m[1], "\n") {
		if line = strings.TrimSpace(line); line != "" {
			entries = append(entries, line)
		}
	}
	return entries
}

// --- E-m5: README's Configuration table == server.LoadConfig's env vars --

func TestREADMEConfigurationTableMatchesLoadConfig(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "server/server.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	loadConfigEnvVars := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "LoadConfig" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Getenv" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "os" {
				return true
			}
			if len(call.Args) != 1 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			name, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			loadConfigEnvVars[name] = true
			return true
		})
		return false
	})
	if len(loadConfigEnvVars) == 0 {
		t.Fatal("found zero os.Getenv calls inside server.LoadConfig -- this test's AST walk is broken, not the code")
	}

	readme := mustReadFile(t, "README.md")
	section := extractMarkdownSection(readme, "## Configuration")
	rowRE := regexp.MustCompile("(?m)^\\| `([A-Z_]+)` \\|")
	readmeVars := map[string]bool{}
	for _, m := range rowRE.FindAllStringSubmatch(section, -1) {
		readmeVars[m[1]] = true
	}

	for v := range loadConfigEnvVars {
		if !readmeVars[v] {
			t.Errorf("server.LoadConfig reads os.Getenv(%q), but README.md's Configuration table does not document it", v)
		}
	}
	for v := range readmeVars {
		if !loadConfigEnvVars[v] {
			t.Errorf("README.md's Configuration table documents %q as read by server.LoadConfig, but LoadConfig does not call os.Getenv(%q) -- either it moved elsewhere (say so in prose, not this table) or it is stale", v, v)
		}
	}
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestREADMEMetricNamesExistInThePrometheusAdapter is P-2 (2026-09-03
// independent review).
//
// The two README gates above compare NUMBERS -- the method count, the
// endpoint counts. A rename, a one-in-one-out swap, or a metric whose
// meaning changed entirely all leave the number alone. The names README.md
// prints as the ones to build a dashboard on had no gate at all, and those
// are the part a reader copies.
//
// One direction only, deliberately: every ledger_* name in the README must
// be one the adapter registers. The reverse would demand the README list
// all forty-one, which is exactly what it says to read core/metrics.go for.
func TestREADMEMetricNamesExistInThePrometheusAdapter(t *testing.T) {
	registered := prometheusMetricNames(t)
	require.GreaterOrEqualf(t, len(registered), 30,
		"only %d metric name(s) were parsed out of observability/prometheus.go, which registers around forty -- "+
			"a scan that finds almost none reads as a pass", len(registered))

	readme := mustReadFile(t, "README.md")
	cited := regexp.MustCompile("`(ledger_[a-z0-9_]+)`").FindAllStringSubmatch(readme, -1)

	checked := 0
	for _, m := range cited {
		name := m[1]
		// The database roles share this prefix and are not metrics.
		if name == "ledger_app" || name == "ledger_ro" || name == "ledger_owner" {
			continue
		}
		checked++
		assert.Truef(t, registered[name],
			"README.md tells a reader to alert on %q, and observability/prometheus.go registers no such metric.\n\n"+
				"A dashboard built from the README would show an empty panel. The count gates next door compare "+
				"NUMBERS, which a rename or a one-in-one-out swap leaves untouched -- the names are the part a "+
				"reader actually copies (P-2). Registered: %v", name, sortedSetKeys(registered))
	}
	require.Positive(t, checked, "no ledger_* metric name was found in README.md -- the citation regexp stopped matching")
}

// prometheusMetricNames returns the fully qualified names the Prometheus
// adapter registers: its Namespace joined to each collector's Name.
func prometheusMetricNames(t *testing.T) map[string]bool {
	t.Helper()
	src := mustReadFile(t, "observability/prometheus.go")

	// The adapter writes `Namespace: ns` against a package constant, so
	// the namespace comes from that declaration rather than from the
	// literal at each call site.
	decl := regexp.MustCompile(`ns\s*=\s*"([a-z0-9_]+)"`).FindStringSubmatch(src)
	require.NotNil(t, decl, "observability/prometheus.go declares no metric namespace constant")
	namespace := decl[1]

	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`Name:\s*"([a-z0-9_]+)"`).FindAllStringSubmatch(src, -1) {
		out[namespace+"_"+m[1]] = true
	}
	return out
}

func sortedSetKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
