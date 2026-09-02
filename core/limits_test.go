package core

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFreeformFieldLimits is the pin for the lead's addendum to the
// 2026-09-02 audit (from w1-ledgerstore's sibling scan): JournalInput's
// Metadata and Source had no upper bound at any layer.
//
// The HTTP surface caps a request body (Config.MaxBodyBytes), which hides the
// gap for one consumption mode -- but library mode has no such cap, so a
// consumer could hand PostJournal a megabyte-sized metadata map and the
// ledger would store it on an append-only table it can never compact.
// "The body limit covers it" is exactly the shape of reasoning that leaves
// the other mode unprotected, which is why the check is in core.
func TestFreeformFieldLimits(t *testing.T) {
	valid := func() JournalInput {
		return JournalInput{
			IdempotencyKey: "k-1",
			Entries: []EntryInput{
				{AccountHolder: 1, CurrencyUID: "cur", ClassificationUID: "cls", EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(1)},
				{AccountHolder: -1, CurrencyUID: "cur", ClassificationUID: "cls", EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(1)},
			},
		}
	}

	t.Run("a normal write is unaffected", func(t *testing.T) {
		in := valid()
		in.Source = "worker"
		in.Metadata = map[string]string{"tx_hash": strings.Repeat("a", 66), "chain_id": "1"}
		require.NoError(t, in.Validate(), "the bounds must be generous enough that real metadata passes")
	})

	t.Run("oversized source", func(t *testing.T) {
		in := valid()
		in.Source = strings.Repeat("s", MaxSourceLen+1)
		err := in.Validate()
		require.ErrorIs(t, err, ErrInvalidInput)
		assert.Contains(t, err.Error(), "source")
	})

	t.Run("too many metadata keys", func(t *testing.T) {
		in := valid()
		in.Metadata = map[string]string{}
		for i := 0; i <= MaxMetadataKeys; i++ {
			in.Metadata[strings.Repeat("k", 1)+string(rune('a'+i%26))+strings.Repeat("x", i)] = "v"
		}
		require.ErrorIs(t, in.Validate(), ErrInvalidInput)
	})

	t.Run("oversized single value", func(t *testing.T) {
		in := valid()
		in.Metadata = map[string]string{"blob": strings.Repeat("v", MaxMetadataValueLen+1)}
		require.ErrorIs(t, in.Validate(), ErrInvalidInput)
	})

	t.Run("many medium values adding up", func(t *testing.T) {
		in := valid()
		in.Metadata = map[string]string{}
		for i := 0; i < MaxMetadataKeys; i++ {
			in.Metadata[string(rune('a'+i%26))+strings.Repeat("k", i+1)] = strings.Repeat("v", MaxMetadataValueLen)
		}
		require.ErrorIs(t, in.Validate(), ErrInvalidInput,
			"a total bound is what keeps per-entry limits from being summed around")
	})

	t.Run("the bound reaches bookings and transitions too", func(t *testing.T) {
		booking := CreateBookingInput{
			ClassificationCode: "deposit", AccountHolder: 1, CurrencyUID: "cur",
			Amount: decimal.NewFromInt(1), IdempotencyKey: "k",
			Metadata: map[string]string{"blob": strings.Repeat("v", MaxMetadataValueLen+1)},
		}
		require.ErrorIs(t, booking.Validate(), ErrInvalidInput)

		transition := TransitionInput{
			BookingUID: "bk", ToStatus: "confirmed", IdempotencyKey: "k",
			Source: strings.Repeat("s", MaxSourceLen+1),
		}
		require.ErrorIs(t, transition.Validate(), ErrInvalidInput)
	})
}

// TestFreeformFieldLimits_EveryInputWithThoseFieldsChecksThem is the
// completeness half: the fix is only as good as its coverage, and coverage
// by inspection is how the original gap survived. This walks core's own
// source and asserts that every input type declaring a `metadata` or
// `source` field AND having a Validate method calls the shared check.
func TestFreeformFieldLimits_EveryInputWithThoseFieldsChecksThem(t *testing.T) {
	fset := token.NewFileSet()
	pkg, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.SkipObjectResolution)
	require.NoError(t, err)

	// Types declaring a metadata or source json field.
	freeform := map[string]bool{}
	// Types with a Validate method, and whether its body calls the check.
	validates := map[string]bool{}
	checked := map[string]bool{}

	for _, p := range pkg {
		for _, file := range p.Files {
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						ts, ok := spec.(*ast.TypeSpec)
						if !ok {
							continue
						}
						st, ok := ts.Type.(*ast.StructType)
						if !ok {
							continue
						}
						for _, field := range st.Fields.List {
							if field.Tag == nil {
								continue
							}
							tag := field.Tag.Value
							if strings.Contains(tag, `json:"metadata`) || strings.Contains(tag, `json:"source`) {
								freeform[ts.Name.Name] = true
							}
						}
					}
				case *ast.FuncDecl:
					if d.Name.Name != "Validate" || d.Recv == nil || len(d.Recv.List) == 0 || d.Body == nil {
						continue
					}
					recv := recvTypeName(d.Recv.List[0].Type)
					validates[recv] = true
					ast.Inspect(d.Body, func(n ast.Node) bool {
						call, ok := n.(*ast.CallExpr)
						if !ok {
							return true
						}
						if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "validateFreeformFields" {
							checked[recv] = true
						}
						return true
					})
				}
			}
		}
	}

	require.NotEmpty(t, freeform, "no metadata/source-bearing types found -- the walk regressed, not the package")

	var missing []string
	for name := range freeform {
		if !validates[name] {
			// A read model (Event, Booking, Journal) rather than an input:
			// nothing validates it, because the ledger produced it.
			continue
		}
		if !checked[name] {
			missing = append(missing, name)
		}
	}
	assert.Empty(t, missing,
		"input type(s) declare a metadata/source field and have a Validate method that does not call validateFreeformFields -- "+
			"an unbounded free-form field on any write path is the whole finding, not just JournalInput's")
}

func recvTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvTypeName(t.X)
	case *ast.Ident:
		return t.Name
	default:
		return ""
	}
}
