package core_test

import (
	"os"
	"regexp"
	"strconv"
	"strings"
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

// sectionBlockHeading matches the two bold sub-headings every invariant section
// uses: what implements the rule, and what pins it.
var sectionBlockHeading = regexp.MustCompile(`^\*\*(Enforced by|Pinned by)\*\*`)

// TestInvariantsDocHasNoOrphanedBlocks pins that no invariant section contains
// two of the same sub-heading.
//
// Why this exists: this wave found two separate orphaned fragments in this
// file, both inside I-22's section. One was a complete headless copy of I-22's
// own body, left behind when the real I-22 was written into its allocated slot
// above -- along with a note still claiming "this document does not yet contain
// I-22" while it plainly did. The other was a partial copy of I-26's content.
// Both went unnoticed for as long as they existed, and both made the pin
// citations inside them appear to belong to whichever invariant they had
// drifted into -- which is how "I-30, on Merkle inclusion proofs, is pinned by
// a test about database role attributes" came to be sitting in a document
// people are meant to audit against.
//
// Twice in one file is a pattern, not an accident: sections get allocated a
// number, drafted somewhere else, and pasted in, and a paste that lands beside
// the placeholder instead of on top of it leaves both. The duplicate
// sub-heading is the cheap, reliable signal -- a section legitimately has one
// "Enforced by" and one "Pinned by", so a second of either means two bodies are
// sharing one heading.
func TestInvariantsDocHasNoOrphanedBlocks(t *testing.T) {
	raw, err := os.ReadFile("../docs/INVARIANTS.md")
	if err != nil {
		t.Fatalf("read INVARIANTS.md: %v", err)
	}

	section := "(preamble)"
	seen := map[string]int{}
	for i, line := range strings.Split(string(raw), "\n") {
		if m := invariantHeading.FindStringSubmatch(line); m != nil {
			section = "I-" + m[1]
			seen = map[string]int{}
			continue
		}
		m := sectionBlockHeading.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		seen[m[1]]++
		if seen[m[1]] > 1 {
			t.Errorf("INVARIANTS.md:%d: %s has a second %q block -- a section has one of each, "+
				"so this is almost certainly another section's body pasted in beside this one "+
				"rather than over its placeholder. Any pin cited below it will read as belonging "+
				"to %s.", i+1, section, m[1], section)
		}
	}
}
