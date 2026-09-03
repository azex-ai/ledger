package core

// F-7 (2026-09-03 independent review): I-18 says "core types and interfaces
// speak uids exclusively", without qualification. The gate said something
// narrower -- "no field whose name is one of the column names the schema
// uses for its BIGSERIAL keys". Adding
//
//	EventRef int64 `json:"event_ref"`
//
// to core.JournalInput left core, server and service entirely green. The
// only test that noticed was TestAPISurface_MatchesSnapshot, and its
// remedy is "regenerate the snapshot", where `EventRef int64` reads as
// harmless.
//
// A name-derived rule can only ever catch the names somebody already used.
// The property I-18 actually wants is about the TYPE: an int64 crossing
// this boundary is either an identifier from a namespace the caller owns
// (their account holder, their chain id, their actor) or a quantity -- and
// never a row id from this library's storage, whatever it is called.
//
// So every int64 field on an exported core type must say which it is. That
// is the same shape as idschema.AllowedInternalIDTypes, one level down:
// name the exception and the reason, and let anything unregistered fail.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// externalNamespaceInt64Fields are field NAMES whose int64 belongs to a
// namespace outside this library. They are keyed by name rather than by
// type.field because each means the same thing everywhere it appears, and
// listing sixty type-qualified copies of "the caller's own holder id" would
// be a list nobody rereads.
//
// idschema's own doc comment already draws this line for the wire keys:
// account_holder / actor_id / chain_id and friends are the caller's
// namespace, not a storage detail.
var externalNamespaceInt64Fields = map[string]string{
	"AccountHolder":    "the consumer's own holder id -- the ledger stores it, never mints it (see core/system_account.go for the sign convention)",
	"ActorID":          "the consumer's own actor/operator id, recorded on audit rows",
	"HolderID":         "same as AccountHolder, spelled the way TemplateParams' callers spell it",
	"ChainID":          "EIP-155 chain id: a public, external namespace",
	"BlockNumber":      "a block height on a public chain",
	"NextBlock":        "a block height on a public chain",
	"ScanStartBlock":   "a block height on a public chain",
	"LastScannedBlock": "a block height on a public chain",
	"NewAfterHolder":   "an account holder, as a scan cursor position",
	"OldAfterHolder":   "an account holder, as a scan cursor position",
}

// quantityInt64Fields are field names whose int64 is a COUNT or an INDEX --
// a number, not an identifier of anything.
var quantityInt64Fields = map[string]string{
	"EntryCount":         "how many entries an attestation batch covers",
	"Seq":                "the attestation chain's own sequence number, which is the chain's identity and is meant to be public",
	"LeafIndex":          "a position within a Merkle tree",
	"TreeSize":           "how many leaves a Merkle tree has",
	"ActiveReservations": "a count, for health reporting",
	"RollupQueueDepth":   "a count, for health reporting",
}

// internalIDInt64Fields are the exceptions: fields that really do carry a
// row id from this library's storage, keyed by Type.Field so the exemption
// cannot spread to a same-named field on another type.
//
// The two attestation types are the ones idschema.AllowedInternalIDTypes
// already registers, for the reason stated there: a digest has to bind the
// exact stored rows it signs, and an attestation over uids would not detect
// a swapped row id. The currency/classification ids on AttestedEntry are
// part of the same signed tuple.
var internalIDInt64Fields = map[string]string{
	"AttestedEntry.EntryID":          "digest input; see idschema.AllowedInternalIDTypes",
	"AttestedEntry.JournalID":        "digest input; see idschema.AllowedInternalIDTypes",
	"AttestedEntry.CurrencyID":       "digest input; see idschema.AllowedInternalIDTypes",
	"AttestedEntry.ClassificationID": "digest input; see idschema.AllowedInternalIDTypes",
	"AttestedLeaf.EntryID":           "digest input; see idschema.AllowedInternalIDTypes",
	"ScanCursorChange.NewAfterCurrency": "a reconcile scan cursor position. It is a currencies row id, and it is deliberately " +
		"not a uid: the cursor is compared with < / > to resume a scan, which a uid cannot do. It is written to and read " +
		"from the audit trail by this library only, and never reaches a consumer -- ScanCursorChange has no json tags and " +
		"no handler serializes it",
	"ScanCursorChange.OldAfterCurrency": "the previous value of the cursor above, same reasoning",
}

// int64Field is one exported int64 field on one exported core type.
type int64Field struct {
	typeName, fieldName, file string
	line                      int
}

func (f int64Field) qualified() string { return f.typeName + "." + f.fieldName }

// TestEveryInt64OnACoreTypeIsClassified is the type-side half of I-18.
//
// The key-derived half (TestNoInternalIDFieldsInCoreTypes) stays: it is
// what catches a field actually NAMED journal_id, including on types this
// one does not reach. This half catches the same leak under any other name,
// which is the case the reviewer walked through.
func TestEveryInt64OnACoreTypeIsClassified(t *testing.T) {
	fields := exportedInt64Fields(t, ".")

	// Fail-closed: a parse regression that finds nothing reads as a pass
	// (working-agreements.md §3). The package had 66 such fields when this
	// gate was written.
	require.GreaterOrEqualf(t, len(fields), 50,
		"only %d exported int64 field(s) were found on exported core types. This package has around sixty-six; "+
			"a scan that finds almost none is a broken scan, not a clean package", len(fields))

	var unclassified []string
	seen := map[string]bool{}
	for _, f := range fields {
		seen[f.qualified()] = true
		seen[f.fieldName] = true
		if _, ok := externalNamespaceInt64Fields[f.fieldName]; ok {
			continue
		}
		if _, ok := quantityInt64Fields[f.fieldName]; ok {
			continue
		}
		if _, ok := internalIDInt64Fields[f.qualified()]; ok {
			continue
		}
		unclassified = append(unclassified, f.file+":"+itoa(f.line)+": "+f.qualified())
	}
	sort.Strings(unclassified)

	assert.Emptyf(t, unclassified,
		"these exported core types carry an int64 field that says nothing about what it identifies:\n\t%s\n\n"+
			"I-18: core types speak uids exclusively. An int64 crossing this boundary is either an identifier from a "+
			"namespace the CALLER owns (account holder, chain id, actor) or a quantity -- never a row id from this "+
			"library's storage. The banned-key gate next door derives its list from the schema's column NAMES, so it "+
			"cannot see one under a new name: `EventRef int64` on core.JournalInput passed every gate in the repository "+
			"except the API snapshot, whose remedy is to regenerate the snapshot (F-7).\n\n"+
			"If the field is a uid, make it a string. If it is genuinely one of the three cases above, add it to "+
			"externalNamespaceInt64Fields / quantityInt64Fields / internalIDInt64Fields with the reason.",
		strings.Join(unclassified, "\n\t"))

	// A stale entry silently pre-approves a future field that reuses the
	// name -- the same rot idschema.VerifyAllowlist exists to prevent.
	for name := range externalNamespaceInt64Fields {
		assert.Truef(t, seen[name], "stale entry %q: no exported core type has an int64 field by that name any more", name)
	}
	for name := range quantityInt64Fields {
		assert.Truef(t, seen[name], "stale entry %q: no exported core type has an int64 field by that name any more", name)
	}
	for name := range internalIDInt64Fields {
		assert.Truef(t, seen[name], "stale entry %q: no exported core type has that field any more", name)
	}
}

// exportedInt64Fields returns every exported int64 (or *int64 / []int64)
// field declared on an exported struct type in dir's non-test sources.
func exportedInt64Fields(t *testing.T, dir string) []int64Field {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	require.NoError(t, err)

	fset := token.NewFileSet()
	var out []int64Field
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		require.NoErrorf(t, err, "parse %s", path)

		ast.Inspect(file, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok || !ts.Name.IsExported() {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, fld := range st.Fields.List {
				if !isInt64Expr(fld.Type) {
					continue
				}
				for _, name := range fld.Names {
					if !name.IsExported() {
						continue
					}
					out = append(out, int64Field{
						typeName:  ts.Name.Name,
						fieldName: name.Name,
						file:      filepath.Base(path),
						line:      fset.Position(name.Pos()).Line,
					})
				}
			}
			return true
		})
	}
	return out
}

// isInt64Expr matches int64, *int64 and []int64. A named type whose
// underlying type is int64 is deliberately NOT matched: naming it is
// already the statement of intent this gate is asking for.
func isInt64Expr(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name == "int64"
	case *ast.StarExpr:
		return isInt64Expr(v.X)
	case *ast.ArrayType:
		return v.Len == nil && isInt64Expr(v.Elt)
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
