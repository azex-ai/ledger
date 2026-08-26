package ledger

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
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
func TestREADMEDocumentsEveryExportedServiceMethod(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(readme)

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "ledger.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	var undocumented []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || !fn.Name.IsExported() {
			continue
		}
		star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		ident, ok := star.X.(*ast.Ident)
		if !ok || ident.Name != "Service" {
			continue
		}
		if !strings.Contains(doc, "`svc."+fn.Name.Name+"(") {
			undocumented = append(undocumented, fn.Name.Name)
		}
	}

	if len(undocumented) > 0 {
		t.Errorf("exported *Service methods missing from README's API Surface: %v\n"+
			"Add a row for each, or unexport it if it is not part of the library's surface.",
			undocumented)
	}
}
