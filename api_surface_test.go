package ledger

// H-M7: the Go exported API surface had no breaking-change detection of any
// kind. readme_api_surface_test.go only asks whether *Service's methods
// appear in a README table -- it never looks at core's ports and types,
// which ARE the library-mode contract, nor at package-level functions, nor
// at the seven other exported packages. The concrete failure shape the audit
// named: core.Metrics has grown to 32 methods, and every addition silently
// breaks any consumer implementation that does not embed core.NoopMetrics.
// Go has no sealed interfaces, so nothing but a gate can notice.
//
// The gate is a committed snapshot of the surface (docs/api-surface.txt)
// plus two checks:
//
//   - TestAPISurface_MatchesSnapshot: the surface derived from the sources
//     must equal the snapshot. Any change -- addition, removal, signature
//     edit -- is red until the author regenerates it, which makes "I am
//     changing the public API" an explicit act rather than a side effect.
//   - TestAPISurface_BreakingChangesAreDocumented: comparing the working
//     tree's snapshot against the one at the BASELINE (the last release tag,
//     or the merge-base with main), any REMOVED or CHANGED symbol -- and any
//     ADDED interface method, which breaks every consumer implementation
//     that does not embed a Noop base -- must be named in docs/BREAKING.md.
//     Other additions are free.
//
// C-3 (W3 adversarial review of the gates): the baseline used to be HEAD,
// which made this gate structurally incapable of failing in CI. The author
// regenerates the snapshot and commits; from that commit on the working
// tree and HEAD agree, the delta is empty, and the post-commit state is the
// only state CI ever sees. The reviewer changed an exported signature,
// regenerated, committed without a BREAKING.md entry, and the gate passed.
// Diffing against a ref that does NOT move with the commit -- the last
// release tag, the same baseline changelog_breaking_test.go uses -- is what
// makes the answer the same locally and in CI.
//
// It follows that CI must have that ref: `actions/checkout` defaults to a
// depth-1 clone with no tags, so .github/workflows/* set `fetch-depth: 0`.
// If the baseline cannot be resolved this gate FAILS rather than skips: a
// gate that did not run is not a gate that passed (working-agreements §3).
//
// deployment.md's terms: a Go library's exported surface is an API contract,
// so a break goes through expand -> migrate -> contract and lands in
// BREAKING.md, not in a release note someone writes afterwards.

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const (
	apiSurfacePath = "docs/api-surface.txt"
	breakingPath   = "docs/BREAKING.md"
)

// surfaceRoots are the module-root directories whose exported surface is a
// consumer contract. Discovered by walking, not listed, so a new exported
// package defaults to covered.
//
// Excluded, each for a structural reason: internal/ (not importable by a
// consumer), cmd/ (a binary, no importable surface), examples/ (illustrative
// programs), web/ (TypeScript), docs/, deploy/, and chains/ + anchors/
// (separate Go modules -- their own go.mod means their surface is versioned
// separately; see docs/BREAKING.md's note on H-M8).
var surfaceSkipDirs = map[string]bool{
	"internal": true,
	"cmd":      true,
	"examples": true,
	"web":      true,
	"docs":     true,
	"deploy":   true,
	"chains":   true,
	"anchors":  true,
	"testdata": true,
	".git":     true,
	".github":  true,
}

// generatedPackages stay IN the snapshot (so a regeneration is still an
// explicit, reviewable diff) but are exempt from the BREAKING.md
// requirement: postgres/sqlcgen is generated from the .sql files ("do not
// edit" -- CLAUDE.md) and its query parameter/row structs are storage
// plumbing, not a consumption contract. Requiring a breaking-change entry
// for each one would make every `sqlc generate` write paperwork nobody
// consumes, which is how a gate teaches people to route around it.
//
// Deliberately an exemption from the DOC requirement rather than a scope
// exclusion: dropping a package out of the snapshot entirely would also
// make deleting that package invisible.
var generatedPackages = map[string]bool{"sqlcgen": true}

func isGeneratedSymbol(sym string) bool {
	pkg, _, _ := strings.Cut(sym, ".")
	return generatedPackages[pkg]
}

// TestAPISurface_MatchesSnapshot is the "changing the public API is an
// explicit act" half.
func TestAPISurface_MatchesSnapshot(t *testing.T) {
	current := renderAPISurface(t)

	want, err := os.ReadFile(apiSurfacePath)
	if err != nil {
		t.Fatalf("read %s: %v\n\nthis snapshot is the record of the library's exported surface; regenerate it with:\n  go run ./internal/... # (no generator: write the rendered surface below)\n\n%s", apiSurfacePath, err, current)
	}

	if string(want) == current {
		return
	}
	if os.Getenv("UPDATE_API_SURFACE") == "1" {
		if err := os.WriteFile(apiSurfacePath, []byte(current), 0o644); err != nil {
			t.Fatalf("write %s: %v", apiSurfacePath, err)
		}
		t.Fatalf("%s regenerated (UPDATE_API_SURFACE=1). Re-run without it, and if the diff REMOVES or CHANGES a symbol, add a %s entry in the same commit.", apiSurfacePath, breakingPath)
	}

	t.Fatalf("the exported Go API surface no longer matches %s.\n\n%s\n\nIf the change is intended:\n"+
		"  1. UPDATE_API_SURFACE=1 go test . -run TestAPISurface_MatchesSnapshot\n"+
		"  2. if the diff removes or changes a symbol (including adding a method to an\n"+
		"     exported interface, which breaks every consumer implementation that does\n"+
		"     not embed a Noop base), add an entry to %s naming the symbol and what a\n"+
		"     consumer must do -- TestAPISurface_BreakingChangesAreDocumented enforces it.",
		apiSurfacePath, diffSurfaces(string(want), current), breakingPath)
}

// TestAPISurface_BreakingChangesAreDocumented compares the working tree's
// snapshot against the one at the release baseline and requires every
// removal, signature change, or added interface method to be named in
// docs/BREAKING.md.
func TestAPISurface_BreakingChangesAreDocumented(t *testing.T) {
	baseline := breakingBaselineRef(t)
	previous := surfaceAtRef(t, baseline)
	current := renderAPISurface(t)

	removed, changed := breakingDelta(previous, current)
	addedMethods := addedInterfaceMethods(previous, current)
	changed = append(changed, addedMethods...)
	sort.Strings(changed)
	if len(removed) == 0 && len(changed) == 0 {
		return
	}

	breaking, err := os.ReadFile(breakingPath)
	if err != nil {
		t.Fatalf("read %s: %v -- this commit changes the exported surface, so it needs a breaking-change record", breakingPath, err)
	}
	doc := string(breaking)

	var undocumented []string
	for _, sym := range append(removed, changed...) {
		if isGeneratedSymbol(sym) {
			continue
		}
		if !symbolDocumented(doc, sym) {
			undocumented = append(undocumented, sym)
		}
	}
	sort.Strings(undocumented)
	if len(undocumented) > 0 {
		t.Fatalf("symbol(s) removed, changed, or added to an exported interface relative to %s without a %s entry naming them: %v\n\n"+
			"A Go library's exported surface is an API contract (deployment.md): a consumer's build breaks, "+
			"and the only place they can find out why is this file. Add an entry under [Unreleased] with the "+
			"symbol name and what the consumer must do.", baseline, breakingPath, undocumented)
	}
}

// breakingBaselineRef resolves the ref this gate diffs against: the last
// release tag, falling back to the merge-base with main for work that
// predates the next tag. Never HEAD -- see the C-3 note in this file's
// header. Unresolvable is fatal, never a skip.
//
// changelog_breaking_test.go resolves the same "last release tag" for the
// CHANGELOG half of this contract and is fail-closed for the same reason;
// the two live in different test packages (ledger vs ledger_test) so the
// resolution cannot be shared, but their POLICY must not diverge: neither
// may go back to skipping.
func breakingBaselineRef(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git not found: this gate diffs the exported surface against the last release and cannot answer without it (%v)", err)
	}
	if inRepo := exec.Command("git", "rev-parse", "--git-dir").Run(); inRepo != nil {
		t.Fatalf("not a git repository (or git failed): this gate diffs the exported surface against the last release and cannot answer without it (%v)", inRepo)
	}
	if out, err := exec.Command("git", "describe", "--tags", "--abbrev=0", "--match", "v[0-9]*").Output(); err == nil {
		if tag := strings.TrimSpace(string(out)); tag != "" {
			return tag
		}
	}
	for _, ref := range []string{"origin/main", "main"} {
		if out, err := exec.Command("git", "merge-base", "HEAD", ref).Output(); err == nil {
			if base := strings.TrimSpace(string(out)); base != "" {
				return base
			}
		}
	}
	t.Fatalf("cannot resolve a baseline to diff the exported surface against: no v[0-9]* tag reachable from HEAD and no merge-base with origin/main or main.\n" +
		"A CI checkout defaults to a depth-1 clone with no tags, which is exactly the state this gate must not silently pass in " +
		"(working-agreements §3: not run is not passed). Check out with `fetch-depth: 0`.")
	return ""
}

// surfaceAtRef renders the exported surface of the sources AT ref, by
// exporting that tree to a temporary directory and running the same renderer
// over it.
//
// Deliberately not `git show ref:docs/api-surface.txt`: that reads the
// snapshot FILE, which only exists at refs newer than this gate (it does not
// exist at v0.6.0, the current baseline), and a missing file would leave the
// gate with nothing to compare -- silently passing, which is the C-3 shape
// again one level down. Rendering the sources works at any ref and does not
// depend on a previous author having regenerated anything.
func surfaceAtRef(t *testing.T, ref string) string {
	t.Helper()
	dir := t.TempDir()
	archive := exec.Command("git", "archive", "--format=tar", ref)
	tarball, err := archive.Output()
	if err != nil {
		t.Fatalf("git archive %s: %v -- this gate renders the exported surface at the baseline and cannot answer without that tree", ref, err)
	}
	extract := exec.Command("tar", "-x", "-C", dir)
	extract.Stdin = bytes.NewReader(tarball)
	if err := extract.Run(); err != nil {
		t.Fatalf("extract the %s tree: %v", ref, err)
	}
	return renderAPISurfaceIn(t, dir)
}

// symbolDocumented reports whether docs/BREAKING.md names sym.
//
// An exact substring is the easy case. The other accepted spelling is
// "owner names the member": the entry headed "core.Metrics grows from 32 to
// 41 methods" lists the nine new methods by bare name under a heading that
// names the interface, which is better documentation than nine
// fully-qualified headings would be. So an owner mention plus the member
// name as a whole word counts. Both halves are required -- a doc that
// mentions the type but never the member does not tell a consumer which
// call to fix.
func symbolDocumented(doc, sym string) bool {
	if strings.Contains(doc, sym) {
		return true
	}
	idx := strings.LastIndex(sym, ".")
	if idx <= 0 {
		return false
	}
	owner, member := sym[:idx], sym[idx+1:]
	// Only members of a named type (pkg.Type.Member) get the relaxation. A
	// package-level symbol's owner is the package name, which is a word
	// common enough ("server", "core") that owner+member would match almost
	// any prose -- pkg.Symbol must be named exactly.
	if !strings.Contains(owner, ".") {
		return false
	}
	if !strings.Contains(doc, owner) {
		return false
	}
	word, err := regexp.Compile(`\b` + regexp.QuoteMeta(member) + `\b`)
	if err != nil {
		return false
	}
	return word.MatchString(doc)
}

// addedInterfaceMethods returns methods added to an interface that already
// existed at the baseline. Go has no sealed interfaces, so this breaks every
// consumer implementation that does not embed a Noop base -- the exact
// failure this whole file was built for (core.Metrics' method growth), and
// the one an "additions are free" rule would wave through.
//
// Methods on an interface that is itself new are NOT breaking: nobody can
// have implemented it yet.
func addedInterfaceMethods(previous, current string) []string {
	prev, cur := surfaceIndex(previous), surfaceIndex(current)
	var out []string
	for sym, decl := range cur {
		if !strings.HasPrefix(decl, "interface method ") && !strings.HasPrefix(decl, "embedded interface") {
			continue
		}
		if _, existed := prev[sym]; existed {
			continue
		}
		owner, _, found := strings.Cut(sym, ".<embedded ")
		if !found {
			if idx := strings.LastIndex(sym, "."); idx != -1 {
				owner = sym[:idx]
			}
		}
		if _, ownerExisted := prev[owner]; !ownerExisted {
			continue // brand-new interface: no implementations to break
		}
		out = append(out, sym)
	}
	sort.Strings(out)
	return out
}

// breakingDelta returns the symbols present in previous but absent from
// current (removed), and those whose rendered line changed (changed).
// Symbol identity is the text before the first " = " separator; the rest is
// the signature/type.
func breakingDelta(previous, current string) (removed, changed []string) {
	prev := surfaceIndex(previous)
	cur := surfaceIndex(current)
	for sym, decl := range prev {
		newDecl, ok := cur[sym]
		switch {
		case !ok:
			removed = append(removed, sym)
		case newDecl != decl:
			changed = append(changed, sym)
		}
	}
	sort.Strings(removed)
	sort.Strings(changed)
	return removed, changed
}

func surfaceIndex(snapshot string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(snapshot, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sym, decl, found := strings.Cut(line, " = ")
		if !found {
			continue
		}
		out[sym] = decl
	}
	return out
}

func diffSurfaces(want, got string) string {
	removed, changed := breakingDelta(want, got)
	added := []string{}
	wantIdx, gotIdx := surfaceIndex(want), surfaceIndex(got)
	for sym := range gotIdx {
		if _, ok := wantIdx[sym]; !ok {
			added = append(added, sym)
		}
	}
	sort.Strings(added)
	var b strings.Builder
	writeList := func(label string, list []string) {
		if len(list) == 0 {
			return
		}
		fmt.Fprintf(&b, "  %s (%d):\n", label, len(list))
		for _, s := range list {
			fmt.Fprintf(&b, "    %s\n", s)
		}
	}
	writeList("removed", removed)
	writeList("changed", changed)
	writeList("added", added)
	return b.String()
}

// --- surface rendering ---

// renderAPISurface walks every consumer-importable package in the module
// root and renders one stable line per exported symbol.
func renderAPISurface(t *testing.T) string {
	t.Helper()
	return renderAPISurfaceIn(t, ".")
}

// renderAPISurfaceIn renders the surface of the module rooted at root, so the
// same renderer can read the working tree and a baseline tree exported to a
// temporary directory.
func renderAPISurfaceIn(t *testing.T, root string) string {
	t.Helper()
	dirs := surfaceDirs(t, root)

	var lines []string
	for _, dir := range dirs {
		fset := token.NewFileSet()
		matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatalf("glob %s: %v", dir, err)
		}
		sort.Strings(matches)
		pkgName := ""
		for _, path := range matches {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			if pkgName == "" {
				pkgName = file.Name.Name
			}
			lines = append(lines, renderFile(fset, pkgName, file)...)
		}
	}
	sort.Strings(lines)

	var b strings.Builder
	b.WriteString("# Exported Go API surface of github.com/azex-ai/ledger (module root packages).\n")
	b.WriteString("# One line per symbol: <package>.<symbol> = <declaration>.\n")
	b.WriteString("#\n")
	b.WriteString("# This file is a gate, not documentation (H-M7). Regenerate with:\n")
	b.WriteString("#   UPDATE_API_SURFACE=1 go test . -run TestAPISurface_MatchesSnapshot\n")
	b.WriteString("# and if the diff REMOVES or CHANGES a symbol -- including adding a method to\n")
	b.WriteString("# an exported interface, which breaks every consumer implementation that does\n")
	b.WriteString("# not embed a Noop base -- add a docs/BREAKING.md entry in the same commit.\n")
	b.WriteString("#\n")
	b.WriteString("# chains/evm and anchors/r2 are separate Go modules and are versioned\n")
	b.WriteString("# separately; they are deliberately out of scope here.\n")
	for _, line := range lines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

func surfaceDirs(t *testing.T, root string) []string {
	t.Helper()
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if path != root && (surfaceSkipDirs[base] || strings.HasPrefix(base, ".")) {
			return filepath.SkipDir
		}
		matches, globErr := filepath.Glob(filepath.Join(path, "*.go"))
		if globErr != nil {
			return globErr
		}
		for _, m := range matches {
			if !strings.HasSuffix(m, "_test.go") {
				dirs = append(dirs, path)
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module root %s: %v", root, err)
	}
	sort.Strings(dirs)
	if len(dirs) < 5 {
		t.Fatalf("only found %d packages to snapshot (%v) -- the walk regressed; a gate that scans almost nothing reads as a pass", len(dirs), dirs)
	}
	return dirs
}

func renderFile(fset *token.FileSet, pkg string, file *ast.File) []string {
	var lines []string
	emit := func(sym, decl string) {
		lines = append(lines, fmt.Sprintf("%s.%s = %s", pkg, sym, decl))
	}

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if !d.Name.IsExported() {
				continue
			}
			sig := renderNode(fset, d.Type)
			if d.Recv == nil {
				emit(d.Name.Name, "func"+strings.TrimPrefix(sig, "func"))
				continue
			}
			if len(d.Recv.List) == 0 {
				continue
			}
			recv := renderNode(fset, d.Recv.List[0].Type)
			recvName := strings.TrimPrefix(recv, "*")
			if !ast.IsExported(strings.TrimSuffix(strings.Split(recvName, "[")[0], "")) {
				continue
			}
			emit(recvName+"."+d.Name.Name, "method "+strings.TrimPrefix(sig, "func"))

		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if !s.Name.IsExported() {
						continue
					}
					lines = append(lines, renderTypeSpec(fset, pkg, s)...)
				case *ast.ValueSpec:
					kind := "var"
					if d.Tok == token.CONST {
						kind = "const"
					}
					for i, name := range s.Names {
						if !name.IsExported() {
							continue
						}
						typeText := ""
						if s.Type != nil {
							typeText = " " + renderNode(fset, s.Type)
						} else if i < len(s.Values) {
							// Untyped: record the value's shape only for
							// literals, which are part of the contract
							// (e.g. a version string constant).
							if lit, ok := s.Values[i].(*ast.BasicLit); ok {
								typeText = " = " + lit.Value
							}
						}
						emit(name.Name, kind+typeText)
					}
				}
			}
		}
	}
	return lines
}

func renderTypeSpec(fset *token.FileSet, pkg string, s *ast.TypeSpec) []string {
	var lines []string
	emit := func(sym, decl string) {
		lines = append(lines, fmt.Sprintf("%s.%s = %s", pkg, sym, decl))
	}

	switch t := s.Type.(type) {
	case *ast.StructType:
		emit(s.Name.Name, "type struct")
		for _, field := range t.Fields.List {
			for _, name := range field.Names {
				if !name.IsExported() {
					continue
				}
				emit(s.Name.Name+"."+name.Name, "field "+renderNode(fset, field.Type))
			}
			if len(field.Names) == 0 {
				emit(s.Name.Name+".<embedded "+renderNode(fset, field.Type)+">", "embedded")
			}
		}
	case *ast.InterfaceType:
		emit(s.Name.Name, "type interface")
		for _, method := range t.Methods.List {
			if len(method.Names) == 0 {
				emit(s.Name.Name+".<embedded "+renderNode(fset, method.Type)+">", "embedded interface")
				continue
			}
			for _, name := range method.Names {
				if !name.IsExported() {
					continue
				}
				emit(s.Name.Name+"."+name.Name, "interface method "+strings.TrimPrefix(renderNode(fset, method.Type), "func"))
			}
		}
	default:
		emit(s.Name.Name, "type "+renderNode(fset, s.Type))
	}
	return lines
}

// renderNode prints an AST node with normalized whitespace so the snapshot
// is stable against reformatting and comments.
func renderNode(fset *token.FileSet, node ast.Node) string {
	var buf bytes.Buffer
	cfg := printer.Config{Mode: printer.RawFormat}
	if err := cfg.Fprint(&buf, fset, node); err != nil {
		return "<unprintable>"
	}
	return strings.Join(strings.Fields(buf.String()), " ")
}
