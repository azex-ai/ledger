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
// holding its pins to the real mechanism. Rewriting the citation style for
// these 32 invariants is tracked as a Wave 3 item, not silently absorbed
// here. If a section not already in this list starts failing after a
// legitimate doc edit, fix the citation or the pin -- do not add the
// section here to make it go away.
var citationStyleGapInvariants = map[string]bool{
	"I-34": true, "I-37": true,
	"I-38": true, "I-39": true, "I-41": true, "I-42": true, "I-43": true,
	"I-44": true, "I-45": true, "I-46": true, "I-49": true, "I-52": true,
	"I-53": true, "I-54": true,
}

func TestInvariantsPinsReferenceEnforcedSymbols(t *testing.T) {
	raw, err := os.ReadFile("../docs/INVARIANTS.md")
	if err != nil {
		t.Fatalf("read INVARIANTS.md: %v", err)
	}

	symbolIdx := buildDeclaredSymbolIndex(t)
	testBodies := buildTestFuncBodyIndex(t)

	var failures, advisories []string
	for _, sec := range splitInvariantSections(string(raw)) {
		enforced := blockBetween(sec.body, "**Enforced by**")
		leaves := enforcedLeafNames(enforced, symbolIdx)
		if len(leaves) == 0 {
			continue // nothing in this section's Enforced by resolves to a repo symbol -- skip, see doc comment
		}

		pinned := blockBetween(sec.body, "**Pinned by**")
		if pinned == "" {
			continue // F-m2's other test already flags a missing Pinned by section
		}

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
