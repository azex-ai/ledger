package core_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pinReference matches a package-qualified test name as INVARIANTS.md cites it,
// e.g. `postgres.TestReversalChainIntegrity` or `core.FuzzJournalValidate`.
var pinReference = regexp.MustCompile("`([a-z][a-z0-9_]*(?:/[a-z][a-z0-9_]*)*)\\.((?:Test|Fuzz|Benchmark)[A-Za-z0-9_]+)`")

// bareReference matches a pin cited without a package qualifier, e.g.
// `TestPartitions_EnsureMonthlyPartitions`. I-13 cites its pins this way, so a
// package-qualified-only check silently skipped them.
var bareReference = regexp.MustCompile("`((?:Test|Fuzz|Benchmark)[A-Za-z0-9_]+)`")

// prefixReference matches a family citation ending in `_*`, e.g.
// `core.TestCanonicalBatchDigest_*`, optionally package-qualified. Five such
// citations existed when this check was written and NONE of them matched the
// two regexps above -- the trailing `*` is not in their character classes, so
// the whole token failed to match and was skipped in silence. Five invariants
// therefore read as pinned while nothing verified their pins at all: the exact
// failure this file exists to prevent, inside this file. Reported by
// p5-authsig during the 2026-08-21 wave.
//
// Residual limitation, disclosed rather than hidden: a family citation is
// satisfied by ONE surviving member. If a table of five vector tests loses
// four, this stays green while the invariant is materially less pinned.
// Requiring explicit enumeration would be stricter, but five families of
// table-driven vectors would then go stale a different way; the bug worth
// fixing here was the silent skip.
var prefixReference = regexp.MustCompile("`(?:([a-z][a-z0-9_]*(?:/[a-z][a-z0-9_]*)*)\\.)?((?:Test|Fuzz|Benchmark)[A-Za-z0-9_]*)_\\*`")

// testFuncDecl matches a test/fuzz/benchmark function declaration.
var testFuncDecl = regexp.MustCompile(`(?m)^func ((?:Test|Fuzz|Benchmark)[A-Za-z0-9_]+)\(`)

// TestInvariantsDocPinsAllExist enforces the contract docs/INVARIANTS.md states
// about itself: "The 'Pinned by' section is the contract. If a test name
// disappears, either (a) the invariant is no longer being checked — fix it — or
// (b) the test was renamed; update this doc."
//
// That was written as a rule for humans to remember, and nothing checked it. An
// invariant whose pin was renamed or deleted still reads as guaranteed, which is
// the worst failure mode this document can have: it is the canonical statement
// of what the ledger promises, and a silently unpinned entry is a promise
// nothing verifies (working-agreements §5, and §3 — an unrun check must never
// read as a pass).
//
// Also checks the package qualifier, so moving a test between packages without
// updating the doc is caught too.
// buildDeclaredTestIndex maps every declared Test/Fuzz/Benchmark function
// name to the set of package directories that declare it. Shared by
// TestInvariantsDocPinsAllExist (existence + package-placement checks) and
// TestInvariantsDocEveryInvariantHasPinnedBy (per-section "does this
// citation resolve to anything real" check) so both consume the identical
// resolution logic rather than two copies that could drift apart.
func buildDeclaredTestIndex(t *testing.T) map[string]map[string]bool {
	t.Helper()
	declared := map[string]map[string]bool{}
	err := filepath.WalkDir("..", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		pkgDir := filepath.ToSlash(filepath.Dir(path))
		pkgDir = strings.TrimPrefix(pkgDir, "../")
		if pkgDir == ".." {
			pkgDir = "."
		}
		for _, m := range testFuncDecl.FindAllStringSubmatch(string(src), -1) {
			if declared[m[1]] == nil {
				declared[m[1]] = map[string]bool{}
			}
			declared[m[1]][pkgDir] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo for test declarations: %v", err)
	}
	return declared
}

func TestInvariantsDocPinsAllExist(t *testing.T) {
	raw, err := os.ReadFile("../docs/INVARIANTS.md")
	if err != nil {
		t.Fatalf("read INVARIANTS.md: %v", err)
	}

	declared := buildDeclaredTestIndex(t)

	refs := pinReference.FindAllStringSubmatch(string(raw), -1)
	if len(refs) == 0 {
		t.Fatal("no pin references found -- the regexp or the doc's citation style changed")
	}

	var missing, wrongPkg []string
	seen := map[string]bool{}
	for _, m := range refs {
		pkg, fn := m[1], m[2]
		key := pkg + "." + fn
		if seen[key] {
			continue
		}
		seen[key] = true

		dirs, ok := declared[fn]
		if !ok {
			missing = append(missing, key)
			continue
		}
		// The doc cites the package name; accept any directory whose final
		// segment matches (service/delivery is cited as either form).
		match := false
		for dir := range dirs {
			if dir == pkg || strings.HasSuffix(dir, "/"+pkg) || filepath.Base(dir) == filepath.Base(pkg) {
				match = true
				break
			}
		}
		if !match {
			got := make([]string, 0, len(dirs))
			for dir := range dirs {
				got = append(got, dir)
			}
			sort.Strings(got)
			wrongPkg = append(wrongPkg, key+" (declared in "+strings.Join(got, ", ")+")")
		}
	}

	// Unqualified citations: existence only, no package to check.
	for _, m := range bareReference.FindAllStringSubmatch(string(raw), -1) {
		fn := m[1]
		if seen[fn] {
			continue
		}
		seen[fn] = true
		if _, ok := declared[fn]; !ok {
			missing = append(missing, fn+" (cited without a package)")
		}
	}

	// Family citations (`Prefix_*`): require at least one declared test whose
	// name starts with the prefix, in the cited package when one is given.
	for _, m := range prefixReference.FindAllStringSubmatch(string(raw), -1) {
		pkg, prefix := m[1], m[2]+"_"
		key := prefix + "*"
		if pkg != "" {
			key = pkg + "." + key
		}
		if seen[key] {
			continue
		}
		seen[key] = true

		found := 0
		for fn, dirs := range declared {
			if !strings.HasPrefix(fn, prefix) {
				continue
			}
			if pkg == "" {
				found++
				continue
			}
			for dir := range dirs {
				if dir == pkg || strings.HasSuffix(dir, "/"+pkg) || filepath.Base(dir) == filepath.Base(pkg) {
					found++
					break
				}
			}
		}
		if found == 0 {
			missing = append(missing, key+" (family citation matched no test)")
		}
	}

	sort.Strings(missing)
	sort.Strings(wrongPkg)
	for _, k := range missing {
		t.Errorf("INVARIANTS.md cites %s as a pin, but no such test exists -- "+
			"either the invariant is no longer checked, or the test was renamed and the doc was not", k)
	}
	for _, k := range wrongPkg {
		t.Errorf("INVARIANTS.md cites the wrong package for %s", k)
	}
	t.Logf("verified %d distinct pin references", len(seen))
}

// ---------------------------------------------------------------------------
// F-m2 (2026-09-02 audit): two more shapes of "the citation exists" not being
// the same as "the citation checks anything". docs/INVARIANTS.md's own "How
// to add a new invariant" rule #4 says every invariant gets a Pinned by test
// -- I-7 and I-34 had none, and nothing above checked that a section has a
// Pinned by block AT ALL, only that names WITHIN one resolve. And a pin
// resolving to a real test function is not the same as that test function
// actually exercising the invariant's mechanism: B's audit finding
// (TestExecuteTemplateBatch_GlobalLockOrder_PreventsCrossJournalDeadlock
// never once calls ExecuteTemplateBatch) is the general shape this section
// checks for mechanically, via go/ast, rather than requiring the next
// reviewer to read every pin's body by hand.
// ---------------------------------------------------------------------------

// invariantSection is one `## I-N` block's raw text, from its heading up to
// (not including) the next `## I-` heading or EOF.
type invariantSection struct {
	number string // "I-7"
	body   string
}

// splitInvariantSections partitions raw (docs/INVARIANTS.md's full text)
// into per-invariant blocks using the same heading regexp
// TestInvariantsDocIsOrderedAndGapless already relies on for structure.
func splitInvariantSections(raw string) []invariantSection {
	idx := invariantHeading.FindAllStringSubmatchIndex(raw, -1)
	sections := make([]invariantSection, 0, len(idx))
	for i, m := range idx {
		start := m[0]
		end := len(raw)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		sections = append(sections, invariantSection{
			number: "I-" + raw[m[2]:m[3]],
			body:   raw[start:end],
		})
	}
	return sections
}

// blockBetween returns the text of the sub-block starting at the first
// occurrence of startMarker (inclusive), up to (not including) the next
// line that begins a new **Bold** sub-heading, or end of body. Used to
// isolate a section's "Enforced by" or "Pinned by" text from its siblings.
var subHeadingLine = regexp.MustCompile(`(?m)^\*\*[A-Za-z][A-Za-z ]*\*\*`)

func blockBetween(body, startMarker string) string {
	i := strings.Index(body, startMarker)
	if i < 0 {
		return ""
	}
	rest := body[i+len(startMarker):]
	if loc := subHeadingLine.FindStringIndex(rest); loc != nil {
		rest = rest[:loc[0]]
	}
	return stripBlockquoteLines(rest)
}

// stripBlockquoteLines drops markdown blockquote lines (`> ...`) from a
// block before it is scanned for citations. Several sections carry a `>`
// historical annotation directly below their Pinned by bullets -- prose
// that mentions a test BY NAME to explain what it once caught (e.g. I-15's
// "Found by `core.TestInvariantsDocPinsAllExist`...") rather than citing it
// as one of this section's own pins. Without this, that kind of mention
// reads as a citation and gets held to a mechanism it was never meant to
// pin.
func stripBlockquoteLines(block string) string {
	lines := strings.Split(block, "\n")
	kept := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), ">") {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n")
}

// TestInvariantsDocEveryInvariantHasPinnedBy pins docs/INVARIANTS.md's own
// "How to add a new invariant" rule #4 ("add at least one test under Pinned
// by") as a machine check instead of a rule nobody runs. Before F-m2, I-7
// and I-34 had gone their entire existence with no Pinned by section at all
// and nothing here said so.
// pinnedBlockHasResolvableCitation reports whether a section's Pinned by
// text contains at least one citation (package-qualified, bare, or a
// `Prefix_*` family) that TestInvariantsDocPinsAllExist's own resolution
// logic would find a real declared test for. Existence only -- package
// placement is that other test's job; this one only asks "is there
// anything here at all".
func pinnedBlockHasResolvableCitation(pinnedBlock string, declared map[string]map[string]bool) bool {
	for _, m := range pinReference.FindAllStringSubmatch(pinnedBlock, -1) {
		if _, ok := declared[m[2]]; ok {
			return true
		}
	}
	for _, m := range bareReference.FindAllStringSubmatch(pinnedBlock, -1) {
		if _, ok := declared[m[1]]; ok {
			return true
		}
	}
	for _, m := range prefixReference.FindAllStringSubmatch(pinnedBlock, -1) {
		prefix := m[2] + "_"
		for fn := range declared {
			if strings.HasPrefix(fn, prefix) {
				return true
			}
		}
	}
	return false
}

// TestInvariantsDocEveryInvariantHasPinnedBy pins docs/INVARIANTS.md's own
// "How to add a new invariant" rule #4 ("add at least one test under Pinned
// by") as a machine check instead of a rule nobody runs. Before F-m2, I-7
// and I-34 had gone their entire existence with no Pinned by section at all
// and nothing here said so.
//
// A **Pinned by** heading with the bullets deleted out from under it read as
// pinned here under the first version of this check (team-lead's mutation,
// 2026-09-02 merge review: emptied I-6's Pinned by list down to the bare
// heading, gate stayed green) -- checking for the heading string alone is
// exactly the "gate that verifies its own shape, not what it promises"
// pattern this whole file exists to catch, reproduced inside the file a
// second time. Now requires the section's Pinned by BLOCK to contain at
// least one citation that resolves to a real declared test (via the same
// resolution TestInvariantsDocPinsAllExist uses, including `Prefix_*`
// family citations resolving to >=1 real function) -- a heading with zero
// bullets, or bullets that are all typos/renamed tests, is treated the same
// as no Pinned by section at all.
func TestInvariantsDocEveryInvariantHasPinnedBy(t *testing.T) {
	raw, err := os.ReadFile("../docs/INVARIANTS.md")
	if err != nil {
		t.Fatalf("read INVARIANTS.md: %v", err)
	}

	declared := buildDeclaredTestIndex(t)

	var missing []string
	for _, sec := range splitInvariantSections(string(raw)) {
		if !strings.Contains(sec.body, "**Pinned by**") {
			missing = append(missing, sec.number+" (no **Pinned by** heading at all)")
			continue
		}
		pinned := blockBetween(sec.body, "**Pinned by**")
		if !pinnedBlockHasResolvableCitation(pinned, declared) {
			missing = append(missing, sec.number+" (**Pinned by** heading present, but no bullet under it resolves to a real test)")
		}
	}
	sort.Strings(missing)
	for _, n := range missing {
		t.Errorf("%s -- docs/INVARIANTS.md's own \"How to add a new invariant\" rule #4 "+
			"requires at least one real pin; a promise nothing verifies is worse than an admitted gap", n)
	}
}

// --- Repo-wide declared-symbol index (go/ast, no type-checking needed) ---

// declaredSymbolIndex answers "does a Go symbol by this name exist anywhere
// in the repo": bare top-level func/type/const/var names, and
// "ReceiverType.Method" pairs for methods (pointer receivers stripped of
// their leading `*`). Deliberately whole-repo, not per-package: the goal is
// catching a citation that doesn't correspond to anything real (a typo, a
// renamed symbol nobody updated the doc for), not enforcing which package
// declared it -- TestInvariantsDocPinsAllExist above already checks package
// placement for the pins themselves.
type declaredSymbolIndex struct {
	bare   map[string]bool
	method map[string]bool // "Type.Method"
}

func buildDeclaredSymbolIndex(t *testing.T) declaredSymbolIndex {
	t.Helper()
	idx := declaredSymbolIndex{bare: map[string]bool{}, method: map[string]bool{}}
	fset := token.NewFileSet()

	err := filepath.WalkDir("..", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// Best-effort: a file that fails to parse standalone (rare;
			// e.g. build-tag-gated files referencing symbols from a
			// sibling file under the same tag) just contributes nothing,
			// rather than failing the whole index.
			return nil
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil || len(d.Recv.List) == 0 {
					idx.bare[d.Name.Name] = true
					continue
				}
				recvType := recvTypeName(d.Recv.List[0].Type)
				if recvType != "" {
					idx.method[recvType+"."+d.Name.Name] = true
				}
				idx.bare[d.Name.Name] = true
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						idx.bare[s.Name.Name] = true
						// Struct fields and interface methods are cited by
						// INVARIANTS.md the same way methods are
						// (`core.TokenConfig.ReconcileFailureLimit`,
						// `core.Metrics.JournalPosted`), so they belong in
						// the index: without them, a citation that names a
						// real field reads as unresolved, which the
						// resolution check below turns into a false red.
						switch ty := s.Type.(type) {
						case *ast.StructType:
							if ty.Fields != nil {
								for _, field := range ty.Fields.List {
									for _, n := range field.Names {
										idx.method[s.Name.Name+"."+n.Name] = true
										idx.bare[n.Name] = true
									}
								}
							}
						case *ast.InterfaceType:
							if ty.Methods != nil {
								for _, m := range ty.Methods.List {
									for _, n := range m.Names {
										idx.method[s.Name.Name+"."+n.Name] = true
										idx.bare[n.Name] = true
									}
								}
							}
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							idx.bare[n.Name] = true
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo for symbol declarations: %v", err)
	}
	return idx
}

// recvTypeName extracts "Foo" from both `Foo` and `*Foo` receiver type
// expressions; generic receivers (`Foo[T]`) resolve to "Foo" too.
func recvTypeName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return recvTypeName(e.X)
	case *ast.Ident:
		return e.Name
	case *ast.IndexExpr:
		return recvTypeName(e.X)
	case *ast.IndexListExpr:
		return recvTypeName(e.X)
	}
	return ""
}

// enforcedSymbolRef matches a backtick-quoted Go-style symbol citation in an
// "Enforced by" bullet, e.g. `core.JournalInput.Validate` or
// `postgres.LedgerStore.postJournalWithQueries`. The package segment may
// contain slashes (`service/delivery.Foo`); the symbol path may itself be
// dotted (Type.Method).
var enforcedSymbolRef = regexp.MustCompile("`([a-z][a-z0-9_]*(?:/[a-z][a-z0-9_]*)*)\\.([A-Za-z_][A-Za-z0-9_]*(?:\\.[A-Za-z_][A-Za-z0-9_]*)*)`")

// nonSymbolExtensions filters out file-path citations that coincidentally
// match enforcedSymbolRef's shape -- `core/reserve.go` parses as
// pkg="core/reserve", symbolPath="go", which is not a symbol reference.
var nonSymbolExtensions = map[string]bool{
	"go": true, "sql": true, "md": true, "yml": true, "yaml": true,
	"json": true, "sh": true, "ts": true, "tsx": true, "js": true,
}

// --- W3 round 2: every citation in an Enforced by block must resolve ---
//
// The round-1 gate held a section's PINS to its Enforced-by symbols, and
// dropped any citation that did not resolve. Team-lead's two mutations
// before the merge both stayed green on that:
//
//	(1) strip every backtick from I-2's Enforced by -- nothing resolves, so
//	    the section had no leaves and was skipped (registered, silently);
//	(2) point I-2's first three citations at symbols that do not exist,
//	    leaving the rest -- the section still had leaves from the survivors,
//	    and a citation naming a symbol this repo does not declare cost
//	    nothing.
//
// Both are the same hole from two ends: the doc's claim about WHICH
// mechanism enforces an invariant was never itself checked. A citation that
// resolves to nothing is a claim about code that does not exist, and it is
// worse than no citation at all, because it reads as one.
//
// So, independently of the pin check:
//
//   - every citation that LOOKS like a Go symbol in a package this repo
//     declares must resolve to a symbol this repo declares (exported or
//     not -- an unexported mechanism is still a real one, it just cannot be
//     a leaf a black-box pin is held to);
//   - every section must have at least one citation that resolves to
//     SOMETHING -- a Go symbol, or a file that exists. Being registered in
//     unresolvableEnforcedCitations does not exempt a section from this: the
//     register says "no EXPORTED symbol to hold a pin to", never "this
//     section cites nothing real".

// repoGoPackages returns the set of package directory names that contain Go
// source. It is what tells `postgres.checkAmountPrecision` (a claim about
// this repo, checkable) from `decimal.Decimal` or `migrate.NewWithSourceInstance`
// (third-party) and from `journals.idempotency_key` or `job.Attempts` (a DB
// column and a local variable, neither of which is a package).
func repoGoPackages(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := filepath.WalkDir("..", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		switch d.Name() {
		case "node_modules", ".git", "web":
			return filepath.SkipDir
		}
		matches, globErr := filepath.Glob(filepath.Join(path, "*.go"))
		if globErr != nil {
			return globErr
		}
		if len(matches) > 0 {
			out[filepath.Base(path)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo for package names: %v", err)
	}
	require.NotEmpty(t, out, "found no Go packages -- the walk regressed")
	return out
}

// repoFileNames returns every file in the repository, by path and by base
// name, so a citation like `postgres/sql/migrations/001_baseline.up.sql` or
// `postgres/convert.go` can be told from one that names a file nobody has.
func repoFileNames(t *testing.T) (byPath, byBase map[string]bool) {
	t.Helper()
	byPath, byBase = map[string]bool{}, map[string]bool{}
	err := filepath.WalkDir("..", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, "../"))
		byPath[rel] = true
		byBase[filepath.Base(rel)] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo for file names: %v", err)
	}
	return byPath, byBase
}

// sqlAndGoCorpus is every .sql file under postgres/sql and every .go file in
// the repository, concatenated as text.
//
// It is what a snake_case citation is checked against. A DB-side name --
// a trigger, a SQL function, a column, a check constraint, a template code --
// exists as TEXT, not as a Go declaration: it is written in a migration, in
// a query, or in a Go string literal that executes it. Text is therefore the
// honest index for it, and "this name appears nowhere in the schema or the
// source" is a claim about the codebase that is checkable and false.
func sqlAndGoCorpus(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir("..", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".sql") && !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		b.Write(body)
		b.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo for the SQL/Go corpus: %v", err)
	}
	require.NotEmpty(t, b.String(), "the SQL/Go corpus is empty -- the walk regressed")
	return b.String()
}

var (
	// backtickToken matches one backtick-quoted token, whatever it is.
	backtickToken = regexp.MustCompile("`([^`]+)`")
	// fileCitation matches a path or bare filename with a known extension,
	// optionally with a `:line` suffix.
	fileCitation = regexp.MustCompile(`^[A-Za-z0-9_./-]+\.(?:go|sql|md|yml|yaml|ts|tsx|js|json|sh)(?::\d+)?$`)
	// bareIdentifier matches a single Go identifier.
	bareIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// goSymbolShaped matches a citation written WITHOUT a package qualifier
	// but shaped like a Go symbol: CamelCase, optionally dotted
	// (`ReverseJournalFraction`, `PendingStore.AddPending`). This document's
	// bare-citation convention (stated in I-13 and I-40) makes these as
	// common as qualified ones, and until round 2's second pass they were
	// checked by nothing: renaming `ReverseJournal` to `ZzReverseJournal`
	// left every gate green.
	// The second character must be lowercase: that is what separates a Go
	// name (`ReverseJournal`, `PendingStore.AddPending`) from a SQL keyword
	// (`UNIQUE`, `MAX`, `NULL`), which the shape alone cannot.
	goSymbolShaped = regexp.MustCompile(`^[A-Z][a-z][A-Za-z0-9]*(\.[A-Za-z0-9]+)*$`)
	// dbObjectShaped matches a snake_case name specific enough to look for:
	// a trigger, a SQL function, a column, a constraint. Short or
	// underscore-free tokens (`uid`, `status`) are prose as often as they
	// are names, and are not checked.
	dbObjectShaped = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// citationAudit is what one Enforced by block claims, and whether those
// claims are true.
type citationAudit struct {
	resolved int      // citations pointing at something that exists
	broken   []string // Go-symbol citations in a repo package that resolve to nothing
}

func auditEnforcedCitations(block string, idx declaredSymbolIndex, pkgs, filesByPath, filesByBase map[string]bool, corpus string) citationAudit {
	out := citationAudit{}
	for _, m := range backtickToken.FindAllStringSubmatch(block, -1) {
		token := strings.TrimSpace(m[1])
		switch {
		case fileCitation.MatchString(token):
			path := strings.SplitN(token, ":", 2)[0]
			// Historical migrations squashed into 001_baseline, and the
			// cross-cutting rule files that live outside this repo
			// (financial.md, deployment.md), are cited as prose about where
			// a decision came from. They count for nothing and are not
			// errors: a missing FILE is a documentation-archaeology
			// question, not a claim about code that does not exist.
			if filesByPath[path] || filesByBase[filepath.Base(path)] {
				out.resolved++
			}
		case strings.Contains(token, "."):
			parts := strings.Split(token, ".")
			leaf := parts[len(parts)-1]
			if !pkgs[parts[0]] || len(parts) < 2 || !bareIdentifier.MatchString(leaf) {
				continue // not a claim about a symbol in this repository
			}
			resolves := idx.bare[leaf]
			if len(parts) > 2 {
				resolves = idx.method[parts[len(parts)-2]+"."+leaf] || idx.bare[leaf]
			}
			if resolves {
				out.resolved++
			} else {
				out.broken = append(out.broken, token)
			}
		default:
			// A mechanism cited without its package. Two shapes, checked
			// differently because they live in different indexes.
			name := strings.SplitN(token, "(", 2)[0]
			switch {
			case goSymbolShaped.MatchString(name) && !testFuncNamePattern.MatchString(strings.Split(name, ".")[0]):
				leaf := name
				if parts := strings.Split(name, "."); len(parts) > 1 {
					leaf = parts[len(parts)-1]
					if idx.method[parts[len(parts)-2]+"."+leaf] {
						out.resolved++
						continue
					}
				}
				if idx.bare[leaf] {
					out.resolved++
				} else {
					out.broken = append(out.broken, token)
				}
			case dbObjectShaped.MatchString(name) && len(name) >= 6 && strings.Contains(name, "_"):
				if strings.Contains(corpus, name) {
					out.resolved++
				} else {
					out.broken = append(out.broken, token)
				}
			}
		}
	}
	sort.Strings(out.broken)
	return out
}

// TestInvariantsEnforcedCitationsResolve is the round-2 gate: what the
// document CLAIMS about the code has to be true of the code.
func TestInvariantsEnforcedCitationsResolve(t *testing.T) {
	raw, err := os.ReadFile("../docs/INVARIANTS.md")
	if err != nil {
		t.Fatalf("read INVARIANTS.md: %v", err)
	}
	idx := buildDeclaredSymbolIndex(t)
	pkgs := repoGoPackages(t)
	filesByPath, filesByBase := repoFileNames(t)
	corpus := sqlAndGoCorpus(t)

	sections := splitInvariantSections(string(raw))
	require.NotEmpty(t, sections, "no invariant sections parsed -- the splitter regressed")

	for _, sec := range sections {
		enforced := blockBetween(sec.body, "**Enforced by**")
		audit := auditEnforcedCitations(enforced, idx, pkgs, filesByPath, filesByBase, corpus)

		for _, citation := range audit.broken {
			t.Errorf("%s's **Enforced by** cites %q, which this repository does not have.\n\n"+
				"Three shapes are checked, each against the index it would live in: a package-qualified symbol (`postgres.Foo`) and a "+
				"bare CamelCase one (`ReverseJournal`) against the Go declarations, and a snake_case name (`journals_no_arbitrary_update`) "+
				"against every .sql and .go file in the repo, since a trigger or column exists as text rather than as a Go declaration.\n\n"+
				"A citation naming something that does not exist is worse than no citation: it reads as a mechanism, it is what the pin "+
				"check holds pins to, and it silently stops holding them to anything. Rename it to what exists (a rename is the usual "+
				"cause), or drop it.", sec.number, citation)
		}

		if audit.resolved == 0 {
			t.Errorf("%s's **Enforced by** names nothing that exists: no symbol in any package of this repository, and no file in it.\n\n"+
				"Whatever enforces this invariant, the document has to point at it -- a section citing only prose can never be checked "+
				"against anything, and stripping the backticks off a section's citations must not be a way to make this gate quiet. "+
				"Registering the section in unresolvableEnforcedCitations does NOT cover this: that register says there is no EXPORTED "+
				"symbol to hold a pin to, not that the section cites nothing real.", sec.number)
		}
	}
}

// selfGatedSection reports whether an invariant's mechanism is a gate test
// file that this same section pins -- i.e. every pin it cites is declared in
// a `_test.go` file its **Enforced by** names.
//
// Two invariants are of this shape by construction: I-50 ("the sign
// convention has one implementation, and THAT FACT IS CHECKED BY MACHINE")
// and I-61 ("core.Metrics has no method without a production call site").
// Their mechanism is the gate, so the pin and the mechanism are the same
// object; holding the pin to a separate exported symbol would mean inventing
// one. Requiring the pins to be declared in the named file is what keeps
// this from being a loophole: move the pin elsewhere, or rename the file,
// and the section stops qualifying.
func selfGatedSection(enforced, body string, testBodies map[string][]testFuncBody) bool {
	gateFiles := enforcedGateFiles(enforced)
	if len(gateFiles) == 0 {
		return false
	}

	pinned := blockBetween(body, "**Pinned by**")
	pins, inGateFile := 0, 0
	for _, m := range append(pinReference.FindAllStringSubmatch(pinned, -1), bareReference.FindAllStringSubmatch(pinned, -1)...) {
		name := m[len(m)-1]
		bodies, ok := testBodies[name]
		if !ok {
			continue
		}
		pins++
		for _, b := range bodies {
			for _, gate := range gateFiles {
				if b.file == gate {
					inGateFile++
				}
			}
		}
	}
	// At least one pin has to BE the gate -- that is what makes the section
	// self-enforcing rather than merely gate-adjacent.
	//
	// It used to be all of them, which had this backwards (F-5,
	// 2026-09-03): a self-gated section could never cite an ordinary
	// behaviour pin alongside its gate without losing its self-gated
	// status. I-61 is exactly that case -- the emission-coverage gate plus
	// the test that actually drives a rollup tick and asserts the gauge --
	// and extra evidence must not cost a section its classification.
	return pins > 0 && inGateFile > 0
}

// enforcedGateFiles returns the `_test.go` files an Enforced by block names
// as the mechanism itself.
func enforcedGateFiles(enforced string) []string {
	var out []string
	for _, m := range backtickToken.FindAllStringSubmatch(enforced, -1) {
		token := strings.TrimSpace(m[1])
		if strings.HasSuffix(token, "_test.go") {
			out = append(out, filepath.Base(token))
		}
	}
	return out
}

// enforcedDBObjects returns the DB-side mechanism names an Enforced by block
// cites: snake_case identifiers long enough to be specific
// (`journals_no_arbitrary_update`, `ledger_unlink_event_journal`,
// `effective_at`). A pin that drives one of these can only name it inside a
// query string, so this is matched against the pin's string literals rather
// than its identifiers.
//
// The length and underscore requirements keep out the tokens that would
// match almost any SQL: `uid`, `journals`, `status`. A DB mechanism whose
// name is that generic cannot be told apart from prose, and is not counted.
func enforcedDBObjects(enforced string) []string {
	var out []string
	for _, m := range backtickToken.FindAllStringSubmatch(enforced, -1) {
		token := strings.TrimSpace(m[1])
		// The doc cites a column with its whole declaration inside one
		// backtick span (`currencies.exponent SMALLINT NOT NULL DEFAULT 18
		// CHECK (0..18)`); the name is the first word of it.
		if i := strings.IndexAny(token, " \t\n"); i > 0 {
			token = token[:i]
		}
		// `ledger_unlink_event_journal(uuid)` -- the doc cites SQL functions
		// with their argument list; the name is what a query string carries.
		if i := strings.Index(token, "("); i > 0 {
			token = token[:i]
		}
		// `classifications.balance_role` names a column. Take the column,
		// not the table: a pin whose SQL merely mentions `classifications`
		// has not shown it touches the constraint on that one column, and
		// admitting the table name here would let almost any query in the
		// package stand in for almost any claim. F-1, 2026-09-03: without
		// this, a doc that cites its mechanism as table.column got no DB
		// object out of the citation at all, so a direct-SQL pin held to
		// exactly that column read as touching nothing.
		if i := strings.LastIndex(token, "."); i >= 0 {
			token = token[i+1:]
			if len(token) < 5 || !regexp.MustCompile(`^[a-z][a-z0-9_]*$`).MatchString(token) {
				continue
			}
			out = append(out, token)
			continue
		}
		if len(token) < 6 || !strings.Contains(token, "_") {
			continue
		}
		if !regexp.MustCompile(`^[a-z][a-z0-9_]*$`).MatchString(token) {
			continue
		}
		out = append(out, token)
	}
	return out
}

// enforcedLeafNames extracts, from an "Enforced by" block's text, the set of
// trailing identifier names (e.g. "Validate" from "JournalInput.Validate",
// or "NewReserverStore" from a bare citation) for every citation that
// resolves to a symbol idx actually contains. Citations that don't resolve
// (third-party package symbols like `decimal.Decimal`, SQL/migration/file
// references, prose that happens to match the shape) are silently dropped --
// this function only returns names this repo can be held to.
func enforcedLeafNames(enforcedBlock string, idx declaredSymbolIndex) map[string]bool {
	leaves := map[string]bool{}
	for _, m := range enforcedSymbolRef.FindAllStringSubmatch(enforcedBlock, -1) {
		symbolPath := m[2]
		parts := strings.Split(symbolPath, ".")
		leaf := parts[len(parts)-1]
		// Unexported leaves (lowercase first rune, e.g.
		// `postgres.LedgerStore.postJournalWithQueries`) are dropped
		// entirely, not just filtered by resolution: almost every pin in
		// this repo lives in an external `_test` package and can never
		// reference an unexported identifier by name regardless of
		// whether it genuinely exercises it, so counting one as a
		// required leaf produces a false failure on every such pin. This
		// mirrors the doc's own intent -- Enforced by describes the
		// mechanism, and the exported entry point is what a black-box
		// test can actually be held to.
		if leaf == "" || !ast.IsExported(leaf) {
			continue
		}
		if len(parts) == 1 {
			if nonSymbolExtensions[leaf] {
				continue
			}
			if idx.bare[leaf] {
				leaves[leaf] = true
			}
			continue
		}
		// Type.Method (or deeper -- only the last two segments are used;
		// anything beyond that is not a shape this doc uses).
		typeName := parts[len(parts)-2]
		if idx.method[typeName+"."+leaf] {
			leaves[leaf] = true
		}
	}
	return leaves
}

// --- Pin test bodies: what identifiers does each cited test actually use? ---

// testFuncBody records one test/fuzz/benchmark function's declaring package
// directory and the set of identifier names its body references (both bare
// calls and `x.Sel` selector expressions) -- a name appearing here means the
// test's source genuinely touches something by that name, not just mentions
// it in a comment or string literal (go/ast walks the syntax tree, not the
// source text).
type testFuncBody struct {
	pkgDir string
	// file is the base name of the _test.go file declaring the function --
	// what selfGatedSection matches an Enforced-by gate-file citation
	// against.
	file      string
	usedNames map[string]bool
	// usedStrings is the body's string literals, concatenated. A DB-side
	// mechanism -- a trigger, a SQL function, a protected column -- can only
	// be referenced from Go by name inside a query string, so an
	// identifier-only view of a pin cannot see that it drives one.
	usedStrings string
}

func buildTestFuncBodyIndex(t *testing.T) map[string][]testFuncBody {
	t.Helper()
	// Pass 1: every function declared in a _test.go file, test or helper,
	// with what its own body touches.
	type rawBody struct {
		pkgDir, file string
		used         map[string]bool
		literals     string
		calls        map[string]bool
	}
	byPkg := map[string]map[string]*rawBody{}
	fset := token.NewFileSet()

	err := filepath.WalkDir("..", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		pkgDir := filepath.ToSlash(filepath.Dir(path))
		pkgDir = strings.TrimPrefix(pkgDir, "../")
		if pkgDir == ".." {
			pkgDir = "."
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			body := &rawBody{
				pkgDir: pkgDir, file: d.Name(),
				used: map[string]bool{}, calls: map[string]bool{},
			}
			var literals strings.Builder
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch e := n.(type) {
				case *ast.SelectorExpr:
					body.used[e.Sel.Name] = true
				case *ast.Ident:
					body.used[e.Name] = true
				case *ast.BasicLit:
					if e.Kind == token.STRING {
						literals.WriteString(e.Value)
						literals.WriteByte('\n')
					}
				case *ast.CallExpr:
					if id, isIdent := e.Fun.(*ast.Ident); isIdent {
						body.calls[id.Name] = true
					}
				}
				return true
			})
			body.literals = literals.String()
			if byPkg[pkgDir] == nil {
				byPkg[pkgDir] = map[string]*rawBody{}
			}
			byPkg[pkgDir][fn.Name.Name] = body
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo for test function bodies: %v", err)
	}

	// Pass 2: fold in one level of same-package test helpers.
	//
	// Round 2: a table-driven pin drives the mechanism from a helper
	// (postgres.TestPresetSolvency_EveryShippedTemplate's cases go through
	// runSolvencyCase, which is where the solvency read actually happens),
	// so a body-only view reports it as touching nothing. One level, not
	// transitive: it covers the "test declares a table, helper runs each
	// row" shape this repo uses without turning every pin into the union of
	// its whole package.
	out := map[string][]testFuncBody{}
	for pkgDir, fns := range byPkg {
		for name, body := range fns {
			if !testFuncNamePattern.MatchString(name) {
				continue
			}
			used := make(map[string]bool, len(body.used))
			for k := range body.used {
				used[k] = true
			}
			literals := body.literals
			for callee := range body.calls {
				helper, ok := fns[callee]
				if !ok || callee == name {
					continue
				}
				for k := range helper.used {
					used[k] = true
				}
				literals += helper.literals
			}
			out[name] = append(out[name], testFuncBody{
				pkgDir: pkgDir, file: body.file, usedNames: used, usedStrings: literals,
			})
		}
	}
	return out
}

var testFuncNamePattern = regexp.MustCompile(`^(?:Test|Fuzz|Benchmark)[A-Za-z0-9_]+$`)

// pkgDirMatches mirrors TestInvariantsDocPinsAllExist's package-matching
// rule: accept any directory whose final path segment equals pkg, or whose
// path ends in "/"+pkg (service/delivery cited as either "delivery" or
// "service/delivery").
func pkgDirMatches(dir, pkg string) bool {
	return dir == pkg || strings.HasSuffix(dir, "/"+pkg) || filepath.Base(dir) == filepath.Base(pkg)
}

// TestInvariantsPinsReferenceEnforcedSymbols is the go/ast half of F-m2: for
// every invariant section whose Enforced by block names at least one Go
// symbol this repo actually declares, every Pinned by test cited in that
// SAME section must reference (call, or use as a selector/type) at least one
// of those symbols somewhere in its body. A pin whose function never once
// touches the mechanism it claims to test -- B's audit finding about
// TestExecuteTemplateBatch_GlobalLockOrder_PreventsCrossJournalDeadlock is
// the concrete precedent -- goes red here without anyone reading the body.
//
// Deliberately conservative in what it flags, per working-agreements §5's
// caution against a gate broad enough to explode the whole document into
// unrelated red: sections whose Enforced by cites nothing resolvable in this
// repo (pure SQL/migration/third-party-package prose, e.g. I-6's "Postgres
// uses NUMERIC(30,18)") are skipped entirely -- there is nothing this check
// could hold such a pin to. Family citations (`Foo_*`) and pins this file
// cannot locate a FuncDecl for are skipped per-pin, not failed, for the same
// reason TestInvariantsDocPinsAllExist treats them differently from a
// concrete named pin.
// citationStyleGapInvariants are sections whose **Enforced by** prose
// doesn't yet back-tick-quote every symbol it means (e.g. I-4's "Reservation
// FSM transition table in core/reserve.go rejects illegal moves" names no
// symbol at all, so this check can only see the one bullet that DOES use a
// backtick -- `postgres.ReserverStore.Reserve` -- and wrongly doubts a pin
// that is actually about the FSM bullet instead).
//
// Running this check for the first time (2026-09-02, F-m2) against every
// section found 146 such mismatches spread across 28 of the 54 invariants
// that exist today -- overwhelmingly this citation-style gap, not bad pins:
// spot-checked a sample (I-4, I-20, I-34, all inside this task's own
// remit) and every one traces to an Enforced-by bullet that describes a
// mechanism in prose without the backtick-quoted symbol this check parses
// for. Fixing 146 citations (in sections spanning nearly every other
// territory's exclusive files) is not a same-task fix, and shipping this as
// a hard failure today would break `make test` on unrelated territory the
// moment it merged -- exactly the "gate broad enough to explode the whole
// document" working-agreements §5 warns against building. So: enforced
// (t.Error, blocking) on every section NOT in this list, which is every
// section this task actually touched or spot-checked clean; advisory
// (t.Log, visible but non-blocking) elsewhere, so nothing is hidden.
//
// GOVERNANCE (team-lead ruling, 2026-09-02 merge review): this list is a
// hardcoded, explicit constant on purpose -- never compute it, never
// auto-derive it from the current failure set, never widen it to silence a
// NEW failure. It may only SHRINK: an entry comes out once its invariant's
// Enforced by prose gets its missing symbols backtick-quoted (a
// documentation fix, not a test fix), at which point this check starts
// holding its pins to the real mechanism. If a section not already in this
// list starts failing after a legitimate doc edit, fix the citation or the
// pin -- do not add the section here to make it go away.
//
// C-2 (W3 adversarial review of the gates): that governance was PROSE. The
// reviewer added "I-59" and "I-8" -- two of the ten invariants that were
// blocking at the time -- and the core package stayed green, because
// nothing asserted the list's contents. W3-citations then emptied it by
// fixing the citations themselves, so the machine-enforced form of "may
// only shrink" is now simply "must stay empty", which
// TestCitationStyleGapListStaysClosed below asserts. Adding an entry is red.
var citationStyleGapInvariants = map[string]bool{}

// TestCitationStyleGapListStaysClosed is the lock on the list above.
func TestCitationStyleGapListStaysClosed(t *testing.T) {
	var entries []string
	for section := range citationStyleGapInvariants {
		entries = append(entries, section)
	}
	sort.Strings(entries)
	assert.Emptyf(t, entries,
		"citationStyleGapInvariants is not empty: %v.\n\n"+
			"This list downgrades a pin-vs-mechanism mismatch from a failure to a log line, and it was emptied when every "+
			"invariant's Enforced by was made to name its exported mechanism (W3-citations). It may only shrink, which at zero "+
			"means it may not grow: an entry here silently un-gates one of the pins this file exists to hold. If a section's "+
			"citation is genuinely unresolvable, register it in unresolvableEnforcedCitations instead -- that list is checked, "+
			"reported, and equally closed to silent growth", entries)
}

// dbOnlyMechanism registers an invariant whose mechanism has no Go face at
// all: a trigger, a constraint, a GRANT, a partition -- something Postgres
// enforces, with no function in this repository a pin could be held to.
//
// C-2, second half, as refined in round 2 (team-lead): the register used to
// carry a prose reason and nothing else, which made it a place to put any
// section whose citations happened not to resolve. It now has to say WHERE
// the mechanism is (a migration in this repo) and WHAT it is called there
// (the trigger / function / constraint / column name), and the gate checks
// the file exists and names those objects. A section that has a Go face may
// not be registered at all -- naming the exported entry point the mechanism
// is reached through is the fix for those (see I-2, I-9, I-18, I-40, I-50,
// I-51, I-61, which left this register when their citations were corrected).
//
// The skip this register authorizes is narrow: the section's PINS are not
// held to an exported symbol, because there is none. It never authorizes a
// section to cite nothing real -- TestInvariantsEnforcedCitationsResolve
// applies to registered sections exactly as it does to any other.
type dbOnlyMechanism struct {
	migration string     // repo-relative path of the migration that declares it
	objects   []dbObject // the declarations that must be in that file
	reason    string
}

// dbObject is one registered declaration, as a kind and a name rather than
// as a string to search for.
//
// F-3 (2026-09-03 independent review): the check used to be
// strings.Contains over the whole migration file. One entry read
// "idempotency_key TEXT UNIQUE NOT NULL", of which the baseline has five
// copies -- deleting four of them still matched, and the reviewer measured
// that it took deleting the last one to turn anything red. A whole-file
// substring cannot tell a declaration from a comment, from a DROP, or from
// four other tables' copies of the same phrase.
//
// Naming the kind makes the check parse for a declaration instead. It also
// makes the register say something a reader can check: "trigger" and
// "function" and "unique column" are claims with shapes, where a bare
// string is a claim about text.
type dbObject struct {
	kind string // one of the kinds objectIsDeclared knows
	name string // the object's name; "table.column" for the column kinds
	// detail qualifies some kinds: the type for columnType, the object a
	// privilege is revoked on for privilege.
	detail string
}

// Kinds objectIsDeclared understands. An unknown kind fails rather than
// passing vacuously -- a register entry nobody can verify is the thing this
// whole file exists to prevent.
const (
	kindTable            = "table"
	kindPartitionedTable = "partitioned_table"
	kindPartition        = "partition"
	kindFunction         = "function"
	kindTrigger          = "trigger"
	kindIndex            = "index"
	kindConstraint       = "constraint"
	kindColumn           = "column"
	kindUniqueColumn     = "unique_column"
	kindNullableColumn   = "nullable_column"
	kindColumnType       = "column_type"
	kindRole             = "role"
	kindPrivilege        = "privilege"
)

var unresolvableEnforcedCitations = map[string]dbOnlyMechanism{
	"I-3": {
		migration: "postgres/sql/migrations/001_baseline.up.sql",
		objects: []dbObject{
			{kind: kindUniqueColumn, name: "journals.idempotency_key"},
			{kind: kindUniqueColumn, name: "reservations.idempotency_key"},
			{kind: kindConstraint, name: "uq_bookings_idempotency"},
			{kind: kindIndex, name: "uq_ingest_dead_letters_idempotency_key"},
		},
		reason: "UNIQUE constraints on every mutation table's idempotency_key",
	},
	"I-6": {
		migration: "postgres/sql/migrations/001_baseline.up.sql",
		objects: []dbObject{
			{kind: kindColumnType, name: "journal_entries.amount", detail: "NUMERIC(30,18)"},
			{kind: kindColumnType, name: "journals.total_debit", detail: "NUMERIC(30,18)"},
		},
		reason: "the column type itself; the Go half is a type choice (decimal.Decimal), not a function",
	},
	"I-7": {
		migration: "postgres/sql/migrations/001_baseline.up.sql",
		objects: []dbObject{
			{kind: kindNullableColumn, name: "journals.reversal_of"},
			{kind: kindNullableColumn, name: "bookings.journal_id"},
		},
		reason: "NOT NULL defaults and the four nullable FK exceptions, declared in the schema",
	},
	"I-12": {
		migration: "postgres/sql/migrations/001_baseline.up.sql",
		objects: []dbObject{
			{kind: kindFunction, name: "check_journal_currency_balance"},
			{kind: kindTrigger, name: "trg_check_journal_currency_balance"},
		},
		reason: "conservation is I-1 + I-2; its DB-side enforcement is the deferred balance trigger, which holds even for writes that never went through this library",
	},
	"I-13": {
		migration: "postgres/sql/migrations/001_baseline.up.sql",
		objects: []dbObject{
			{kind: kindPartitionedTable, name: "journal_entries"},
			{kind: kindPartition, name: "journal_entries_default"},
		},
		reason: "partition declaration plus the catch-all partition",
	},
	"I-18": {
		migration: "postgres/sql/migrations/001_baseline.up.sql",
		objects: []dbObject{
			{kind: kindIndex, name: "uq_journals_uid"},
			{kind: kindIndex, name: "uq_bookings_uid"},
			{kind: kindIndex, name: "uq_currencies_uid"},
		},
		reason: "external identity is the uid column and its unique index; the adapter-side conversion (uidToPG/pgToUID) is unexported by design, since nothing outside the store may hold an internal id",
	},
	"I-22": {
		migration: "postgres/sql/migrations/001_baseline.up.sql",
		objects: []dbObject{
			{kind: kindRole, name: "ledger_app"},
			{kind: kindPrivilege, name: "PUBLIC", detail: "SCHEMA public"},
		},
		reason: "role creation and the GRANT set that withholds DDL from ledger_app",
	},
	"I-24": {
		migration: "postgres/sql/migrations/001_baseline.up.sql",
		objects: []dbObject{
			{kind: kindFunction, name: "check_journal_currency_balance"},
			{kind: kindTrigger, name: "trg_check_journal_currency_balance"},
		},
		reason: "the deferred constraint trigger that balances every journal inside the DB",
	},
	"I-25": {
		migration: "postgres/sql/migrations/001_baseline.up.sql",
		objects: []dbObject{
			{kind: kindFunction, name: "ledger_classifications_guard"},
			{kind: kindFunction, name: "ledger_reservations_guard"},
			{kind: kindFunction, name: "ledger_block_mutation"},
		},
		reason: "per-table guard trigger functions on the balance-computation config tables",
	},
	"I-35": {
		migration: "postgres/sql/migrations/007_role_hardening_and_partition_security_definer.up.sql",
		objects: []dbObject{
			{kind: kindFunction, name: "ledger_create_monthly_partition", detail: "SECURITY DEFINER"},
			{kind: kindFunction, name: "ledger_rebalance_default_partition", detail: "SECURITY DEFINER"},
		},
		reason: "partition maintenance runs as the definer, so the serving credential needs no DDL",
	},
	"I-36": {
		migration: "postgres/sql/migrations/007_role_hardening_and_partition_security_definer.up.sql",
		objects: []dbObject{
			{kind: kindPrivilege, name: "ledger_ro", detail: "public.webhook_subscribers"},
		},
		reason: "a column-level GRANT that withholds the webhook secret from ledger_ro",
	},
	"I-57": {
		migration: "postgres/sql/migrations/019_ownership_resweep.up.sql",
		objects: []dbObject{
			{kind: kindFunction, name: "ledger_resweep_ownership"},
		},
		reason: "the ownership sweep is a SQL function migrations call at their end",
	},
	"I-58": {
		migration: "postgres/sql/migrations/020_audit_trail_integrity_and_coverage.up.sql",
		objects: []dbObject{
			{kind: kindFunction, name: "ledger_log_config_table_change", detail: "SECURITY DEFINER"},
		},
		reason: "the audit trigger and its SECURITY DEFINER writer, attached by a catalogue-derived DO loop",
	},
}

// dbOnlyRegisterSize is the register's size when F-3 locked it. The sister
// list (citationStyleGapInvariants) has been at zero and locked since C-2;
// this one had nothing asserting its contents at all, and the reviewer
// showed that two lines of doc edit plus one entry here un-gates any
// invariant's pins entirely, with core still green.
//
// The rule is "may only shrink", and a constant is how that is said to a
// machine: removing an entry means editing this number down, adding one
// means editing it up in a diff a reviewer reads. The register exists for
// mechanisms with no Go face, which is a property of the schema, not of
// anybody's schedule -- it should be shrinking as citations improve.
const dbOnlyRegisterSize = 13

// checkedSectionFloor is how many invariant sections
// TestInvariantsPinsReferenceEnforcedSymbols actually holds to their
// mechanism. See the require at the end of that test for why it is a
// number rather than a comparison against the register's size.
const checkedSectionFloor = 50

// TestDbOnlyMechanismsExistWhereRegistered checks the register's own
// claims: the migration is there, and it DECLARES the objects the entry
// names -- parsed as declarations, not searched for as text (F-3).
//
// Without this, the register is a list of assertions nobody verifies -- the
// same shape as an Enforced by citation pointing at a symbol that does not
// exist, which is what round 2 is about.
func TestDbOnlyMechanismsExistWhereRegistered(t *testing.T) {
	for _, section := range sortedKeys(unresolvableEnforcedCitations) {
		entry := unresolvableEnforcedCitations[section]
		require.NotEmptyf(t, entry.migration, "%s: a db-only mechanism must say which migration declares it", section)
		require.NotEmptyf(t, entry.objects, "%s: a db-only mechanism must name the objects it declares", section)

		body, err := os.ReadFile(filepath.Join("..", entry.migration))
		require.NoErrorf(t, err, "%s registers %s as the migration declaring its mechanism, but that file does not exist", section, entry.migration)

		sql := string(body)
		for _, object := range entry.objects {
			ok, known := objectIsDeclared(sql, object)
			require.Truef(t, known,
				"%s registers %q with kind %q, which no rule in objectIsDeclared understands. An entry nobody can "+
					"verify is worse than no entry: it reads as coverage", section, object.name, object.kind)
			assert.Truef(t, ok,
				"%s registers %s %q as declared in %s, and no such declaration is there.\n\n"+
					"This is parsed, not searched: a mention in a comment, in a DROP, or in another table's copy of "+
					"the same phrase does not count. The check used to be strings.Contains over the whole file, and "+
					"one entry -- \"idempotency_key TEXT UNIQUE NOT NULL\", of which the baseline has five copies -- "+
					"still matched with four of the five deleted (F-3). If the object was renamed or moved to another "+
					"migration, update the entry",
				section, object.kind, object.name, entry.migration)
		}
	}
}

// TestDbOnlyMechanismRegisterOnlyShrinks is the lock F-3 found missing.
//
// citationStyleGapInvariants has had TestCitationStyleGapListStaysClosed on
// it since C-2, and that test's own message claims this register is
// "equally closed to silent growth". It was not: nothing asserted its size
// or its contents, and the reviewer added one entry -- plus two lines of
// doc edit -- to detach an invariant from its pins entirely, with the core
// package still green.
func TestDbOnlyMechanismRegisterOnlyShrinks(t *testing.T) {
	assert.Lenf(t, unresolvableEnforcedCitations, dbOnlyRegisterSize,
		"the db-only mechanism register has %d entries, and dbOnlyRegisterSize says %d.\n\n"+
			"Registering a section here stops TestInvariantsPinsReferenceEnforcedSymbols from holding ANY of its pins "+
			"to its mechanism, so the list may only shrink -- and shrinking means editing this constant down in the "+
			"same commit, which is the point: the change becomes a line a reviewer reads instead of a silence. If a "+
			"new section genuinely has no Go face, say so here and in the commit message; if it has one, name the "+
			"exported entry point in **Enforced by** instead, which is what I-2, I-9, I-18, I-40, I-50, I-51 and I-61 "+
			"all did to leave this list. Registered: %v",
		len(unresolvableEnforcedCitations), dbOnlyRegisterSize, sortedKeys(unresolvableEnforcedCitations))
}

// objectIsDeclared reports whether sql declares object, and whether the
// object's kind is one this function knows how to look for. An unknown kind
// returns known=false so it fails loudly rather than passing vacuously.
func objectIsDeclared(sql string, object dbObject) (found, known bool) {
	name := regexp.QuoteMeta(object.name)
	switch object.kind {
	case kindTable:
		return reFind(sql, `(?im)^\s*CREATE\s+TABLE\s+(IF\s+NOT\s+EXISTS\s+)?`+name+`\s*\(`), true
	case kindPartitionedTable:
		return declaresPartitionedTable(sql, object.name), true
	case kindPartition:
		return reFind(sql, `(?is)CREATE\s+TABLE\s+(IF\s+NOT\s+EXISTS\s+)?`+name+`\s+PARTITION\s+OF\b`), true
	case kindFunction:
		return declaresFunction(sql, object.name, object.detail), true
	case kindTrigger:
		return reFind(sql, `(?is)CREATE\s+(CONSTRAINT\s+)?TRIGGER\s+`+name+`\b`), true
	case kindIndex:
		return reFind(sql, `(?is)CREATE\s+(UNIQUE\s+)?INDEX\s+(CONCURRENTLY\s+)?(IF\s+NOT\s+EXISTS\s+)?`+name+`\b`), true
	case kindConstraint:
		return reFind(sql, `(?is)CONSTRAINT\s+`+name+`\b`), true
	case kindColumn, kindUniqueColumn, kindNullableColumn, kindColumnType:
		decl, ok := columnDeclaration(sql, object.name)
		if !ok {
			return false, true
		}
		switch object.kind {
		case kindUniqueColumn:
			return regexp.MustCompile(`(?i)\bUNIQUE\b`).MatchString(decl), true
		case kindNullableColumn:
			return !regexp.MustCompile(`(?i)\bNOT\s+NULL\b`).MatchString(decl), true
		case kindColumnType:
			return strings.Contains(strings.ReplaceAll(strings.ToUpper(decl), " ", ""),
				strings.ReplaceAll(strings.ToUpper(object.detail), " ", "")), true
		}
		return true, true
	case kindRole:
		return reFind(sql, `(?is)CREATE\s+ROLE\s+`+name+`\b`), true
	case kindPrivilege:
		return revokesFrom(sql, object.name, object.detail), true
	}
	return false, false
}

func reFind(sql, pattern string) bool {
	return regexp.MustCompile(pattern).MatchString(sql)
}

// declaresFunction finds CREATE [OR REPLACE] FUNCTION name(...) and, when
// detail is set (e.g. "SECURITY DEFINER"), requires it inside that
// function's own body rather than anywhere in the file.
func declaresFunction(sql, name, detail string) bool {
	head := regexp.MustCompile(`(?is)CREATE\s+(OR\s+REPLACE\s+)?FUNCTION\s+` + regexp.QuoteMeta(name) + `\s*\(`)
	loc := head.FindStringIndex(sql)
	if loc == nil {
		return false
	}
	if detail == "" {
		return true
	}
	body := sql[loc[1]:]
	// The body ends at the dollar-quoted terminator that closes it; taking
	// everything up to the next CREATE is close enough and never reaches
	// into a neighbouring declaration.
	if next := regexp.MustCompile(`(?is)\n\s*CREATE\s`).FindStringIndex(body); next != nil {
		body = body[:next[0]]
	}
	return strings.Contains(strings.ToUpper(body), strings.ToUpper(detail))
}

// declaresPartitionedTable requires "PARTITION BY" to be part of THIS
// table's CREATE statement, not merely present somewhere in the file.
func declaresPartitionedTable(sql, table string) bool {
	block, ok := createTableBlock(sql, table)
	if !ok {
		return false
	}
	return regexp.MustCompile(`(?is)PARTITION\s+BY\b`).MatchString(block)
}

// createTableBlock returns the text of one CREATE TABLE statement, from the
// table name to the semicolon that ends it.
func createTableBlock(sql, table string) (string, bool) {
	head := regexp.MustCompile(`(?im)^\s*CREATE\s+TABLE\s+(IF\s+NOT\s+EXISTS\s+)?` + regexp.QuoteMeta(table) + `\s*[(\s]`)
	loc := head.FindStringIndex(sql)
	if loc == nil {
		return "", false
	}
	rest := sql[loc[0]:]
	if end := strings.Index(rest, ";"); end >= 0 {
		rest = rest[:end]
	}
	return rest, true
}

// columnDeclaration returns the one line declaring "table.column" inside
// that table's CREATE TABLE statement. Scoping to the statement is what
// makes this a declaration check: five tables declare idempotency_key, and
// a file-wide search cannot tell which one it found.
func columnDeclaration(sql, qualified string) (string, bool) {
	table, column, ok := strings.Cut(qualified, ".")
	if !ok {
		return "", false
	}
	block, ok := createTableBlock(sql, table)
	if !ok {
		return "", false
	}
	line := regexp.MustCompile(`(?im)^\s*` + regexp.QuoteMeta(column) + `\s+[A-Za-z].*$`)
	m := line.FindString(block)
	if m == "" {
		return "", false
	}
	return m, true
}

// revokesFrom requires one REVOKE statement that names both the grantee and
// the object -- statement-scoped, so a REVOKE on some other table plus a
// mention of the role elsewhere does not add up to coverage.
func revokesFrom(sql, grantee, object string) bool {
	for _, stmt := range strings.Split(sql, ";") {
		up := strings.ToUpper(stmt)
		if !strings.Contains(up, "REVOKE") {
			continue
		}
		if strings.Contains(up, strings.ToUpper(object)) && strings.Contains(up, strings.ToUpper(grantee)) {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]dbOnlyMechanism) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestInvariantsPinsReferenceEnforcedSymbols(t *testing.T) {
	raw, err := os.ReadFile("../docs/INVARIANTS.md")
	if err != nil {
		t.Fatalf("read INVARIANTS.md: %v", err)
	}

	symbolIdx := buildDeclaredSymbolIndex(t)
	testBodies := buildTestFuncBodyIndex(t)

	var failures, advisories []string
	checked, skipped := 0, map[string]bool{}
	for _, sec := range splitInvariantSections(string(raw)) {
		enforced := blockBetween(sec.body, "**Enforced by**")
		leaves := enforcedLeafNames(enforced, symbolIdx)
		if len(leaves) == 0 && selfGatedSection(enforced, sec.body, testBodies) {
			// A self-enforcing gate: the mechanism IS the test, and the doc
			// names the file it lives in (I-50's sign-authority gate, I-61's
			// emission-coverage gate). "Does the pin touch the mechanism"
			// is satisfied by identity here, and there is nothing else to
			// hold it to -- but this is NOT the silent skip: the pins must
			// actually live in the file the Enforced by names.
			checked++
			continue
		}
		if len(leaves) == 0 {
			// Nothing in this section's Enforced by resolves to a repo
			// symbol. Registered, not silent (C-2).
			skipped[sec.number] = true
			if _, known := unresolvableEnforcedCitations[sec.number]; !known {
				t.Errorf("%s's **Enforced by** names no exported Go symbol this repository declares, so none of its pins can be held to a "+
					"mechanism -- and this check would otherwise skip it in silence.\n\n"+
					"Fix the citation (name the EXPORTED entry point the mechanism is reached through -- that is what I-49, I-53 and, in "+
					"round 2, I-2 / I-9 / I-18 / I-40 / I-50 / I-51 / I-61 needed), or -- only if the mechanism genuinely has no Go face at "+
					"all -- register %q in unresolvableEnforcedCitations with the migration that declares it and the object names it "+
					"declares there, which TestDbOnlyMechanismsExistWhereRegistered then verifies.", sec.number, sec.number)
			}
			continue
		}

		pinned := blockBetween(sec.body, "**Pinned by**")
		if pinned == "" {
			continue // F-m2's other test already flags a missing Pinned by section
		}
		checked++

		bucket := &failures
		if citationStyleGapInvariants[sec.number] {
			bucket = &advisories
		}

		gateFiles := enforcedGateFiles(enforced)
		dbObjects := enforcedDBObjects(enforced)
		for _, m := range pinReference.FindAllStringSubmatch(pinned, -1) {
			pkg, fn := m[1], m[2]
			checkPinTouchesLeaves(t, sec.number, pkg, fn, leaves, gateFiles, dbObjects, testBodies, bucket)
		}
		for _, m := range bareReference.FindAllStringSubmatch(pinned, -1) {
			checkPinTouchesLeaves(t, sec.number, "", m[1], leaves, gateFiles, dbObjects, testBodies, bucket)
		}
	}

	sort.Strings(advisories)
	for _, a := range advisories {
		t.Log("advisory (citation-style gap, not blocking): " + a)
	}

	sort.Strings(failures)
	for _, f := range failures {
		t.Error(f)
	}

	// A registered section that grows a Go face must leave the register, or
	// the register becomes a permanent carve-out nobody rereads. Round 2
	// makes that rule explicit in both directions: the register is for
	// mechanisms with NO Go face, so a section whose Enforced by now names an
	// exported symbol belongs on the checked side.
	var stale []string
	for section, entry := range unresolvableEnforcedCitations {
		if !skipped[section] {
			stale = append(stale, section+" ("+entry.reason+")")
		}
	}
	sort.Strings(stale)
	assert.Empty(t, stale,
		"section(s) registered as db-only now name an exported Go symbol -- a section with a Go face may not be registered; "+
			"delete their unresolvableEnforcedCitations entries so this check starts holding their pins: %v", stale)

	// Fail-closed sanity: if the section splitter or the symbol index ever
	// regresses, every section lands in the skip path and this check silently
	// verifies nothing.
	//
	// F-3 (2026-09-03 independent review): this bound used to be `checked >
	// len(unresolvableEnforcedCitations)`, which with thirteen registered
	// sections meant the register had to reach thirty-two before the sanity
	// check noticed anything -- far enough away to be no bound at all. The
	// floor is now stated against the document: 52 sections are checked today,
	// so the floor is 50. Adding invariants raises the real
	// number and never trips it; a splitter or index regression drops it off
	// a cliff and does.
	require.GreaterOrEqualf(t, checked, checkedSectionFloor,
		"only %d invariant section(s) were actually checked against their Enforced-by symbols, against a floor of %d "+
			"(%d registered as unresolvable) -- a check that inspects almost nothing reads as a pass",
		checked, checkedSectionFloor, len(unresolvableEnforcedCitations))
}

// checkPinTouchesLeaves looks up every declared body for a (pkg, fn) pin
// citation and appends a failure only if at least one body was found (a pin
// this file can't locate is silently skipped, not failed -- see the calling
// test's doc comment) AND none of the found bodies reference any leaf name.
func checkPinTouchesLeaves(t *testing.T, sectionNum, pkg, fn string, leaves map[string]bool, gateFiles, dbObjects []string, testBodies map[string][]testFuncBody, failures *[]string) {
	t.Helper()
	bodies, ok := testBodies[fn]
	if !ok {
		return
	}
	var candidates []testFuncBody
	for _, b := range bodies {
		if pkg == "" || pkgDirMatches(b.pkgDir, pkg) {
			candidates = append(candidates, b)
		}
	}
	if len(candidates) == 0 {
		return
	}
	for _, b := range candidates {
		for leaf := range leaves {
			if b.usedNames[leaf] {
				return // at least one candidate body touches at least one enforced symbol
			}
		}
		// A pin declared in a gate file the Enforced by names IS the
		// mechanism (I-50's three sign-authority gate tests, I-61's two
		// emission-coverage ones). Holding it to a separate symbol would
		// mean inventing one; requiring it to live in the named file is
		// what keeps that from being a loophole.
		for _, gate := range gateFiles {
			if b.file == gate {
				return
			}
		}
		// A pin that drives a DB-side mechanism names it in a query string.
		for _, object := range dbObjects {
			if strings.Contains(b.usedStrings, object) {
				return
			}
		}
	}
	leafList := make([]string, 0, len(leaves))
	for l := range leaves {
		leafList = append(leafList, l)
	}
	sort.Strings(leafList)
	*failures = append(*failures, sectionNum+"'s pin "+fn+" never references any of its Enforced by symbols ("+
		strings.Join(leafList, ", ")+") -- it may not actually exercise this invariant's mechanism")
}

// --- F-1: a DB-side claim needs a pin that talks to the DB directly ---

// dbMechanismClaim matches an **Enforced by** bullet that names Postgres
// itself as the enforcer: a constraint, an index, a trigger, a grant, a
// partition. These are the claims a pin written against the Go API cannot
// substantiate, because the Go API's own guards stand in front of them.
//
// F-1 (2026-09-03 independent review) is the general form of that: dropping
// `UNIQUE` from journals.idempotency_key left all fifteen of I-3's pins
// green, because every one of them called PostJournal, where an advisory
// lock and a pre-read settle a duplicate before Postgres ever sees it. The
// constraint exists for the writers that do not hold that lock -- a second
// replica, a leaked ledger_app credential, a replayed WAL -- and only a pin
// that writes the way those writers write can tell whether it is still
// there.
var dbMechanismClaim = regexp.MustCompile(`(?i)\b(unique (constraint|index)|check constraint|foreign key|not null|` +
	`trigger|grant|revoke|deferrable|partition|column-level)\b`)

// directSQLStatement matches a raw SQL statement among a pin's string
// literals -- the signature of a test that reaches the database without
// going through this library's write path.
//
// Catalogue reads (`FROM pg_index`, `information_schema`) count: asking
// Postgres what it built is a direct interrogation of the mechanism, and it
// is the only way to see an index left INVALID by a failed concurrent
// build, which no INSERT can distinguish from a missing one.
var directSQLStatement = regexp.MustCompile(`(?is)(insert\s+into|update\s+[a-z_"]|delete\s+from|alter\s+table|` +
	`\bgrant\s+|\brevoke\s+|create\s+(table|index|trigger|temp)|from\s+(pg_|information_schema)|` +
	`has_table_privilege|has_column_privilege|set\s+role)`)

// dbClaimsWithoutDirectPin registers sections whose Enforced by names a
// Postgres-side mechanism but whose pins legitimately do not issue SQL.
//
// This list is closed the way unresolvableEnforcedCitations is closed
// (TestDbOnlyMechanismRegisterOnlyShrinks): the count is snapshotted, so an
// entry cannot be added without editing a number a reviewer sees. That is
// the shape C-2 established and F-3 found missing on the sister list.
// It is empty, and W5 emptied it: the five sections that failed when this
// gate was written (I-1, I-6, I-11, I-12, I-16) each turned out to be
// fixable at the mechanism rather than at the register -- three by adding a
// direct-SQL pin that did not exist, two by citing one that did but was
// filed under a neighbouring invariant. Empty is therefore the honest
// starting state, and "may only shrink" at zero means "may not grow".
var dbClaimsWithoutDirectPin = map[string]string{}

func TestDatabaseSideClaimsHaveADirectSQLPin(t *testing.T) {
	raw, err := os.ReadFile("../docs/INVARIANTS.md")
	require.NoError(t, err, "read INVARIANTS.md")

	testBodies := buildTestFuncBodyIndex(t)

	checked := 0
	var claimed []string
	for _, sec := range splitInvariantSections(string(raw)) {
		enforced := blockBetween(sec.body, "**Enforced by**")
		if !dbMechanismClaim.MatchString(enforced) {
			continue
		}
		claimed = append(claimed, sec.number)
		pinned := blockBetween(sec.body, "**Pinned by**")
		if pinned == "" {
			continue // a missing Pinned by is another test's failure
		}

		names := map[string]bool{}
		for _, m := range pinReference.FindAllStringSubmatch(pinned, -1) {
			names[m[2]] = true
		}
		for _, m := range bareReference.FindAllStringSubmatch(pinned, -1) {
			names[m[1]] = true
		}

		direct := ""
		for name := range names {
			for _, body := range testBodies[name] {
				if directSQLStatement.MatchString(body.usedStrings) {
					direct = name
					break
				}
			}
			if direct != "" {
				break
			}
		}
		if _, exempt := dbClaimsWithoutDirectPin[sec.number]; exempt {
			assert.Emptyf(t, direct,
				"%s is registered in dbClaimsWithoutDirectPin, but its pin %s does issue direct SQL. "+
					"The register is for sections that cannot have one; delete the entry", sec.number, direct)
			continue
		}
		checked++
		assert.NotEmptyf(t, direct,
			"%s's **Enforced by** claims a Postgres-side mechanism (constraint / index / trigger / grant / partition), "+
				"but not one of its pins issues SQL of its own -- every one of them goes through this library's write "+
				"path.\n\nThat is the F-1 shape: the Go path holds an advisory lock and pre-reads, so it returns the right "+
				"answer whether or not the database constraint is still there. Dropping UNIQUE from "+
				"journals.idempotency_key left all fifteen of I-3's pins green. Add a pin that writes the way an "+
				"unlocked writer writes -- a direct INSERT asserting 23505 (see "+
				"postgres.TestJournalIdempotencyKey_RejectsDirectSQLDuplicate), a catalogue read, or a permission probe "+
				"-- or register %s in dbClaimsWithoutDirectPin with the reason it cannot have one.\n\nPins seen: %v",
			sec.number, sec.number, sortedNames(names))
	}

	sort.Strings(claimed)
	require.GreaterOrEqualf(t, len(claimed), 20,
		"only %d invariant section(s) were read as claiming a Postgres-side mechanism (%v). The doc has around twenty; "+
			"a matcher that finds almost none inspects nothing and reads as a pass", len(claimed), claimed)
	require.Greaterf(t, checked, len(dbClaimsWithoutDirectPin),
		"%d section(s) actually checked against %d registered exemptions -- the register has swallowed the check", checked, len(dbClaimsWithoutDirectPin))
	assert.Emptyf(t, dbClaimsWithoutDirectPin,
		"dbClaimsWithoutDirectPin is not empty: %v.\n\nIt was emptied when this gate was written, by fixing the five "+
			"sections that failed rather than registering them, so at zero \"may only shrink\" means \"may not grow\". "+
			"An entry here un-gates one DB-side claim; F-3 is what happens to a register nobody locks", dbClaimsWithoutDirectPin)
}

func sortedNames(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
