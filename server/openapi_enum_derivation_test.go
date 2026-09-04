package server

// P-3 (2026-09-03 independent review): the twenty-two OpenAPI contract gates
// are thorough about SHAPE -- both directions, types, formats, required,
// nesting, version -- and say nothing about VALUES. No `enum:` in
// docs/openapi.yaml is derived from the Go constants that produce it, so
// adding a fifth core.BalanceRole or a fifth core.ReservationStatus leaves
// the spec quietly describing four.
//
// The consequence is the one I-44 already paid for once on the holder-kind
// vocabulary: a consumer generates types from the spec, exhaustively
// switches on the enum, and meets a value the spec never mentioned. For
// BalanceRole the same drift is worse than a missing case, because that
// value decides whether an amount is counted as a liability at all
// (I-11 / I-37).
//
// So: every enum whose values come from a Go constant set is derived from
// that set, and every enum that does not is registered with the reason. A
// new enum in the spec is red until it is one or the other, which is the
// same fail-closed contract postgres/grant_coverage_test.go uses for
// tables.

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
	"gopkg.in/yaml.v3"
)

// publishedGoVocabularies are the Go named string types whose constant set
// is published in docs/openapi.yaml as an enum.
//
// Keyed by Go type rather than by OpenAPI component because most of these
// vocabularies appear in the spec BOTH as a named component and inline in
// individual schemas -- ReservationStatus is a component, a query
// parameter and a response field. Deriving only the named copy would leave
// the inline ones free to fall behind, which is a spec that contradicts
// itself. The rule below therefore holds every occurrence.
var publishedGoVocabularies = map[string]string{
	"core.EntryType":                  "debit / credit on a journal entry",
	"core.NormalSide":                 "a classification's polarity",
	"core.HolderRole":                 "user / system side of a template line",
	"core.BalanceRole":                "which liquidity bucket a classification's balance counts in (I-11 / I-37)",
	"core.AccountPolicyStatus":        "an account policy's lifecycle state",
	"core.ReservationStatus":          "a reservation's lifecycle state",
	"presets.DepositToleranceOutcome": "how a deposit that did not match its expected amount was resolved",
	"core.HolderTxKind":               "the product vocabulary a host app labels a holder's transactions with (I-44)",
}

// deliberatelyNarrowedEnums are spec enums that are a published vocabulary
// MINUS specific values, on purpose. They are derived too: the spec list
// must equal the Go set with exactly the named values removed, so the Go
// set growing a value grows this one as well.
//
// Without this they would be indistinguishable from the drift the near-miss
// rule below exists to catch -- which is the point. A vocabulary that is
// published in two widths needs the difference written down somewhere a
// machine reads, not inferred from two YAML blocks that happen to differ.
var deliberatelyNarrowedEnums = map[string]struct{ goType, omits, reason string }{
	"adjustment,deposit,fee,other,transfer,withdrawal": {
		goType: "core.HolderTxKind",
		omits:  "",
		reason: "a holder TRANSACTION always has a kind on the wire: the read path " +
			"(postgres/sql/queries/holder.sql) resolves HolderTxKindNone to HolderTxKindOther rather than emitting " +
			"\"\", so the empty value is reachable on a journal type (which may be untagged) and not on a transaction",
	},
}

// inlineEnumsWithoutAGoConstantSet registers every OTHER `enum:` in the
// spec -- the ones written inline rather than as a named component -- with
// the reason it is not derived. Keyed by the exact sorted value list, since
// an inline enum has no name to key on.
//
// The register is closed the way the others in this repo are: adding an
// entry is an edit a reviewer reads, and the point of it is that an enum
// with a Go constant set behind it must be DERIVED rather than listed here.
var inlineEnumsWithoutAGoConstantSet = map[string]string{
	"ready": "a literal in one health-probe response body, not a vocabulary anything switches on",
	"settled": "the single status a settle/finalize response can carry; it is the endpoint's contract, " +
		"not a set (the set is ReservationStatus, derived above)",
	"settling":    "as above, for the partial-settle response",
	"released":    "as above, for the release response",
	"ignored":     "the single status a duplicate webhook delivery reports",
	"deactivated": "the single status a deactivate response carries",
	"btc,custom,evm": "the channel adapter names the server has routes for. These are registered at runtime by the " +
		"consumer (server.WithChannelAdapter), so there is no Go constant set to derive from -- the spec lists the " +
		"ones this repository ships",
	"in,out": "direction of a holder-facing transaction row; a presentation split of entry_type, not a stored value",
}

// TestOpenAPIEnumsAreDerivedFromGoConstants is the fail-closed half: every
// enum in the spec is either exactly a published Go vocabulary, or
// registered as not being one -- and no enum may be a NEAR-MISS of a
// published vocabulary, which is what drift looks like.
func TestOpenAPIEnumsAreDerivedFromGoConstants(t *testing.T) {
	constants := map[string][]string{}
	for pkg, dir := range map[string]string{"core": "core", "presets": "presets"} {
		for name, values := range stringConstantSetsIn(t, filepath.Join("..", dir)) {
			constants[pkg+"."+name] = values
		}
	}

	specEnums := allSpecEnums(t)
	require.GreaterOrEqualf(t, len(specEnums), 20,
		"only %d enum(s) were found in docs/openapi.yaml -- the spec has around twenty-five, so a scan that finds "+
			"almost none is a broken scan reading as a pass", len(specEnums))

	published := map[string][]string{}
	for goType, purpose := range publishedGoVocabularies {
		values, ok := constants[goType]
		require.Truef(t, ok, "publishedGoVocabularies names %s (%s), and no string constants of that type are declared", goType, purpose)
		published[goType] = values

		found := false
		for _, spec := range specEnums {
			if equalStrings(spec, values) {
				found = true
			}
		}
		assert.Truef(t, found,
			"no enum in docs/openapi.yaml lists exactly the constants of %s: [%s].\n\n"+
				"The spec is what a consumer generates their types from, so a value in the Go set and not the spec "+
				"arrives at an exhaustive switch with no case for it. For BalanceRole that value decides whether an "+
				"amount counts as a liability at all (I-11 / I-37), so a consumer holding a stale copy of the "+
				"vocabulary computes a wrong balance rather than missing a branch (P-3)",
			goType, strings.Join(values, ", "))
	}

	for _, spec := range specEnums {
		key := strings.Join(spec, ",")

		// A near miss is the drift this gate exists for: the Go set grew a
		// value and one of the spec's copies did not. A single-value enum
		// is exempt -- those are literals in one response body ("status":
		// "settled"), not a copy of a vocabulary, and every vocabulary here
		// would contain them.
		if narrowed, ok := deliberatelyNarrowedEnums[key]; ok {
			full, known := published[narrowed.goType]
			require.Truef(t, known, "deliberatelyNarrowedEnums[%q] names %s, which is not a published vocabulary", key, narrowed.goType)
			want := make([]string, 0, len(full))
			for _, v := range full {
				if v != narrowed.omits {
					want = append(want, v)
				}
			}
			assert.Equalf(t, want, spec,
				"docs/openapi.yaml declares enum [%s], registered as %s minus %q (%s), but %s minus %q is [%s]. "+
					"A narrowed copy has to be derived from the wide one or it drifts exactly like an underived one",
				key, narrowed.goType, narrowed.omits, narrowed.reason, narrowed.goType, narrowed.omits, strings.Join(want, ", "))
			continue
		}

		if len(spec) > 1 {
			for goType, values := range published {
				if equalStrings(spec, values) {
					continue
				}
				assert.Falsef(t, isSubset(spec, values) || isSubset(values, spec),
					"docs/openapi.yaml declares enum [%s], which is a strict subset or superset of %s's constants "+
						"[%s]. That is what a vocabulary looks like after one of its copies stopped being updated: "+
						"the spec has a component AND inline copies of most of these, and only one of them being "+
						"right is a spec that contradicts itself (P-3)",
					key, goType, strings.Join(values, ", "))
			}
		}

		if publishedSomewhere(spec, published) {
			continue
		}
		_, registered := inlineEnumsWithoutAGoConstantSet[key]
		assert.Truef(t, registered,
			"docs/openapi.yaml declares enum [%s] and nothing derives it.\n\n"+
				"If a Go constant set produces these values, add its type to publishedGoVocabularies so the two "+
				"cannot drift. If it is a literal in one response body, or a vocabulary registered at runtime, add "+
				"it to inlineEnumsWithoutAGoConstantSet with the reason (P-3)", key)
	}

	present := map[string]bool{}
	for _, spec := range specEnums {
		present[strings.Join(spec, ",")] = true
	}
	for key := range inlineEnumsWithoutAGoConstantSet {
		assert.Truef(t, present[key], "stale entry [%s]: no enum in the spec has those values any more -- delete it", key)
	}
	for key := range deliberatelyNarrowedEnums {
		assert.Truef(t, present[key], "stale entry [%s]: no enum in the spec has those values any more -- delete it", key)
	}
}

func publishedSomewhere(spec []string, published map[string][]string) bool {
	for _, values := range published {
		if equalStrings(spec, values) {
			return true
		}
	}
	return false
}

// isSubset reports whether every value in a appears in b, with a strictly
// smaller than b.
func isSubset(a, b []string) bool {
	if len(a) >= len(b) {
		return false
	}
	set := make(map[string]bool, len(b))
	for _, v := range b {
		set[v] = true
	}
	for _, v := range a {
		if !set[v] {
			return false
		}
	}
	return true
}

// --- helpers ---

// stringConstantSetsIn returns, for each named string type declared in dir,
// the sorted set of constant values declared with that type.
func stringConstantSetsIn(t *testing.T, dir string) map[string][]string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	require.NoError(t, err)

	fset := token.NewFileSet()
	out := map[string][]string{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		require.NoErrorf(t, err, "parse %s", path)

		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				ident, ok := vs.Type.(*ast.Ident)
				if !ok {
					continue
				}
				for _, v := range vs.Values {
					lit, ok := v.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					s, err := strconv.Unquote(lit.Value)
					if err != nil {
						continue
					}
					out[ident.Name] = append(out[ident.Name], s)
				}
			}
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

// allSpecEnums returns every `enum:` list anywhere in the document, sorted
// within each list.
func allSpecEnums(t *testing.T) [][]string {
	t.Helper()
	raw, err := os.ReadFile("../docs/openapi.yaml")
	require.NoError(t, err, "read docs/openapi.yaml")

	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &doc), "parse docs/openapi.yaml")

	var out [][]string
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			if values, ok := enumValues(v["enum"]); ok {
				out = append(out, values)
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(doc)
	return out
}

func enumValues(n any) ([]string, bool) {
	list, ok := n.([]any)
	if !ok || len(list) == 0 {
		return nil, false
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			return nil, false // a non-string enum is not a vocabulary this gate reads
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out, true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
