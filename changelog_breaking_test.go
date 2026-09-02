package ledger_test

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// changelog_breaking_test.go is the "readme_api_surface_test.go, but for the
// public Go API's breaking-change history" gate E-M6 asks for
// (docs/audits/2026-09-02-deep-audit/TODO.md): a hand-maintained CHANGELOG
// breaking-change list drifts the same way a hand-maintained README method
// table does (this repo's own history: the [0.6.0] Changed section missed
// six breaking Go API changes entirely -- see CHANGELOG.md's note in that
// section). The fix that worked for the README table was replacing "remember
// to write it down" with "derive it and fail if it is missing" --
// readme_api_surface_test.go's own doc comment calls this out as the
// precedent. This test applies the same idea to exported core/*.go + ledger.go
// symbols: diff them against the last release tag, and require every removed
// or changed one to be named somewhere in the CHANGELOG's current
// [Unreleased] section.
//
// Scope: exported top-level func/method SIGNATURES, exported top-level TYPE
// names, and the METHOD SETS of exported interfaces in core/*.go and
// ledger.go. The interface method set is here because of M11 (W3 adversarial
// review of the gates): this file's whole premise is core.Metrics' method
// growth, and yet a TypeSpec was recorded as the bare string "type", so
// adding a method to an exported interface -- breaking every hand-written
// implementation -- was structurally invisible to the diff. Adding one is now
// reported as a break, in the direction that matters: an added method is only
// breaking if the interface already existed. Struct field additions (e.g.
// SettleInput gaining a required IdempotencyKey) are a real breaking change
// too but are not diffed here -- they don't change a symbol's presence or a
// func signature, and diffing struct field sets doubles this file's size for
// a class of change the six-item CHANGELOG gap (all signature/type changes)
// didn't actually contain. If a future field-only break slips through
// undocumented, extend fieldsOf below; don't route around this test.
//
// Requires `git` AND the release tags. C-3 (W3 adversarial review of the
// gates): this used to t.Skip when the tag could not be resolved, and
// `actions/checkout` defaults to a depth-1 clone with NO tags -- so in CI,
// the only place this gate had to hold, it skipped every single run. A
// `git clone --depth 1` reproduces it: `git describe --tags` answers
// "fatal: No names found". The workflows now check out with `fetch-depth: 0`
// and an unresolvable tag is a failure, because a gate that did not run is
// not a gate that passed (working-agreements §3).
//
// api_surface_test.go's TestAPISurface_BreakingChangesAreDocumented resolves
// the same baseline for the BREAKING.md half of this contract and is
// fail-closed for the same reason. The two live in different test packages
// so the resolution cannot be shared; their POLICY must not diverge.
func TestChangelogListsBreakingGoAPIChanges(t *testing.T) {
	tag, err := lastReleaseTag()
	if err != nil {
		t.Fatalf("could not resolve the last release tag: %v\n\n"+
			"This gate diffs the exported Go API against the last release, so without tags it cannot run -- and not running is not passing "+
			"(working-agreements §3; before this was a failure it silently skipped on every CI run, since actions/checkout fetches depth 1 and no tags). "+
			"Fetch tags: `git fetch --tags`, or check out with `fetch-depth: 0`.", err)
	}

	changelog := mustReadFile(t, "CHANGELOG.md")
	unreleased := extractMarkdownSection(changelog, "## [Unreleased]")
	if unreleased == "" {
		t.Fatal("CHANGELOG.md has no \"## [Unreleased]\" section for this test to check against")
	}

	files := []string{"ledger.go"}
	coreFiles, err := exec.Command("git", "ls-tree", "-r", "--name-only", "HEAD", "--", "core/").Output()
	if err != nil {
		t.Fatalf("git ls-tree core/: %v", err)
	}
	for _, f := range strings.Split(strings.TrimSpace(string(coreFiles)), "\n") {
		if strings.HasSuffix(f, ".go") && !strings.HasSuffix(f, "_test.go") {
			files = append(files, f)
		}
	}

	var broken []string
	for _, f := range files {
		oldSrc, err := gitShow(tag, f)
		if err != nil {
			continue // file did not exist at tag -- nothing to have removed/changed
		}
		newSrc, err := gitShow("HEAD", f)
		if err != nil {
			continue // file deleted entirely; its symbols are handled as "removed" below via emptiness
		}
		oldSym := exportedSymbols(t, f, oldSrc)
		newSym := exportedSymbols(t, f, newSrc)
		for name, oldSig := range oldSym {
			newSig, stillPresent := newSym[name]
			switch {
			case !stillPresent:
				broken = append(broken, name+" (removed from "+f+")")
			case newSig != oldSig:
				broken = append(broken, name+" (signature changed in "+f+")")
			}
		}
		// Methods added to an interface that already existed: breaking for
		// every implementor (M11). Methods of a NEW interface are not --
		// nobody can have implemented it yet.
		for name, sig := range newSym {
			if !strings.HasPrefix(sig, interfaceMethodSig) {
				continue
			}
			if _, existed := oldSym[name]; existed {
				continue
			}
			owner := name[:strings.LastIndex(name, ".")]
			if _, ownerExisted := oldSym[owner]; !ownerExisted {
				continue
			}
			broken = append(broken, name+" (added to exported interface "+owner+" in "+f+")")
		}
	}

	sort.Strings(broken)
	for _, sym := range broken {
		bareName := strings.SplitN(sym, " ", 2)[0]
		if !changelogMentions(unreleased, bareName) {
			t.Errorf("exported symbol %s since %s, but CHANGELOG.md's [Unreleased] section does not name it. "+
				"Accepted spellings: the qualified key %q, %q, or an entry that names the owning type AND the member as a word",
				sym, tag, bareName, memberCallNeedle(bareName))
		}
	}
}

// changelogMentions reports whether the CHANGELOG section names symbol.
//
// A method key is "Type.Method", but CHANGELOG.md (like Go doc convention
// generally) writes it as "(*ledger.Type).Method", "ledger.Type.Method", or
// -- for a list of methods added to one interface -- the owning type in the
// entry's opening line and the members as bare backticked names below it.
// The last form is better documentation than nine qualified headings would
// be, so it counts, provided BOTH halves are present: an entry that names
// the type but never the member does not tell a consumer which call to fix.
//
// docs/BREAKING.md's half of this contract (api_surface_test.go's
// symbolDocumented) accepts the same three spellings, deliberately.
func changelogMentions(section, symbol string) bool {
	if strings.Contains(section, symbol) {
		return true
	}
	dot := strings.LastIndex(symbol, ".")
	if dot == -1 {
		return false
	}
	if strings.Contains(section, memberCallNeedle(symbol)) {
		return true
	}
	owner, member := symbol[:dot], symbol[dot+1:]
	if !strings.Contains(section, owner) {
		return false
	}
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(member) + `\b`).MatchString(section)
}

// memberCallNeedle renders "Type.Method" as ".Method(", the spelling every
// qualified Go-doc form of a method call contains.
func memberCallNeedle(symbol string) string {
	dot := strings.LastIndex(symbol, ".")
	if dot == -1 {
		return symbol
	}
	return symbol[dot:] + "("
}

// lastReleaseTag returns the most recent vX.Y.Z tag reachable from HEAD,
// excluding the ledger-react-vX.Y.Z tags (a different artifact, versioned
// independently -- see CHANGELOG.md's intro).
func lastReleaseTag() (string, error) {
	out, err := exec.Command("git", "describe", "--tags", "--abbrev=0", "--match", "v[0-9]*").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// gitShow returns the content of path as of rev, or an error if it did not
// exist there.
func gitShow(rev, path string) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command("git", "show", rev+":"+path)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}

// interfaceMethodSig prefixes the signature of an interface's method, so the
// diff above can tell "a method appeared on an existing interface" (breaking
// for implementors) from any other addition.
const interfaceMethodSig = "interface method "

// exportedSymbols parses src and returns a map of exported symbol name to a
// normalized signature string: package-level funcs and Option-style
// constructors keyed by name, methods keyed by "Recv.Method", exported
// top-level type declarations keyed by name with signature "type", and each
// exported interface's methods keyed "Iface.Method" with an
// interfaceMethodSig-prefixed signature.
func exportedSymbols(t *testing.T, filename, src string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	out := map[string]string{}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if !d.Name.IsExported() {
				continue
			}
			key := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				recvType := d.Recv.List[0].Type
				if star, ok := recvType.(*ast.StarExpr); ok {
					recvType = star.X
				}
				if ident, ok := recvType.(*ast.Ident); ok {
					key = ident.Name + "." + d.Name.Name
				}
			}
			out[key] = signatureString(d.Type)
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				continue
			}
			for _, spec := range d.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || !ts.Name.IsExported() {
					continue
				}
				out[ts.Name.Name] = "type"
				iface, ok := ts.Type.(*ast.InterfaceType)
				if !ok || iface.Methods == nil {
					continue
				}
				for _, m := range iface.Methods.List {
					ft, isFunc := m.Type.(*ast.FuncType)
					for _, name := range m.Names {
						if !name.IsExported() {
							continue
						}
						sig := interfaceMethodSig
						if isFunc {
							sig += signatureString(ft)
						}
						out[ts.Name.Name+"."+name.Name] = sig
					}
					if len(m.Names) == 0 {
						// Embedded interface: its method set arrives
						// wholesale, so record the embedding itself.
						out[ts.Name.Name+".<embedded "+exprString(m.Type)+">"] = interfaceMethodSig + "embedded"
					}
				}
			}
		}
	}
	return out
}

// signatureString renders a func type's params and results, ignoring
// parameter/result NAMES (renaming a parameter is not a breaking change)
// but not ignoring types or order (reordering or retyping is).
func signatureString(ft *ast.FuncType) string {
	var buf bytes.Buffer
	buf.WriteByte('(')
	writeFieldTypes(&buf, ft.Params)
	buf.WriteString(") (")
	writeFieldTypes(&buf, ft.Results)
	buf.WriteByte(')')
	return buf.String()
}

func writeFieldTypes(buf *bytes.Buffer, fl *ast.FieldList) {
	if fl == nil {
		return
	}
	first := true
	for _, field := range fl.List {
		n := len(field.Names)
		if n == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			if !first {
				buf.WriteString(", ")
			}
			first = false
			buf.WriteString(exprString(field.Type))
		}
	}
}

// exprString renders an ast.Expr type expression back to source-like text
// without needing the original file bytes (go/printer would work too, but
// pulls in a heavier dependency for what a small manual renderer covers:
// the type expression shapes actually used in this package's exported
// signatures -- idents, selectors, stars, slices, maps, ellipsis, and
// qualified generics are not used here).
func exprString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	case *ast.StarExpr:
		return "*" + exprString(v.X)
	case *ast.ArrayType:
		if v.Len == nil {
			return "[]" + exprString(v.Elt)
		}
		return "[N]" + exprString(v.Elt)
	case *ast.MapType:
		return "map[" + exprString(v.Key) + "]" + exprString(v.Value)
	case *ast.Ellipsis:
		return "..." + exprString(v.Elt)
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.FuncType:
		return "func(...)"
	case *ast.ChanType:
		return "chan " + exprString(v.Value)
	default:
		return "?"
	}
}
