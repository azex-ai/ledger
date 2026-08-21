package core_test

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// invariantHeading matches an invariant section heading in docs/INVARIANTS.md.
var invariantHeading = regexp.MustCompile(`(?m)^## I-(\d+):`)

// TestInvariantsDocIsOrderedAndGapless is a mechanical pin on docs/INVARIANTS.md,
// the canonical contract this ledger claims to uphold. It asserts the I-N
// sections appear in ascending order with no duplicates and no gaps.
//
// Why this exists: the 2026-08-21 integrity wave allocated invariant numbers up
// front and let each branch append its own section, so git placed new sections
// wherever the surrounding diff context happened to match. Twice in one day a
// merge produced an order like 22, 27, 28, 23, 24 -- harmless to the code and
// invisible to every other test, but this file is meant to be read top to
// bottom by an auditor. A duplicate number is worse: two branches would each
// believe they own it, and one would silently document a rule nothing pins.
//
// Ordering is trivially checkable, so it should not depend on whoever runs the
// merge noticing (working-agreements §5).
func TestInvariantsDocIsOrderedAndGapless(t *testing.T) {
	raw, err := os.ReadFile("../docs/INVARIANTS.md")
	if err != nil {
		t.Fatalf("read INVARIANTS.md: %v", err)
	}

	matches := invariantHeading.FindAllStringSubmatch(string(raw), -1)
	if len(matches) == 0 {
		t.Fatal("no I-N headings found -- the regexp or the doc's heading style changed")
	}

	seen := make(map[int]bool, len(matches))
	prev := 0
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("unparseable invariant number %q", m[1])
		}
		if seen[n] {
			t.Errorf("I-%d appears more than once -- two branches think they own that number", n)
		}
		seen[n] = true
		if n != prev+1 {
			t.Errorf("I-%d follows I-%d: sections must be in ascending order with no gaps "+
				"(a merge most likely inserted a section where the diff context matched, "+
				"not where the number belongs)", n, prev)
		}
		prev = n
	}
}
