package ledger_test

import (
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/azex-ai/ledger/internal/postgrestest"
)

// readme_runnable_test.go turns the manual verification the second-round
// audit did by hand (docs/audits/2026-09-02-deep-audit/consumer-surface.md:
// "把 README 的 20 个 Go 代码块逐字抠进一个临时 module...编译" -- a scratchpad
// exercise, not a repeatable gate) into an automated one for the specific
// class of bug readme_api_surface_test.go cannot see: a README example that
// COMPILES but FAILS AT RUNTIME. E-M2 found exactly that -- "Add a custom
// lifecycle" builds a ClassificationInput with no BalanceRole and fails
// against a real database with "must declare an explicit balance_role".
//
// Each test below extracts one README ```go block VERBATIM by anchor
// substring, splices it into a minimal runnable program (imports + a small
// preamble providing whatever the block assumes already exists -- a pool, a
// currency, ...), and `go run`s it against a real, empty, freshly-migrated
// database. A stale anchor (the README text changed shape) fails loudly
// rather than silently skipping -- working-agreements.md §3: "未运行 ≠ 通过".

// readmeRunnableAnchors is every anchor the runnable tests below use, and the
// input to TestREADMEGoBlocksAreAllClassified: a block containing one of
// these is executed against a real database by the test that names it.
//
// Constants rather than literals at the call sites so the coverage gate reads
// the same list the tests do -- a hand-copied second list is exactly the
// drift shape this file exists to close.
const (
	anchorTier1QuickStart      = "j, err := svc.JournalWriter().PostJournal(ctx, core.JournalInput{"
	anchorTier2QuickStart      = `// 8 bundles, see "Built-in Presets" below`
	anchorCustomLifecycle      = `"kyc_review"`
	anchorCustomClassification = `"promotion_credit"`
	anchorCustomJournalType    = `CreateJournalType(ctx, core.JournalTypeInput{`
	anchorCustomTemplate       = `CreateTemplate(ctx, core.TemplateInput{`
)

var readmeRunnableAnchors = []string{
	anchorTier1QuickStart,
	anchorTier2QuickStart,
	anchorCustomLifecycle,
	anchorCustomClassification,
	anchorCustomJournalType,
	anchorCustomTemplate,
}

var goFenceRE = regexp.MustCompile(`(?s)` + "```go\n(.*?)\n```")

// extractREADMEGoBlock returns the verbatim text of the first ```go fenced
// code block in README.md containing anchor.
func extractREADMEGoBlock(t *testing.T, anchor string) string {
	t.Helper()
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range goFenceRE.FindAllStringSubmatch(string(readme), -1) {
		if strings.Contains(m[1], anchor) {
			return m[1]
		}
	}
	t.Fatalf("no ```go block in README.md contains %q -- this test's extraction anchor is stale (README changed shape)", anchor)
	return ""
}

// stripLeadingImport removes a leading `import (...)` block from an
// extracted README snippet -- the generated program supplies its own import
// list (which also covers the preamble's needs), so splicing the block's own
// import block in verbatim would double-import every package it names.
func stripLeadingImport(block string) string {
	re := regexp.MustCompile(`(?s)^import \([^)]*\)\n+`)
	return re.ReplaceAllString(block, "")
}

// runGoProgram writes src to a throwaway package-main file inside the module
// (so `go run` resolves github.com/azex-ai/ledger and go.work normally),
// runs it with DATABASE_URL set to dbURL, and fails the test on a non-zero
// exit or any stderr/stdout the program's own panic/print didn't expect.
func runGoProgram(t *testing.T, dbURL, src string) {
	t.Helper()
	dir, err := os.MkdirTemp(".", "readmecheck-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	mainPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "run", mainPath)
	cmd.Env = append(os.Environ(), "DATABASE_URL="+dbURL)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("README example failed to run:\n--- generated program ---\n%s\n--- output ---\n%s\n--- error ---\n%v",
			src, out, err)
	}
}

// TestREADMETier1QuickStartRuns runs the "Tier 1 — Hello Ledger" block
// verbatim (minus its own leading import block, which the harness supplies)
// against a real database, seeding the one currency/classification/journal
// type the block's own comment says a caller must provide first.
func TestREADMETier1QuickStartRuns(t *testing.T) {
	dbURL := postgrestest.SetupRawDB(t)
	block := stripLeadingImport(extractREADMEGoBlock(t, anchorTier1QuickStart))

	src := `package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
)

func main() {
	ctx := context.Background()
	dbURL := os.Getenv("DATABASE_URL")
	// The test fixture is a single connection playing both roles: real
	// deployments point MIGRATE_DATABASE_URL at a CREATEROLE-capable
	// connection and DATABASE_URL at ledger_app (README's "Prerequisite"),
	// but postgrestest.SetupRawDB hands back one superuser connection to a
	// database nothing has installed ledger_app's narrower grants against
	// yet, so both names resolve to it here.
	migrateDBURL := dbURL

	// The block below also calls ledger.Migrate(migrateDBURL) itself (its
	// first statement, verbatim from README) -- that is fine, Migrate is
	// idempotent (postgres/migrate.go tolerates migrate.ErrNoChange). It has
	// to happen here too because the schema must exist before the seed
	// writes below.
	if err := ledger.Migrate(dbURL); err != nil {
		panic(err)
	}

	// Seed the one Currency/Classification/JournalType the README block's
	// own comment says Tier 1 needs before any post (README: "see
	// examples/embed/main.go for a self-contained boot"), using a separate
	// pool/Service from the ones the block below declares for itself.
	seedPool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		panic(err)
	}
	seedSvc, err := ledger.New(seedPool)
	if err != nil {
		panic(err)
	}
	cur, err := seedSvc.Currencies().CreateCurrency(ctx, core.CurrencyInput{Code: "USDT", Name: "Tether USD", Exponent: 6})
	if err != nil {
		panic(err)
	}
	currencyUID := cur.UID
	custody, err := seedSvc.Classifications().CreateClassification(ctx, core.ClassificationInput{
		Code: "custody", Name: "Custody", NormalSide: core.NormalSideCredit, IsSystem: true,
	})
	if err != nil {
		panic(err)
	}
	custodyUID := custody.UID
	wallet, err := seedSvc.Classifications().CreateClassification(ctx, core.ClassificationInput{
		Code: "wallet", Name: "Wallet", NormalSide: core.NormalSideDebit, IsSystem: false, BalanceRole: core.BalanceRoleAvailable,
	})
	if err != nil {
		panic(err)
	}
	walletUID := wallet.UID
	jt, err := seedSvc.JournalTypes().CreateJournalType(ctx, core.JournalTypeInput{Code: "hello", Name: "Hello"})
	if err != nil {
		panic(err)
	}
	jtUID := jt.UID
	seedPool.Close()

	// --- README "Tier 1 — Hello Ledger" block, verbatim below this line ---
	// (wrapped in its own scope so the block's own ":=" redeclarations of
	// names the preamble also uses, like err, are legal shadows rather than
	// "no new variables on left side of :=" compile errors)
	{
` + block + `
		_ = j
		if bal.IsZero() {
			panic("expected a non-zero balance after posting")
		}
	}
}
`
	runGoProgram(t, dbURL, src)
}

// TestREADMETier2QuickStartRuns runs the "Tier 2 — With Built-in Presets"
// block verbatim, seeding only the one currency the block itself does not
// create (InstallExtendedPresets creates every classification/journal
// type/template it uses).
func TestREADMETier2QuickStartRuns(t *testing.T) {
	dbURL := postgrestest.SetupRawDB(t)
	block := extractREADMEGoBlock(t, anchorTier2QuickStart)

	src := `package main

import (
	"context"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/presets"
)

func main() {
	ctx := context.Background()
	dbURL := os.Getenv("DATABASE_URL")
	if err := ledger.Migrate(dbURL); err != nil {
		panic(err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		panic(err)
	}
	defer pool.Close()
	seedSvc, err := ledger.New(pool)
	if err != nil {
		panic(err)
	}
	cur, err := seedSvc.Currencies().CreateCurrency(ctx, core.CurrencyInput{Code: "USDT", Name: "Tether USD", Exponent: 6})
	if err != nil {
		panic(err)
	}
	currencyUID := cur.UID

	// --- README "Tier 2 — With Built-in Presets" block, verbatim below ---
	{
` + block + `
	}
}
`
	runGoProgram(t, dbURL, src)
}

// TestREADMECustomLifecycleRuns runs the "Add a custom lifecycle (state
// machine)" block verbatim. This is the exact block E-M2 found: it compiled
// (readme_api_surface_test.go's prior coverage) but failed at runtime
// against a real database because it omitted the now-required BalanceRole.
func TestREADMECustomLifecycleRuns(t *testing.T) {
	dbURL := postgrestest.SetupRawDB(t)
	block := extractREADMEGoBlock(t, anchorCustomLifecycle)

	src := `package main

import (
	"context"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
)

func main() {
	ctx := context.Background()
	dbURL := os.Getenv("DATABASE_URL")
	if err := ledger.Migrate(dbURL); err != nil {
		panic(err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		panic(err)
	}
	defer pool.Close()
	svc, err := ledger.New(pool)
	if err != nil {
		panic(err)
	}

	// --- README "Add a custom lifecycle (state machine)" block, verbatim ---
	_, err = ` + block + `
	if err != nil {
		panic(err)
	}
	// --- end README block ---
}
`
	runGoProgram(t, dbURL, src)
}

// TestREADMECustomClassificationRuns runs the "Add a custom classification"
// block verbatim -- it declares IsSystem: true, so it is exempt from the
// BalanceRole requirement, but running it (not just compiling it) is what
// would have caught that exemption silently regressing.
func TestREADMECustomClassificationRuns(t *testing.T) {
	dbURL := postgrestest.SetupRawDB(t)
	block := extractREADMEGoBlock(t, anchorCustomClassification)

	src := `package main

import (
	"context"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
)

func main() {
	ctx := context.Background()
	dbURL := os.Getenv("DATABASE_URL")
	if err := ledger.Migrate(dbURL); err != nil {
		panic(err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		panic(err)
	}
	defer pool.Close()
	svc, err := ledger.New(pool)
	if err != nil {
		panic(err)
	}

	// --- README "Add a custom classification" block, verbatim below ---
` + block + `
	// --- end README block ---
}
`
	runGoProgram(t, dbURL, src)
}

// TestREADMECustomJournalTypeRuns runs the "Add a custom journal type" block
// verbatim.
func TestREADMECustomJournalTypeRuns(t *testing.T) {
	dbURL := postgrestest.SetupRawDB(t)
	block := extractREADMEGoBlock(t, anchorCustomJournalType)

	src := `package main

import (
	"context"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
)

func main() {
	ctx := context.Background()
	dbURL := os.Getenv("DATABASE_URL")
	if err := ledger.Migrate(dbURL); err != nil {
		panic(err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		panic(err)
	}
	defer pool.Close()
	svc, err := ledger.New(pool)
	if err != nil {
		panic(err)
	}

	// --- README "Add a custom journal type" block, verbatim below ---
` + block + `
	// --- end README block ---
}
`
	runGoProgram(t, dbURL, src)
}

// TestREADMECustomTemplateRuns runs the "Add a custom template" block
// verbatim AND then executes the template it defines.
//
// M-12 (W3 adversarial review of the gates): the reviewer changed this
// block's second leg to read a different AmountKey than its first. It still
// compiles, and CreateTemplate still succeeds -- the two legs are only
// reconciled when the template is RENDERED, which is when a reader following
// the README discovers their promo grant cannot be posted. Creating the
// template is therefore not enough to pin the recipe; executing it is.
func TestREADMECustomTemplateRuns(t *testing.T) {
	dbURL := postgrestest.SetupRawDB(t)
	block := extractREADMEGoBlock(t, anchorCustomTemplate)

	src := `package main

import (
	"context"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
)

func main() {
	ctx := context.Background()
	dbURL := os.Getenv("DATABASE_URL")
	if err := ledger.Migrate(dbURL); err != nil {
		panic(err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		panic(err)
	}
	defer pool.Close()
	svc, err := ledger.New(pool)
	if err != nil {
		panic(err)
	}

	// The names the README block reads: the two classifications a promotion
	// grant moves value between, and the journal type it books under.
	cur, err := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{Code: "USDT", Name: "Tether USD", Exponent: 6})
	if err != nil {
		panic(err)
	}
	equity, err := svc.Classifications().CreateClassification(ctx, core.ClassificationInput{
		Code: "equity", Name: "Equity", NormalSide: core.NormalSideDebit, IsSystem: true,
	})
	if err != nil {
		panic(err)
	}
	equityUID := equity.UID
	wallet, err := svc.Classifications().CreateClassification(ctx, core.ClassificationInput{
		Code: "wallet", Name: "Wallet", NormalSide: core.NormalSideDebit, IsSystem: false, BalanceRole: core.BalanceRoleAvailable,
	})
	if err != nil {
		panic(err)
	}
	walletUID := wallet.UID
	jt, err := svc.JournalTypes().CreateJournalType(ctx, core.JournalTypeInput{Code: "promo_grant", Name: "Promotion Grant"})
	if err != nil {
		panic(err)
	}
	jtUID := jt.UID

	// --- README "Add a custom template" block, verbatim below ---
` + block + `
	// --- end README block ---

	// Now USE it, which is the half a create-only check cannot see: a leg
	// whose AmountKey no caller supplies renders as an unbalanced (or
	// missing-parameter) journal, and only here does that surface.
	if _, err := svc.JournalWriter().ExecuteTemplate(ctx, "promo_grant", core.TemplateParams{
		CurrencyUID:    cur.UID,
		HolderID:       4242,
		Amounts:        map[string]decimal.Decimal{"amount": decimal.RequireFromString("25")},
		IdempotencyKey: "readme-promo-grant-1",
		Source:         "readme-check",
	}); err != nil {
		panic("the README's promo_grant template could not be executed with the parameters the README itself documents: " + err.Error())
	}
}
`
	runGoProgram(t, dbURL, src)
}

// --- W5-readme: every ```go block in README.md is classified, and every
// block that is not run against a real database must at least COMPILE ---
//
// The six tests above run six blocks against a real, migrated database.
// README has twenty-four blocks; the other eighteen used to be waved through
// by a hand-maintained exemption table that only required the block to
// PARSE. Parsing catches a truncated line or an unbalanced brace. It cannot
// catch a snippet that calls a function with the wrong arity or a method
// that no longer exists -- exactly what the 2026-09-03 consumer audit found
// twice in the same README: `worker := svc.Worker(cfg)` (assignment mismatch
// -- svc.Worker now returns two values) and `srv.Handler()` (*server.Server
// has no such method, it implements http.Handler directly) both PARSE fine
// -- a `:=` assignment and a method call are syntactically unremarkable --
// and both fail to COMPILE. A gate that only parses cannot see either bug,
// which is how both shipped and stayed broken through a full audit round.
//
// So: every block not covered by one of the six run-tests above is now
// compiled -- spliced into a throwaway `package main` with an inferred
// import list and a fixed preamble of the names README prose assumes
// already exist (svc, ctx, pool, jtUID, ...) -- unless the README itself
// marks it, on the line immediately after the closing fence, with
// `<!-- readme-gate: snippet -- <reason> -->`. That marker is the only way
// to opt a block out, it has to carry a real reason (checked below), and an
// unmarked block that does not compile is red. A brand new block defaults to
// red too: nothing here waves through what it has never seen classified.

// readmeGoBlocks returns every ```go block in README.md.
func readmeGoBlocks(t *testing.T) []string {
	t.Helper()
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range goFenceRE.FindAllStringSubmatch(string(readme), -1) {
		out = append(out, m[1])
	}
	if len(out) < 20 {
		t.Fatalf("found only %d ```go blocks in README.md -- the fence regexp regressed, and a scan that sees almost nothing reads as a pass", len(out))
	}
	return out
}

// blockFingerprint is a block's first line of actual code: stable across
// edits to the rest of the block, and readable in a test name / failure.
func blockFingerprint(block string) string {
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		return trimmed
	}
	return "(empty block)"
}

// readmeSnippetMarkerPrefix is the HTML comment a README author places on
// the line immediately after a ```go block's closing fence to opt it out of
// the compile gate below. It must be followed by "-- " and a reason; a bare
// marker with no reason is itself a gate failure (see
// TestREADMEGoBlocksCompileUnlessMarkedSnippet) -- "opted out" and "opted out
// because X" are different claims, and only the second is checkable by a
// future reader.
const readmeSnippetMarkerPrefix = "<!-- readme-gate: snippet"

// readmeSnippetMarker reports whether the text immediately following a
// ```go block's closing fence is a readme-gate marker, and the reason text
// it carries (empty if the marker is missing its reason, or missing
// entirely).
func readmeSnippetMarker(afterFence string) (marked bool, reason string) {
	afterFence = strings.TrimLeft(afterFence, "\n")
	line := afterFence
	if i := strings.IndexByte(afterFence, '\n'); i >= 0 {
		line = afterFence[:i]
	}
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, readmeSnippetMarkerPrefix) {
		return false, ""
	}
	reason = strings.TrimSuffix(line, "-->")
	reason = strings.TrimPrefix(reason, readmeSnippetMarkerPrefix)
	reason = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(reason), "--"))
	return true, reason
}

// readmeBlock pairs a ```go block's verbatim text with whatever
// readme-gate marker immediately follows its closing fence.
type readmeBlock struct {
	text   string
	marked bool
	reason string
}

// readmeGoBlocksWithMarkers is readmeGoBlocks plus each block's marker --
// found by looking at the README source immediately after the closing fence,
// not by a second independent scan, so the marker can never drift from the
// block it annotates.
func readmeGoBlocksWithMarkers(t *testing.T) []readmeBlock {
	t.Helper()
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(readme)
	var out []readmeBlock
	for _, m := range goFenceRE.FindAllStringSubmatchIndex(s, -1) {
		text := s[m[2]:m[3]]
		marked, reason := readmeSnippetMarker(s[m[1]:])
		out = append(out, readmeBlock{text: text, marked: marked, reason: reason})
	}
	if len(out) < 20 {
		t.Fatalf("found only %d ```go blocks in README.md -- the fence regexp regressed, and a scan that sees almost nothing reads as a pass", len(out))
	}
	return out
}

// readmeExtraImportsByPrefix maps a package short name a README block might
// reference (as the block's own source spells it, e.g. "slog.New(...)") to
// the import line needed to resolve it. The compile gate scans each block
// (with line comments stripped, so a package named only in prose does not
// pull in an import nothing in the block's actual code uses -- which would
// itself fail to compile as "imported and not used") for these prefixes and
// includes only the ones a block actually references.
//
// context / pgxpool / decimal / ledger / core / server / service are NOT
// here: readmeCompilePreamble below uses all seven unconditionally, so they
// are part of every compiled program's fixed import list regardless of
// which of those packages a given block's own text happens to mention. A
// hand-maintained miss in this table fails LOUD (the block will not
// compile) rather than by silently resolving to the wrong package -- there
// is no general resolver here, just a small, closed vocabulary this one
// README uses.
var readmeExtraImportsByPrefix = []struct {
	prefix string
	imp    string
}{
	{"presets", `"github.com/azex-ai/ledger/presets"`},
	{"channel", `"github.com/azex-ai/ledger/channel"`},
	{"observability", `"github.com/azex-ai/ledger/observability"`},
	{"slog", `"log/slog"`},
	{"otel", `"go.opentelemetry.io/otel"`},
	{"trace", `"go.opentelemetry.io/otel/sdk/trace"`},
	{"ledgerotel", `ledgerotel "github.com/azex-ai/ledger/pkg/otel"`},
	{"http", `"net/http"`},
	{"os", `"os"`},
	{"log", `"log"`},
	{"testing", `"testing"`},
	{"anchortest", `"github.com/azex-ai/ledger/anchortest"`},
}

// stripLineComment returns line with any trailing `//` comment removed,
// aware enough of string/rune literals not to cut on a `//` that appears
// inside one (reusing braceDepth's scan style). Used before prefix-matching
// a block for inferExtraImports, so a package named only in prose ("// see
// slog for more") is not mistaken for actual usage.
func stripLineComment(line string) string {
	inString, inRune := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inString:
			switch c {
			case '\\':
				i++
			case '"':
				inString = false
			}
		case inRune:
			switch c {
			case '\\':
				i++
			case '\'':
				inRune = false
			}
		case c == '"':
			inString = true
		case c == '\'':
			inRune = true
		case c == '/' && i+1 < len(line) && line[i+1] == '/':
			return line[:i]
		}
	}
	return line
}

// inferExtraImports returns the import lines from readmeExtraImportsByPrefix
// that block's code (comments stripped) actually references.
func inferExtraImports(block string) []string {
	var out []string
	for _, spec := range readmeExtraImportsByPrefix {
		re := regexp.MustCompile(`\b` + regexp.QuoteMeta(spec.prefix) + `\.`)
		for _, line := range strings.Split(block, "\n") {
			if re.MatchString(stripLineComment(line)) {
				out = append(out, spec.imp)
				break
			}
		}
	}
	return out
}

// leadingImportRE matches a single leading import declaration -- either a
// parenthesized block or a single `import "path"` / `import alias "path"`
// line -- at the very start of a README snippet.
var leadingImportRE = regexp.MustCompile(`(?s)^import\s+(?:\([^)]*\)|(?:\w+\s+)?"[^"]*")\n+`)

// stripLeadingImportAny is stripLeadingImport generalised to the single-line
// import form several README blocks use (`import "log/slog"`,
// `import ledgerotel "..."`) in addition to the parenthesized form. The
// compile gate always regenerates the block's import list from
// inferExtraImports plus the fixed preamble imports, rather than preserving
// whatever the block declared for itself -- several blocks' own leading
// import covers only PART of what they use (e.g. the slog adapter block
// imports "log/slog" but not "os", which os.Stdout still needs), so keeping
// it verbatim would under-import exactly the class of bug this gate exists
// to catch.
func stripLeadingImportAny(block string) string {
	return leadingImportRE.ReplaceAllString(block, "")
}

// readmeCompileDeclKeywords are the keywords that open a top-level
// declaration inside a compile-tested block -- everything else is a
// statement, meant to run inside func main.
var readmeCompileDeclKeywords = []string{"type", "func", "var", "const"}

// bucketForCompile splits an (import-stripped) README snippet into top-level
// declarations and plain statements, the same way TestREADMEGoBlocksParse's
// syntheticFileFor does for parsing -- README blocks mix both shapes in one
// fence (the slog adapter block is a type, three methods, and two
// statements), so both gates need the split. Kept as its own copy rather
// than shared with syntheticFileFor: the parse gate's synthetic file also
// buckets bare `import` lines (this function does not -- callers strip
// imports first via stripLeadingImportAny), and the two gates fail
// independently on purpose.
func bucketForCompile(block string) (decls []string, stmts []string) {
	lines := strings.Split(block, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		isDecl := false
		for _, kw := range readmeCompileDeclKeywords {
			if strings.HasPrefix(line, kw+" ") || strings.HasPrefix(line, kw+"(") {
				isDecl = true
				break
			}
		}
		if !isDecl {
			stmts = append(stmts, line)
			continue
		}
		decl := []string{line}
		if depth := braceDepth(line); depth > 0 {
			for i+1 < len(lines) {
				i++
				decl = append(decl, lines[i])
				depth += braceDepth(lines[i])
				if depth <= 0 {
					break
				}
			}
		}
		decls = append(decls, strings.Join(decl, "\n"))
	}
	return decls, stmts
}

// topLevelShortDeclRE matches a `:=` short variable declaration at the very
// start of a (trimmed) statement line -- `svc, _ := ledger.New(...)`,
// `err := svc.RunInTx(...)`.
var topLevelShortDeclRE = regexp.MustCompile(`^(\w+)(?:,\s*(\w+))?\s*:=`)

// topLevelShortDecls returns the identifiers a block's own statements
// short-declare at the statement list's OWN nesting level -- not inside a
// nested if/for/closure literal, which has its own scope and is usually
// already self-contained.
//
// Most of these blocks show a call, not a complete program: `svc, _ :=
// ledger.New(pool, ledger.WithLogger(...))` with nothing after it is exactly
// what a README paragraph about constructing a Service WITH a logger option
// looks like, and Go's "declared and not used" check does not know that the
// next paragraph of a reader's real program is what would use svc. This is
// what compileSyntheticProgram's sink references (`_ = svc`) exist to
// answer for -- not a workaround for a bug in the block, but the standalone
// program's stand-in for "and then the caller's code goes on to use this".
func topLevelShortDecls(stmts []string) []string {
	var names []string
	depth := 0
	for _, line := range stmts {
		if depth == 0 {
			if m := topLevelShortDeclRE.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
				for _, name := range m[1:] {
					if name != "" && name != "_" {
						names = append(names, name)
					}
				}
			}
		}
		depth += braceDepth(line)
	}
	return names
}

// readmeCompilePreamble is spliced, unmodified, into every compile-tested
// block: a fixed set of package-level values covering every name README
// prose assumes already exists (svc, ctx, pool, the *UID strings, ...).
// Package-level declarations are exempt from Go's "declared and not used"
// check (unlike locals), so the same preamble works for every block
// regardless of which of these names that particular block's own text
// happens to reference -- there is no per-block trimming to keep in sync,
// and no risk of a block compiling today and failing tomorrow because a
// sibling block's edit changed what the shared preamble needs to declare.
const readmeCompilePreamble = `
var (
	svc  *ledger.Service
	pool *pgxpool.Pool
	ctx  = context.Background()
	cfg  = &server.Config{Env: "dev"}

	jtUID, currencyUID, walletUID, custodyUID, equityUID, feesUID, key string

	amt                            = decimal.NewFromInt(100)
	params, lockParams, feeParams  core.TemplateParams
	err                            error

	depositClass *core.Classification
	booking      *core.Booking
	jt           *core.JournalType
	worker       *service.Worker
)
`

// readmeCompileBaseImports are the imports readmeCompilePreamble itself
// needs -- present in every compiled program regardless of which of
// inferExtraImports' packages a given block additionally references.
var readmeCompileBaseImports = []string{
	`"context"`,
	`"github.com/jackc/pgx/v5/pgxpool"`,
	`"github.com/shopspring/decimal"`,
	`"github.com/azex-ai/ledger"`,
	`"github.com/azex-ai/ledger/core"`,
	`"github.com/azex-ai/ledger/server"`,
	`"github.com/azex-ai/ledger/service"`,
}

// compileSyntheticProgram reassembles a README snippet into a standalone
// `package main` program: readmeCompileBaseImports plus whatever
// inferExtraImports finds, readmeCompilePreamble's package-level values, the
// block's own top-level declarations (if any), and the block's own
// statements wrapped in a `func run() error` (not `func main()` directly --
// the server Quick Start block's own text is `if err != nil { return err }`,
// which only compiles inside a function that returns an error; run() ends
// in `return nil` so a block that never returns for itself still compiles).
// After the block's own statements and before that trailing return, one
// `_ = name` sink line per topLevelShortDecls name silences "declared and
// not used" for a variable the block constructs and, being a fragment
// rather than a complete program, never goes on to use.
//
// Never run -- runGoBuild only compiles it -- so it needs no database and no
// *testing.T inside the generated file.
func compileSyntheticProgram(block string) string {
	block = stripLeadingImportAny(block)
	decls, stmts := bucketForCompile(block)
	extra := inferExtraImports(block)
	sinks := topLevelShortDecls(stmts)

	var b strings.Builder
	b.WriteString("package main\n\nimport (\n")
	for _, imp := range readmeCompileBaseImports {
		b.WriteString("\t" + imp + "\n")
	}
	for _, imp := range extra {
		b.WriteString("\t" + imp + "\n")
	}
	b.WriteString(")\n")
	b.WriteString(readmeCompilePreamble)
	b.WriteString("\n")
	for _, decl := range decls {
		b.WriteString(decl)
		b.WriteString("\n\n")
	}
	b.WriteString("func run() error {\n")
	b.WriteString(strings.Join(stmts, "\n"))
	b.WriteString("\n")
	for _, name := range sinks {
		b.WriteString("_ = " + name + "\n")
	}
	b.WriteString("return nil\n}\n\nfunc main() {\n\t_ = run()\n}\n")
	return b.String()
}

// runGoBuild compiles src (a throwaway package-main file, written inside the
// module so `go build` resolves github.com/azex-ai/ledger and go.work
// normally) and fails the test with the generated source and compiler
// output on any error. Compile-only: no database, no execution.
func runGoBuild(t *testing.T, src string) {
	t.Helper()
	dir, err := os.MkdirTemp(".", "readmecompile-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	mainPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", os.DevNull, mainPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("README example failed to COMPILE:\n--- generated program ---\n%s\n--- output ---\n%s\n--- error ---\n%v",
			src, out, err)
	}
}

// readmeSnippetMinReasonLen is the shortest reason TestREADMEGoBlocksCompile
// UnlessMarkedSnippet accepts next to a readme-gate marker. Not a precise
// bar -- just enough to reject a bare `<!-- readme-gate: snippet -->` (no
// "-- reason" at all) or a one-word placeholder, both of which are "opted
// out" claims with nothing a future reader could check.
const readmeSnippetMinReasonLen = 15

// TestREADMEGoBlocksCompileUnlessMarkedSnippet is the fail-closed
// enumeration this repo uses elsewhere (postgres/grant_coverage_test.go's
// table classification, the sign gate's query exemptions): every ```go block
// in README.md is either run against a real database by one of the six
// tests above, marked `<!-- readme-gate: snippet -->` with a reason, or
// compiled here. A block that is none of the three is a gate failure by
// construction (it falls through to runGoBuild and, if the reassembled
// program does not compile, fails there) -- there is no fourth bucket to
// silently land in.
func TestREADMEGoBlocksCompileUnlessMarkedSnippet(t *testing.T) {
	blocks := readmeGoBlocksWithMarkers(t)

	anchored := map[string]bool{}
	for _, b := range blocks {
		fingerprint := blockFingerprint(b.text)

		runnable := false
		for _, anchor := range readmeRunnableAnchors {
			if strings.Contains(b.text, anchor) {
				anchored[anchor] = true
				runnable = true
			}
		}
		if runnable {
			// Covered end-to-end, against a real database, by its own
			// TestREADME*Runs test above -- compiling it again here would
			// just be a slower, weaker version of that check.
			continue
		}

		if b.marked {
			if len(b.reason) < readmeSnippetMinReasonLen {
				t.Errorf("README ```go block starting %q is marked %s but gives no (or too short a) "+
					"reason after \"-- \" -- working-agreements.md requires saying why, not just opting "+
					"out: got %q", fingerprint, readmeSnippetMarkerPrefix, b.reason)
			}
			continue
		}

		block := b.text
		t.Run(fingerprint, func(t *testing.T) {
			runGoBuild(t, compileSyntheticProgram(block))
		})
	}

	// Every run-anchor must still match a block: a README rewrite that moves
	// an anchored block must not leave its test silently pointing at
	// nothing. (extractREADMEGoBlock also Fatals in that case, but only when
	// that specific test runs, and only if it is not skipped for a missing
	// database.)
	for _, anchor := range readmeRunnableAnchors {
		if !anchored[anchor] {
			t.Errorf("runnable anchor %q matches no ```go block in README.md any more -- the test using it is pointing at nothing", anchor)
		}
	}
}

// TestREADMEGoBlocksParse gives every block -- the eighteen that are not run
// especially -- the cheapest real check there is: a snippet that does not
// parse cannot be right. A truncated line, an unbalanced brace or a mistyped
// keyword is red here without a type-checking harness for fragments that
// assume a dozen names from their surrounding prose.
//
// README blocks mix three shapes, sometimes in one block (the slog adapter
// block is an import, three method declarations and two statements), so the
// snippet is reassembled into a synthetic file: imports on top, top-level
// declarations after them, loose statements in a function body.
func TestREADMEGoBlocksParse(t *testing.T) {
	for _, block := range readmeGoBlocks(t) {
		fingerprint := blockFingerprint(block)
		synthetic := syntheticFileFor(block)
		if _, err := parser.ParseFile(token.NewFileSet(), "block.go", synthetic, parser.SkipObjectResolution); err != nil {
			t.Errorf("README ```go block starting %q does not parse as Go: %v\n\n--- reassembled as ---\n%s", fingerprint, err, synthetic)
		}
	}
}

// syntheticFileFor reassembles a README snippet into a parseable file.
//
// Line-based on purpose: the input is not a Go file, so there is nothing to
// parse it WITH until it has been sorted into the three buckets. A top-level
// line beginning with import/type/func/var/const opens a declaration, which
// runs to the line that closes its brace (or ends on its own line, for a
// one-liner); everything else is a statement.
func syntheticFileFor(block string) string {
	var imports, decls, stmts []string
	lines := strings.Split(block, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		keyword := ""
		for _, kw := range []string{"import", "type", "func", "var", "const"} {
			if strings.HasPrefix(line, kw+" ") || strings.HasPrefix(line, kw+"(") {
				keyword = kw
				break
			}
		}
		if keyword == "" {
			stmts = append(stmts, line)
			continue
		}
		decl := []string{line}
		if depth := braceDepth(line); depth > 0 {
			for i+1 < len(lines) {
				i++
				decl = append(decl, lines[i])
				depth += braceDepth(lines[i])
				if depth <= 0 {
					break
				}
			}
		}
		text := strings.Join(decl, "\n")
		if keyword == "import" {
			imports = append(imports, text)
		} else {
			decls = append(decls, text)
		}
	}

	var b strings.Builder
	b.WriteString("package p\n")
	for _, imp := range imports {
		b.WriteString(imp)
		b.WriteString("\n")
	}
	for _, decl := range decls {
		b.WriteString(decl)
		b.WriteString("\n")
	}
	b.WriteString("func _() {\n")
	b.WriteString(strings.Join(stmts, "\n"))
	b.WriteString("\n}\n")
	return b.String()
}

// braceDepth is the net number of braces/parens/brackets a line opens,
// ignoring those inside string or rune literals and after a line comment --
// enough to tell "this declaration continues" from "this one is complete".
func braceDepth(line string) int {
	depth := 0
	inString, inRune := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inString:
			switch c {
			case '\\':
				i++
			case '"':
				inString = false
			}
		case inRune:
			switch c {
			case '\\':
				i++
			case '\'':
				inRune = false
			}
		case c == '"':
			inString = true
		case c == '\'':
			inRune = true
		case c == '/' && i+1 < len(line) && line[i+1] == '/':
			return depth
		case c == '{' || c == '(' || c == '[':
			depth++
		case c == '}' || c == ')' || c == ']':
			depth--
		}
	}
	return depth
}
