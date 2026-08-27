package ledger

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestREADMEDocumentsEveryExportedServiceMethod keeps README's "API Surface"
// from drifting away from ledger.go, which is how it got 11 methods behind in
// the first place -- including the entire integrity subsystem, while
// CheckpointIntegrity's own godoc said the withdrawal path MUST use it.
//
// A reader who cannot find a method in the table concludes it does not exist.
// That is not a documentation nicety when the missing entry is the one the
// signing path depends on: Authorize and AuthorizeTemplate are the only way
// to get a signed journal out of RunInTx, and a consumer who never learns
// they exist writes an unsigned money path and is told nothing.
//
// Adding an exported method to Service therefore fails this test until the
// table mentions it. If a method is genuinely internal, unexport it -- that
// is the honest fix, not an exception list here.
//
// m-6 (2026-08-26 independent review, third pass): the previous version
// called parser.ParseFile(fset, "ledger.go", ...) and only recognized a
// pointer receiver (*ast.StarExpr around an *ast.Ident named "Service").
// Both were silent escape hatches -- an exported method added to ANY other
// file in this package (idempotency.go, or a new file), or written with a
// value receiver (func (s Service) Foo(...)), would never be seen by this
// loop and so could never fail it, regardless of whether README documented
// it (working-agreements §3: "未运行 ≠ 通过" -- a check that cannot see a
// whole class of the thing it exists to police is functionally the same as
// no check for that class). This version parses every non-test .go file in
// the root directory (root has both `package ledger` and the external
// `package ledger_test`; only the former's declared methods count as real
// API surface -- the package-name check below filters that) and accepts
// either receiver form.
//
// parser.ParseDir would do this in one call, but it is deprecated as of Go
// 1.25 (does not consider build tags when associating files with packages;
// golang.org/x/tools/go/packages is the replacement, and pkg-first.md
// disfavors adding that dependency -- full go/packages loading -- for what
// this file only ever needed: a flat, non-recursive directory listing).
// os.ReadDir + parser.ParseFile per file is the un-deprecated equivalent for
// that narrower need.
func TestREADMEDocumentsEveryExportedServiceMethod(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(readme)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	var undocumented []string
	sawPackage := false
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if f.Name.Name != "ledger" {
			continue // e.g. a future non-test file in package ledger_test
		}
		sawPackage = true

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || !fn.Name.IsExported() {
				continue
			}
			recvType := fn.Recv.List[0].Type
			if star, ok := recvType.(*ast.StarExpr); ok {
				recvType = star.X
			}
			ident, ok := recvType.(*ast.Ident)
			if !ok || ident.Name != "Service" {
				continue
			}
			if !strings.Contains(doc, "`svc."+fn.Name.Name+"(") {
				undocumented = append(undocumented, fn.Name.Name)
			}
		}
	}
	if !sawPackage {
		t.Fatal(`no "package ledger" .go file found in the root directory -- ` +
			filepath.Join(".", "*.go") + " listing may be broken")
	}

	sort.Strings(undocumented)
	if len(undocumented) > 0 {
		t.Errorf("exported *Service methods missing from README's API Surface: %v\n"+
			"Add a row for each, or unexport it if it is not part of the library's surface.",
			undocumented)
	}
}
