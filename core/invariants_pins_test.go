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

var (
	// backtickToken matches one backtick-quoted token, whatever it is.
	backtickToken = regexp.MustCompile("`([^`]+)`")
	// fileCitation matches a path or bare filename with a known extension,
	// optionally with a `:line` suffix.
	fileCitation = regexp.MustCompile(`^[A-Za-z0-9_./-]+\.(?:go|sql|md|yml|yaml|ts|tsx|js|json|sh)(?::\d+)?$`)
	// bareIdentifier matches a single Go identifier.
	bareIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// citationAudit is what one Enforced by block claims, and whether those
// claims are true.
type citationAudit struct {
	resolved int      // citations pointing at something that exists
	broken   []string // Go-symbol citations in a repo package that resolve to nothing
}

func auditEnforcedCitations(block string, idx declaredSymbolIndex, pkgs, filesByPath, filesByBase map[string]bool) citationAudit {
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
		case bareIdentifier.MatchString(token) && idx.bare[token]:
			// A mechanism cited without its package (`ReverseJournal`,
			// `mergeWorkerConfig`). Counted when it resolves; never an
			// error when it does not, because most bare backticks in this
			// document are SQL keywords and column names.
			out.resolved++
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

	sections := splitInvariantSections(string(raw))
	require.NotEmpty(t, sections, "no invariant sections parsed -- the splitter regressed")

	for _, sec := range sections {
		enforced := blockBetween(sec.body, "**Enforced by**")
		audit := auditEnforcedCitations(enforced, idx, pkgs, filesByPath, filesByBase)

		for _, citation := range audit.broken {
			t.Errorf("%s's **Enforced by** cites %q, which this repository does not declare.\n\n"+
				"The package segment names a package that exists here, so this is a claim about OUR code -- and it resolves to nothing. "+
				"A citation naming a symbol that does not exist is worse than no citation: it reads as a mechanism, it is what the pin "+
				"check holds pins to, and it silently stops holding them to anything. Rename it to the symbol that exists (a rename is "+
				"the usual cause), or drop it.", sec.number, citation)
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
	pkgDir    string
	usedNames map[string]bool
}

func buildTestFuncBodyIndex(t *testing.T) map[string][]testFuncBody {
	t.Helper()
	out := map[string][]testFuncBody{}
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
			if !testFuncNamePattern.MatchString(fn.Name.Name) {
				continue
			}
			used := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch e := n.(type) {
				case *ast.SelectorExpr:
					used[e.Sel.Name] = true
				case *ast.Ident:
					used[e.Name] = true
				}
				return true
			})
			out[fn.Name.Name] = append(out[fn.Name.Name], testFuncBody{pkgDir: pkgDir, usedNames: used})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo for test function bodies: %v", err)
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

// unresolvableEnforcedCitations registers the invariants whose **Enforced
// by** block names no Go symbol this repository declares, with the reason.
// Their pins cannot be held to a mechanism by this check -- there is no
// symbol to hold them to -- so the section is skipped.
//
// C-2, second half: that skip used to be a bare `continue`. Nineteen
// sections took it, including (before this pass) I-49 and I-53, two of the
// Wave 1 money-path invariants -- and the output said nothing at all, so
// from the outside a skipped section and a checked one looked identical
// (working-agreements §3: not run is not passed). The register makes the set
// explicit and closed: a NEW unresolvable section is red until someone
// either fixes the citation (the I-49 / I-53 fix in this same commit: name
// the exported entry point the mechanism is reached through) or writes down
// why it cannot be fixed.
//
// The recurring honest reason: the mechanism is a DDL object -- a trigger, a
// constraint, a GRANT, a partition -- and the invariant is enforced by
// Postgres, not by a Go function a test can name.
//
// ⚠️ What an entry here does NOT do (round 2): it does not exempt the section
// from TestInvariantsEnforcedCitationsResolve. A registered section must
// still cite something that exists -- an unexported mechanism, a file -- and
// any Go-symbol citation it makes must still resolve. The register means
// "nothing here is an EXPORTED symbol a black-box pin can be held to", never
// "nothing here is checkable at all". Team-lead's mutation of stripping
// I-2's backticks was green precisely because those two meanings had been
// collapsed into one.
var unresolvableEnforcedCitations = map[string]string{
	"I-2":  "the mechanism is the journals.reversal_of FK plus SELECT ... FOR UPDATE; the two Go methods it names are cited bare, without a package qualifier",
	"I-3":  "UNIQUE constraints on five tables' idempotency_key columns",
	"I-6":  "column types (NUMERIC(30,18)) and a Go field type (decimal.Decimal), not functions",
	"I-7":  "three migrations' NOT NULL work",
	"I-9":  "a four-line helper cited by file and line rather than by symbol",
	"I-12": "derived: 'I-1 + I-2 together', with no mechanism of its own",
	"I-13": "partition DDL across three migrations",
	"I-18": "a migration's uid columns plus per-store conversion helpers cited by file",
	"I-22": "role GRANTs in 001_baseline",
	"I-24": "the check_journal_currency_balance() deferred constraint trigger",
	"I-25": "the per-table mutation guard trigger functions",
	"I-35": "two SECURITY DEFINER partition functions",
	"I-36": "a column-level GRANT/REVOKE pair on webhook_subscribers",
	"I-40": "cited as expressions inside methods ('the s.attestor != nil branch'), not as symbols",
	"I-50": "the mechanism IS a gate test file (postgres/sign_authority_gate_test.go) and its classification tables",
	"I-51": "unexported validators cited with their file paths; the exported entry points are the four reversal APIs, named in prose",
	"I-57": "ledger_resweep_ownership(), a SQL function",
	"I-58": "migration 020's catalogue-derived DO loop and its SECURITY DEFINER writers",
	"I-61": "the mechanism IS a gate test file (observability/emission_coverage_test.go)",
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
		if len(leaves) == 0 {
			// Nothing in this section's Enforced by resolves to a repo
			// symbol. Registered, not silent (C-2).
			skipped[sec.number] = true
			if _, known := unresolvableEnforcedCitations[sec.number]; !known {
				t.Errorf("%s's **Enforced by** names no Go symbol this repository declares, so none of its pins can be held to a mechanism -- "+
					"and this check would otherwise skip it in silence.\n\n"+
					"Fix the citation (name the EXPORTED entry point the mechanism is reached through -- that is what I-49 and I-53 needed), "+
					"or register %q in unresolvableEnforcedCitations with the reason it cannot be named.", sec.number, sec.number)
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

		for _, m := range pinReference.FindAllStringSubmatch(pinned, -1) {
			pkg, fn := m[1], m[2]
			checkPinTouchesLeaves(t, sec.number, pkg, fn, leaves, testBodies, bucket)
		}
		for _, m := range bareReference.FindAllStringSubmatch(pinned, -1) {
			checkPinTouchesLeaves(t, sec.number, "", m[1], leaves, testBodies, bucket)
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

	// A registered section that starts resolving must leave the register, or
	// the register becomes a permanent carve-out nobody rereads.
	var stale []string
	for section, reason := range unresolvableEnforcedCitations {
		if !skipped[section] {
			stale = append(stale, section+" ("+reason+")")
		}
	}
	sort.Strings(stale)
	assert.Empty(t, stale,
		"section(s) registered as having unresolvable Enforced-by citations now resolve to a repo symbol -- delete their unresolvableEnforcedCitations entries so this check starts holding their pins: %v", stale)

	// Fail-closed sanity: if the section splitter or the symbol index ever
	// regresses, every section lands in the skip path and this check silently
	// verifies nothing.
	require.Greater(t, checked, len(unresolvableEnforcedCitations),
		"only %d invariant section(s) were actually checked against their Enforced-by symbols, against %d registered as unresolvable -- "+
			"a check that inspects almost nothing reads as a pass", checked, len(unresolvableEnforcedCitations))
}

// checkPinTouchesLeaves looks up every declared body for a (pkg, fn) pin
// citation and appends a failure only if at least one body was found (a pin
// this file can't locate is silently skipped, not failed -- see the calling
// test's doc comment) AND none of the found bodies reference any leaf name.
func checkPinTouchesLeaves(t *testing.T, sectionNum, pkg, fn string, leaves map[string]bool, testBodies map[string][]testFuncBody, failures *[]string) {
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
	}
	leafList := make([]string, 0, len(leaves))
	for l := range leaves {
		leafList = append(leafList, l)
	}
	sort.Strings(leafList)
	*failures = append(*failures, sectionNum+"'s pin "+fn+" never references any of its Enforced by symbols ("+
		strings.Join(leafList, ", ")+") -- it may not actually exercise this invariant's mechanism")
}
