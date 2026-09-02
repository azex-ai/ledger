package ledger

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestCLAUDEMdFileLayoutPathsExist pins I-N22: CLAUDE.md's "File Layout
// Quick Reference" table is the first map an agent or new contributor reads.
// Before this fix it pointed at `deploy/helm/ledger/`, deleted since 30bd872
// -- a reader concludes the checkout is incomplete rather than that the doc
// is stale. Every path cell in that table (a file, or a directory ending in
// `/`) must exist on disk.
func TestCLAUDEMdFileLayoutPathsExist(t *testing.T) {
	raw, err := os.ReadFile("CLAUDE.md")
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}
	content := string(raw)

	start := strings.Index(content, "## File Layout Quick Reference")
	if start == -1 {
		t.Fatal(`CLAUDE.md has no "## File Layout Quick Reference" section -- update this test if it was renamed`)
	}
	// The table ends at the next "## " heading.
	rest := content[start+len("## File Layout Quick Reference"):]
	end := strings.Index(rest, "\n## ")
	if end != -1 {
		rest = rest[:end]
	}

	// Table rows look like: | `path/to/thing` | description |
	rowPattern := regexp.MustCompile("(?m)^\\|\\s*`([^`]+)`\\s*\\|")
	matches := rowPattern.FindAllStringSubmatch(rest, -1)
	if len(matches) == 0 {
		t.Fatal("found the File Layout section but no `path` cells in it -- table format changed?")
	}

	var missing []string
	for _, m := range matches {
		path := m[1]
		if _, err := os.Stat(path); err != nil {
			missing = append(missing, path)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("CLAUDE.md File Layout table references paths that do not exist on disk: %v", missing)
	}
}
