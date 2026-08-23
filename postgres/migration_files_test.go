package postgres_test

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var migrationFileName = regexp.MustCompile(`^(\d+)_([a-z0-9_]+)\.(up|down)\.sql$`)

// TestMigrationFiles_UniqueNumbersAndPairedDown is a filesystem check on the
// migration set: no two migrations may claim the same number, and every up
// must have a matching down.
//
// Both risks come from how migration numbers get allocated in practice. During
// the 2026-08 integrity waves the numbers were handed out by a person writing
// them into a contract document, and parallel branches were trusted to honour
// that. golang-migrate does surface a duplicate eventually, but only when
// someone runs it, with an error that describes the symptom rather than the
// two files fighting over the number -- and by then both branches are merged.
//
// deployment.md requires a down script per migration and a rollback-capable
// release. TestMigrations_FullDownChainAndReapply proves the chain executes,
// but it can only execute what exists: an up with no down simply stops the
// chain earlier, which that test cannot distinguish from a shorter chain.
//
// Deliberately does NOT require contiguity. Gaps are legitimate -- 053 was
// allocated to a task that turned out to need no schema change, and reserving
// a number ahead of the work that uses it is the whole point of allocating.
func TestMigrationFiles_UniqueNumbersAndPairedDown(t *testing.T) {
	dir := "sql/migrations"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	type file struct{ num, name string }
	ups := map[string]file{}
	downs := map[string]bool{}
	var unparsed []string

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasSuffix(n, ".sql") {
			continue
		}
		m := migrationFileName.FindStringSubmatch(n)
		if m == nil {
			unparsed = append(unparsed, n)
			continue
		}
		num, dir := m[1], m[3]
		if dir == "down" {
			downs[num] = true
			continue
		}
		if prev, dup := ups[num]; dup {
			t.Errorf("migration number %s is claimed twice: %s and %s -- "+
				"two branches allocated the same number", num, prev.name, n)
			continue
		}
		ups[num] = file{num: num, name: n}
	}

	for _, n := range unparsed {
		t.Errorf("migration file %q does not match <number>_<slug>.(up|down).sql", n)
	}

	var missing []string
	for num, f := range ups {
		if !downs[num] {
			missing = append(missing, f.name)
		}
	}
	sort.Strings(missing)
	for _, f := range missing {
		t.Errorf("%s has no matching .down.sql -- deployment.md requires one per "+
			"migration, or an explicit written reason it cannot be rolled back", f)
	}

	if len(ups) == 0 {
		t.Fatal("no migrations found -- the directory or naming convention changed")
	}
	t.Logf("checked %d migrations, all uniquely numbered and paired", len(ups))
}
