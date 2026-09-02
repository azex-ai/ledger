package ledger_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// examples_static_test.go: cheap grep-shaped static assertions over
// examples/**/main.go and README.md, one per D-surface finding that has no
// better mechanical home. Each of these was a real drifted-example bug found
// by docs/audits/2026-09-02-deep-audit/TODO.md; the pin is deliberately as
// simple as the finding was.

// --- E-M10: the only HTTP composition-root constructor taught anywhere is
// NewFromDeps -- server.NewWithConfig's 21 same-shaped positional params are
// exactly the trap H-R1/E-M10 found examples/fullstack and README.md
// teaching. -----------------------------------------------------------

func TestNoNewWithConfigInDocsOrExamples(t *testing.T) {
	newWithConfigRE := regexp.MustCompile(`server\.NewWithConfig\(`)
	checkNoMatch(t, newWithConfigRE, "README.md",
		"README.md must not teach server.NewWithConfig( -- the only documented HTTP composition-root entry point is server.NewFromDeps (E-M10)")
	forEachExampleMainGo(t, func(t *testing.T, path, src string) {
		if newWithConfigRE.MatchString(src) {
			t.Errorf("%s calls server.NewWithConfig( -- examples must use server.NewFromDeps instead (E-M10)", path)
		}
	})
}

// --- E-M7: the at-least-once event delivery wording must not regress back
// to the at-most-once claim it replaced. -------------------------------

func TestNoAtMostOnceDeliveryWording(t *testing.T) {
	banned := []string{"still marked delivered", "at-most-once"}
	checkNoBannedPhrases(t, "README.md", banned,
		"contradicts Worker.Subscribe's documented at-least-once delivery contract (service/worker.go) -- E-M7")
	forEachExampleMainGo(t, func(t *testing.T, path, src string) {
		for _, phrase := range banned {
			if strings.Contains(src, phrase) {
				t.Errorf("%s contains %q -- contradicts Worker.Subscribe's documented at-least-once delivery contract (E-M7)", path, phrase)
			}
		}
	})
}

// --- E-m10: every example's USDT Currency uses the same Exponent literal,
// so examples can share one database without one refusing to boot against a
// currency another already created at a different precision. ----------

func TestExampleUSDTExponentsAreConsistent(t *testing.T) {
	// (?s) so Exponent: can appear on a later line than Code: "USDT" inside
	// the same struct literal (gofmt wraps long CurrencyInput{} literals).
	usdtRE := regexp.MustCompile(`(?s)Code:\s*"USDT".{0,200}?Exponent:\s*(\d+)`)

	seen := map[string][]string{} // exponent -> files
	forEachExampleMainGo(t, func(t *testing.T, path, src string) {
		for _, m := range usdtRE.FindAllStringSubmatch(src, -1) {
			seen[m[1]] = append(seen[m[1]], path)
		}
	})
	if len(seen) > 1 {
		t.Errorf("examples declare USDT at more than one Exponent: %v -- every example must use the same precision so a fresh DB works for whichever one runs first (E-m10)", seen)
	}
}

// --- E-m11: no example's own demo tables reference a ledger dimension by
// internal id -- api-contract.md §3 / README's own "uid is the only
// identifier" claim applies to a consumer's schema too, and an example
// contradicting it is the worst possible place for that habit to spread
// from. -------------------------------------------------------------

func TestExampleDemoTablesUseUIDNotInternalID(t *testing.T) {
	// Matches a SQL column name ending in _id that names a ledger dimension
	// that IS uid-identified (currency/classification/journal/booking).
	// holder_id is deliberately excluded: AccountHolder is an int64
	// throughout this library (README's Core Concepts: "Positive holder =
	// user-side, negative = system counterpart"), never a uid -- it is not
	// part of the uid-only contract this check exists to enforce.
	// currency_uid-style names are fine and do not match (no "_id" suffix).
	dimIDRE := regexp.MustCompile(`(?i)\b(currency|classification|journal|booking)_id\b`)
	forEachExampleMainGo(t, func(t *testing.T, path, src string) {
		for _, stmt := range extractCreateTableStatements(src) {
			if m := dimIDRE.FindString(stmt); m != "" {
				t.Errorf("%s: CREATE TABLE statement uses %q, a ledger-internal id column name -- examples must reference ledger dimensions by uid (e.g. currency_uid), never the internal BIGSERIAL id (E-m11, README's uid-only contract)", path, m)
			}
		}
	})
}

func extractCreateTableStatements(src string) []string {
	re := regexp.MustCompile(`(?is)CREATE TABLE.*?\)\s*`)
	return re.FindAllString(src, -1)
}

// --- shared helpers ---------------------------------------------------

func forEachExampleMainGo(t *testing.T, fn func(t *testing.T, path, src string)) {
	t.Helper()
	matches, err := filepath.Glob("examples/*/main.go")
	if err != nil {
		t.Fatal(err)
	}
	nested, err := filepath.Glob("examples/*/*/main.go") // examples/fullstack/backend/main.go
	if err != nil {
		t.Fatal(err)
	}
	matches = append(matches, nested...)
	if len(matches) == 0 {
		t.Fatal("no examples/**/main.go files found -- glob is broken")
	}
	for _, path := range matches {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		fn(t, path, string(b))
	}
}

func checkNoMatch(t *testing.T, re *regexp.Regexp, path, msg string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if re.MatchString(string(b)) {
		t.Errorf("%s: %s", path, msg)
	}
}

func checkNoBannedPhrases(t *testing.T, path string, phrases []string, why string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, phrase := range phrases {
		if strings.Contains(src, phrase) {
			t.Errorf("%s contains %q -- %s", path, phrase, why)
		}
	}
}

// --- M-5 (2026-09-02 adversarial re-review, w3-review/money-path.md): no
// example may hand the same connection URL to Migrate and to pgxpool.New.
//
// For a non-superuser runner, postgres.Migrate grants that role
// `ledger_owner WITH INHERIT TRUE` for the length of each migration. That
// grant is a row in pg_auth_members -- cluster-wide and ROLE-scoped, not
// session-scoped -- so while it is held, every session authenticated as the
// same role inherits ledger_owner, including the application's own pool. All
// eight examples used one DATABASE_URL for both, which is precisely the
// shape that makes an application pool owner-equivalent (able to DROP the
// append-only triggers) for the duration of a migration run.
//
// The library cannot enforce credential separation -- it never sees the
// operator's roles -- so the examples are where the practice is taught, and
// this is the gate that keeps them teaching it. -----------------------

func TestExamplesUseASeparateMigrationURL(t *testing.T) {
	migrateRE := regexp.MustCompile(`(?:postgres|ledger)\.Migrate(?:Context)?\(([^,)]+)`)
	poolRE := regexp.MustCompile(`pgxpool\.New\([^,]+,\s*([^,)]+)`)

	forEachExampleMainGo(t, func(t *testing.T, path, src string) {
		migrateArgs := migrateRE.FindAllStringSubmatch(src, -1)
		if len(migrateArgs) == 0 {
			return // an example that does not migrate has nothing to separate
		}
		poolArgs := poolRE.FindAllStringSubmatch(src, -1)
		if len(poolArgs) == 0 {
			t.Errorf("%s calls Migrate but never pgxpool.New -- this gate's assumption about example shape no longer holds", path)
			return
		}
		for _, m := range migrateArgs {
			migrateVar := strings.TrimSpace(m[1])
			for _, p := range poolArgs {
				poolVar := strings.TrimSpace(p[1])
				if migrateVar == poolVar {
					t.Errorf("%s passes %s to both Migrate and pgxpool.New -- migrations must run on their own credential "+
						"(MIGRATE_DATABASE_URL), because Migrate's ledger_owner grant is role-wide and every session on that "+
						"role inherits it for the length of the run (see docs/RUNBOOK.md \"Database roles\")", path, migrateVar)
				}
			}
		}
		if !strings.Contains(src, "MIGRATE_DATABASE_URL") {
			t.Errorf("%s migrates without reading MIGRATE_DATABASE_URL -- the separation has to be visible to a reader "+
				"copying this example, not only true of its variable names", path)
		}
	})
}
