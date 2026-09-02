package core_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// moneyPathInputs are the operation inputs docs/INVARIANTS.md I-3 covers:
// they move money or advance a money-bearing state machine, so each MUST
// carry an IdempotencyKey.
var moneyPathInputs = map[string]string{
	"JournalInput":            "posts a journal (every reversal form funnels through it too)",
	"ReserveInput":            "locks funds",
	"SettleInput":             "moves a reservation to a terminal status",
	"SettlePartialInput":      "accumulator: settled_amount += x, needs a durable per-application record",
	"ReleaseInput":            "moves a reservation to a terminal status",
	"FinalizeSettlementInput": "moves a reservation to a terminal status",
	"AddPendingInput":         "credits the pending balance",
	"ConfirmPendingInput":     "moves pending balance into the real one",
	"CancelPendingInput":      "reverses a pending credit",
	"CreateBookingInput":      "opens a lifecycle instance",
	"TransitionInput":         "advances a lifecycle instance",
}

// exemptInputs are the inputs I-3 deliberately does NOT cover, each with the
// reason it needs no key. A key here would be pure cost: these are
// self-idempotent by construction, or not operation inputs at all.
var exemptInputs = map[string]string{
	"CurrencyInput":            "natural-key insert: code is UNIQUE, so a replay is a duplicate-key conflict",
	"ClassificationInput":      "natural-key insert: code is UNIQUE",
	"JournalTypeInput":         "natural-key insert: code is UNIQUE",
	"TemplateInput":            "natural-key insert: code is UNIQUE",
	"AccountPolicyInput":       "upsert at an exact dimension; account_policy_changes records each application",
	"ClosePeriodInput":         "append-only, latest-row-wins: a duplicate line leaves the active line unchanged",
	"AddressRegistrationInput": "upsert on a deterministically derived address (EnsureAddress)",
	"EntryInput":               "not an operation input -- a field of JournalInput, which carries the key",
	"TemplateLineInput":        "not an operation input -- a field of TemplateInput",
}

// TestIdempotencyKeyScopeMatchesInvariantI3 is the pin for B-m9.
//
// I-3 used to open with "Every state-changing operation requires an
// idempotency_key" -- a universal claim that every configuration write in
// this package falsifies and always has. An invariant whose letter is
// contradicted by shipped code is worse than no invariant: it reads as an
// unfulfilled promise about six real paths, which invites someone to "fix"
// them by bolting keys onto self-idempotent upserts.
//
// So the doc was narrowed to money movement, and this test is what keeps the
// narrowing honest in both directions. A new money-path input that forgets
// the key is red. A new configuration input that nobody classified is also
// red -- it forces the author to state which side of I-3 it lands on rather
// than inheriting an answer by accident.
func TestIdempotencyKeyScopeMatchesInvariantI3(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	require.NoError(t, err)
	pkg, ok := pkgs["core"]
	require.True(t, ok, "package core not found in .")

	withKey := map[string]bool{}
	for _, file := range pkg.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok || !strings.HasSuffix(ts.Name.Name, "Input") {
				return true
			}
			has := false
			for _, f := range st.Fields.List {
				for _, name := range f.Names {
					if name.Name == "IdempotencyKey" {
						has = true
					}
				}
			}
			withKey[ts.Name.Name] = has
			return true
		})
	}
	require.NotEmpty(t, withKey, "the AST scan found no *Input types -- the gate is not actually inspecting anything")

	var missingKey, unexpectedKey, unclassified []string
	for name, has := range withKey {
		_, money := moneyPathInputs[name]
		_, exempt := exemptInputs[name]
		switch {
		case money && exempt:
			t.Errorf("%s is on both I-3 lists in this test -- pick one", name)
		case money && !has:
			missingKey = append(missingKey, name)
		case exempt && has:
			unexpectedKey = append(unexpectedKey, name)
		case !money && !exempt:
			unclassified = append(unclassified, name)
		}
	}
	sort.Strings(missingKey)
	sort.Strings(unexpectedKey)
	sort.Strings(unclassified)

	assert.Empty(t, missingKey,
		"I-3 lists these as money-path inputs but they carry no IdempotencyKey field: %v", missingKey)
	assert.Empty(t, unexpectedKey,
		"these are listed as exempt from I-3 but carry an IdempotencyKey -- either the exemption is wrong or the field is: %v", unexpectedKey)
	assert.Empty(t, unclassified,
		"new *Input type(s) %v are on neither I-3 list. Decide: does this operation move money or advance a money-bearing "+
			"state machine? Then add an IdempotencyKey and list it in moneyPathInputs, or list it in exemptInputs with the "+
			"reason it is self-idempotent -- and keep docs/INVARIANTS.md I-3's exclusion table in step", unclassified)

	// Guard the lists themselves against rot: an entry naming a type that no
	// longer exists would silently stop asserting anything.
	for name := range moneyPathInputs {
		_, found := withKey[name]
		assert.True(t, found, "moneyPathInputs names %s, which no longer exists in package core", name)
	}
	for name := range exemptInputs {
		_, found := withKey[name]
		assert.True(t, found, "exemptInputs names %s, which no longer exists in package core", name)
	}
}
