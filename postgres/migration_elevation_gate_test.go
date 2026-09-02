package postgres_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestMigrationsDoNotManageLedgerOwnerMembership pins the rule D-M2 (2026-09-02
// audit) settled on: postgres.Migrate is the only thing that grants and revokes
// the runner's ledger_owner membership, one window per migration. A migration
// that does it itself (018 used to) REVOKEs the very membership Migrate is
// holding -- Postgres keeps a single grantor row -- and a NOSUPERUSER bootstrap
// dies at the next owner-gated statement. 001 is the one legitimate site: it
// creates the role and ends by handing the membership back.
func TestMigrationsDoNotManageLedgerOwnerMembership(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("sql", "migrations", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 20 {
		t.Fatalf("expected the migration set, found %d files", len(files))
	}
	re := regexp.MustCompile(`(?i)(GRANT\s+ledger_owner\s+TO|REVOKE\s+ledger_owner\s+FROM)`)
	for _, f := range files {
		base := filepath.Base(f)
		if strings.HasPrefix(base, "001_") {
			continue
		}
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "--") {
				continue // prose may describe the rule; only statements count
			}
			if re.MatchString(line) {
				t.Errorf("%s:%d manages the runner's ledger_owner membership itself; postgres.Migrate owns that window (see 018's section 0 note)", base, i+1)
			}
		}
	}
}
