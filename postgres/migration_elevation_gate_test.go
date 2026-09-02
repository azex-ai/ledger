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
//
// M-11 (W3 adversarial review of the gates) reshaped how it reads the files.
// Two holes, both reproduced:
//
//   - It matched line by line, so splitting one statement across two lines
//     (`GRANT ledger_owner` / `  TO current_user;`) hid it. SQL does not care
//     where the newlines are, so neither does this now: statements are split
//     on `;` with comments stripped, and whitespace is flattened.
//   - It only knew about ledger_owner membership. `ALTER ROLE ledger_app
//     SUPERUSER` -- a strictly larger escalation -- was not in its judgement
//     at all. TestRoleAttributes happens to catch that one AFTER the
//     migrations run, but that is a different gate's side effect, not this
//     one's promise.
func TestMigrationsDoNotManageLedgerOwnerMembership(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("sql", "migrations", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 20 {
		t.Fatalf("expected the migration set, found %d files", len(files))
	}

	// Each pattern is matched against a whole statement with whitespace
	// flattened, so `GRANT\n  ledger_owner\n  TO x` reads as one line here.
	forbidden := []struct {
		re  *regexp.Regexp
		why string
	}{
		{regexp.MustCompile(`(?i)\bGRANT\s+ledger_owner\s+TO\b`),
			"grants the runner's ledger_owner membership; postgres.Migrate owns that window (see 018's section 0 note)"},
		{regexp.MustCompile(`(?i)\bREVOKE\s+ledger_owner\s+FROM\b`),
			"revokes the membership postgres.Migrate is currently holding -- Postgres keeps a single grantor row, so a NOSUPERUSER bootstrap dies at the next owner-gated statement"},
		{regexp.MustCompile(`(?i)\bALTER\s+ROLE\s+\S+\s+(WITH\s+)?(SUPERUSER|CREATEROLE|CREATEDB|REPLICATION|BYPASSRLS)\b`),
			"gives a role a cluster-level attribute. Nothing in a migration needs one, and SUPERUSER on ledger_app is a larger escalation than the membership this gate was originally written for (M-11)"},
		{regexp.MustCompile(`(?i)\bGRANT\s+(ALL|pg_\w+)\s+.*\bTO\s+ledger_app\b`),
			"grants ledger_app a blanket or built-in role privilege; its grants are enumerated per object and reviewed in grant_coverage_test.go"},
	}

	for _, f := range files {
		base := filepath.Base(f)
		if strings.HasPrefix(base, "001_") {
			continue
		}
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range sqlStatements(string(body)) {
			for _, rule := range forbidden {
				if rule.re.MatchString(stmt.text) {
					t.Errorf("%s (statement near line %d) %s:\n\t%s\n\n"+
						"Reformatting will not help: statements are read whole, with comments stripped and whitespace flattened.",
						base, stmt.line, rule.why, stmt.text)
				}
			}
		}
	}
}

type migrationStatement struct {
	text string // comments stripped, whitespace flattened
	line int    // 1-based line the statement starts on
}

// sqlStatements splits a migration into statements on `;`, stripping `--`
// comments and flattening whitespace. Dollar-quoted function bodies contain
// their own semicolons; splitting inside one only ever produces MORE
// fragments to match against, never fewer, so the patterns above still see
// everything (an escalation hidden inside a function body is matched as a
// fragment of it).
func sqlStatements(body string) []migrationStatement {
	var out []migrationStatement
	var sb strings.Builder
	start := 1
	flush := func(line int) {
		text := strings.Join(strings.Fields(sb.String()), " ")
		if text != "" {
			out = append(out, migrationStatement{text: text, line: start})
		}
		sb.Reset()
		start = line + 1
	}
	for i, line := range strings.Split(body, "\n") {
		code := line
		if j := strings.Index(code, "--"); j >= 0 {
			code = code[:j]
		}
		if strings.TrimSpace(code) == "" {
			if strings.TrimSpace(sb.String()) == "" {
				start = i + 2
			}
			continue
		}
		for {
			semi := strings.Index(code, ";")
			if semi < 0 {
				sb.WriteString(code)
				sb.WriteByte(' ')
				break
			}
			sb.WriteString(code[:semi])
			flush(i + 1)
			code = code[semi+1:]
		}
	}
	flush(len(strings.Split(body, "\n")))
	return out
}
