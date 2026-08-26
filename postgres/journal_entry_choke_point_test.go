package postgres

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInsertJournalEntry_SingleChokePoint is a load-bearing gate, not a
// property test for its own sake.
//
// docs/INVARIANTS.md I-5's "Load-bearing prerequisite" says, in prose only:
// every journal_entries write must go through acquireBalanceLocks before
// InsertJournalEntry, and warns "any future write path that inserts entries
// without acquireBalanceLocks silently reopens this visibility race -- do
// not add one." Nothing checked that promise -- see
// docs/audits/2026-08-25-financial-engineering/financial-correctness.md
// ("I-5 的载荷前提只有散文守卫，没有机器门禁").
//
// This test makes it a fact instead of a promise: it parses every
// non-generated, non-test .go file in this package, finds every call to
// (*sqlcgen.Queries).InsertJournalEntry, and asserts (a) there is exactly
// one call site and (b) the function containing it also calls
// acquireBalanceLocks. A second entry-insert path -- with or without the
// lock -- fails this test immediately instead of silently reopening the I-5
// race the day someone adds one.
func TestInsertJournalEntry_SingleChokePoint(t *testing.T) {
	// parser.ParseDir is deprecated (doesn't consider build tags); this
	// package has none, and pulling in golang.org/x/tools/go/packages for a
	// single test-only AST scan isn't warranted (pkg-first: use what's
	// already a dependency), so parse each non-test .go file directly.
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		require.NoError(t, err, "parse %s", name)
		files = append(files, file)
	}
	require.NotEmpty(t, files)

	type callSite struct {
		funcName          string
		callsAcquireLocks bool
	}
	var sites []callSite

	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			var hasInsert, hasLock bool
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fnExpr := call.Fun.(type) {
				case *ast.SelectorExpr:
					if fnExpr.Sel.Name == "InsertJournalEntry" {
						hasInsert = true
					}
				case *ast.Ident:
					if fnExpr.Name == "acquireBalanceLocks" {
						hasLock = true
					}
				}
				return true
			})
			if hasInsert {
				sites = append(sites, callSite{funcName: fn.Name.Name, callsAcquireLocks: hasLock})
			}
		}
	}

	names := make([]string, len(sites))
	for i, s := range sites {
		names[i] = s.funcName
	}
	require.Len(t, sites, 1, "InsertJournalEntry must have exactly one call site in this package; found in: %v", names)
	assert.True(t, sites[0].callsAcquireLocks,
		"the single InsertJournalEntry call site (%s) must also call acquireBalanceLocks in the same function -- I-5's load-bearing prerequisite (docs/INVARIANTS.md)",
		sites[0].funcName,
	)
}
