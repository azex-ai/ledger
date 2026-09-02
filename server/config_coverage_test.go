// Package server: config_coverage_test.go
//
// H-m5: Config.AllowSystemClassificationPost's doc comment named the
// environment variable ALLOW_SYSTEM_CLASSIFICATION_POST, and LoadConfig
// never read it. A deployment that set it got the default and believed it
// had opted out of a guard. The direction happened to be the safe one, so
// nothing broke loudly -- an operator's mental model of which gates are on
// was simply wrong.
//
// Two mechanical checks, both derived from the package's own AST rather than
// from a list of fields to remember:
//
//  1. every UPPER_SNAKE token a Config field's doc comment mentions must
//     actually be read by an os.Getenv in this package;
//  2. every exported Config field must be assigned in LoadConfig's Config
//     literal, or carry the explicit marker below saying it is not
//     environment-driven.
package server

import (
	"go/ast"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// notFromEnvMarker is how a Config field declares "LoadConfig deliberately
// does not read me; I am set programmatically by the composition root".
// Spelled out in the field's doc comment, so the reader of the field learns
// it at the same place the gate does.
const notFromEnvMarker = "Not read by LoadConfig"

var envTokenRe = regexp.MustCompile(`\b[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+\b`)

// configFieldDocs returns each exported Config field's name and doc/line
// comment text.
func configFieldDocs(t *testing.T, files []*ast.File) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name.Name != "Config" {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				require.True(t, ok, "Config is not a struct type")
				for _, field := range st.Fields.List {
					var text strings.Builder
					if field.Doc != nil {
						text.WriteString(field.Doc.Text())
					}
					if field.Comment != nil {
						text.WriteString(field.Comment.Text())
					}
					for _, name := range field.Names {
						if name.IsExported() {
							out[name.Name] = text.String()
						}
					}
				}
			}
		}
	}
	require.NotEmpty(t, out, "no exported Config fields found -- the AST walk is broken, not the config")

	// Fail closed on a comment-less parse: without parser.ParseComments
	// every doc string is empty and both checks below pass vacuously (which
	// is exactly what happened the first time this file was written).
	var documented int
	for _, doc := range out {
		if strings.TrimSpace(doc) != "" {
			documented++
		}
	}
	require.NotZero(t, documented, "every Config field's doc comment came back empty -- the AST was parsed without parser.ParseComments, so these checks would pass vacuously")
	return out
}

// getenvNames returns every os.Getenv("NAME") literal in the package.
func getenvNames(t *testing.T, files []*ast.File) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Getenv" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "os" {
				return true
			}
			if lit, ok := stringLit(call.Args[0]); ok {
				out[lit] = true
			}
			return true
		})
	}
	return out
}

// loadConfigAssignedFields returns the Config field names LoadConfig's
// composite literal assigns.
func loadConfigAssignedFields(t *testing.T, files []*ast.File) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	var found bool
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "LoadConfig" || fn.Recv != nil {
				continue
			}
			found = true
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				ident, ok := lit.Type.(*ast.Ident)
				if !ok || ident.Name != "Config" {
					return true
				}
				for _, elt := range lit.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if key, ok := kv.Key.(*ast.Ident); ok {
						out[key.Name] = true
					}
				}
				return true
			})
		}
	}
	require.True(t, found, "LoadConfig not found in the server package")
	return out
}

// TestConfig_DocumentedEnvVarsAreRead is H-m5's gate: a Config field whose
// documentation names an environment variable must have that variable read
// somewhere in the package.
func TestConfig_DocumentedEnvVarsAreRead(t *testing.T) {
	_, files := parsePackageFiles(t)
	docs := configFieldDocs(t, files)
	read := getenvNames(t, files)

	var missing []string
	for field, doc := range docs {
		for _, m := range envTokenRe.FindAllString(doc, -1) {
			if !read[m] {
				missing = append(missing, field+" documents "+m)
			}
		}
	}
	sort.Strings(missing)
	require.Empty(t, missing,
		"Config field(s) document an environment variable that no os.Getenv in this package reads -- "+
			"an operator who sets it gets the default and believes otherwise; either read it in LoadConfig or stop naming it in the doc comment")
}

// TestConfig_EveryFieldIsLoadedOrMarked is the other direction: a new field
// that LoadConfig forgets defaults to the zero value in every
// LoadConfig-based deployment. Silence is not an acceptable answer for a
// field that gates behavior, so a field is either loaded from the
// environment or explicitly marked as programmatic-only.
func TestConfig_EveryFieldIsLoadedOrMarked(t *testing.T) {
	_, files := parsePackageFiles(t)
	docs := configFieldDocs(t, files)
	assigned := loadConfigAssignedFields(t, files)

	var unloaded []string
	for field, doc := range docs {
		if assigned[field] || strings.Contains(doc, notFromEnvMarker) {
			continue
		}
		unloaded = append(unloaded, field)
	}
	sort.Strings(unloaded)
	require.Empty(t, unloaded,
		"Config field(s) are neither assigned in LoadConfig nor marked %q in their doc comment -- "+
			"a field that LoadConfig skips is silently zero in every deployment that uses it", notFromEnvMarker)
}

// TestLoadConfig_ReadsAllowSystemClassificationPost is H-m5's behavioral
// pin, next to the structural gates above: the two would both stay green if
// LoadConfig read the variable into a local and forgot to put it in the
// Config literal, so pin the value that actually comes back.
func TestLoadConfig_ReadsAllowSystemClassificationPost(t *testing.T) {
	t.Setenv("ENV", "dev")
	t.Setenv("CORS_ALLOWED_ORIGIN", "*")

	t.Setenv("ALLOW_SYSTEM_CLASSIFICATION_POST", "true")
	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.True(t, cfg.AllowSystemClassificationPost,
		"ALLOW_SYSTEM_CLASSIFICATION_POST=true must reach Config.AllowSystemClassificationPost")

	// Anything other than the exact string "true" leaves the guard on: the
	// default for a guard is closed.
	t.Setenv("ALLOW_SYSTEM_CLASSIFICATION_POST", "yes")
	cfg, err = LoadConfig()
	require.NoError(t, err)
	require.False(t, cfg.AllowSystemClassificationPost)

	t.Setenv("ALLOW_SYSTEM_CLASSIFICATION_POST", "")
	cfg, err = LoadConfig()
	require.NoError(t, err)
	require.False(t, cfg.AllowSystemClassificationPost)
}
