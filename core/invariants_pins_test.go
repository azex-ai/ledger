package core_test

import (
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
func TestInvariantsDocPinsAllExist(t *testing.T) {
	raw, err := os.ReadFile("../docs/INVARIANTS.md")
	if err != nil {
		t.Fatalf("read INVARIANTS.md: %v", err)
	}

	// funcName -> set of package directories declaring it.
	declared := map[string]map[string]bool{}
	err = filepath.WalkDir("..", func(path string, d os.DirEntry, err error) error {
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
