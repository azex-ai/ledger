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

	// The block below also calls ledger.Migrate(dbURL) itself (its first
	// statement, verbatim from README) -- that is fine, Migrate is
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

// --- M-12: every ```go block in README.md is classified ---
//
// The tests above run six blocks. README has twenty-four, and the other
// eighteen had no runtime gate of any kind -- which is how the reviewer's
// mutation of the promo_grant recipe (a second leg reading an AmountKey no
// caller supplies: compiles, creates, fails on use) went unnoticed. E-M2
// fixed one block; nothing covered the CLASS.
//
// This is the fail-closed enumeration this repo uses elsewhere
// (postgres/grant_coverage_test.go's table classification, the sign gate's
// query exemptions): every block must either contain a runnable anchor, or
// be classified below with the reason it is not executed. A new block
// defaults to red.
//
// Blocks that are not executed still get the cheapest real check available:
// they must PARSE as Go. That catches a truncated or mistyped snippet without
// a type-checking harness for fragments that assume a dozen names.

// readmeBlockExemptions maps a block's fingerprint -- its first line of code
// -- to why the block is not run against a database.
//
// "Needs a live X" is a reason. "Nobody got around to it" is not: the three
// entries marked FOLLOW-UP say what it would take, so the list can shrink
// deliberately.
var readmeBlockExemptions = map[string]string{
	"import (": "an import list, not a program",
	`import "github.com/azex-ai/ledger/presets"`:                                                     "an import line",
	`import "log/slog"`:                                                                              "an import line",
	`import "github.com/azex-ai/ledger/observability"`:                                               "an import line",
	`import ledgerotel "github.com/azex-ai/ledger/pkg/otel"`:                                         "an import line",
	"worker := svc.Worker(service.DefaultWorkerConfig())":                                            "starts a background worker with its own goroutines and tickers; a run would have to own its lifecycle, and service/worker_test.go already covers the worker",
	"srv, err := server.NewFromDeps(cfg, server.Deps{":                                               "builds an http.Handler and expects the caller to serve it; server/*_test.go drives the real router through httptest instead",
	"svc.InstallDefaultPresets(ctx)    // Deposit + Withdrawal only":                                 "one-line preset install, exercised by presets/*_test.go and by the Tier 2 block that is run",
	"presets.InstallPendingBundle(ctx, svc.Classifications(), svc.JournalTypes(), svc.Templates())":  "same: a bundle install line, covered by presets/*_test.go",
	"presets.DepositLifecycle      // pending → confirming → confirmed | failed | expired":           "a list of exported lifecycle values, not statements",
	"svc.JournalWriter().PostJournal(ctx, core.JournalInput{":                                        "the same call the Tier 1 block makes, shown without its surrounding boot; running it would duplicate TestREADMETier1QuickStartRuns",
	`svc.JournalWriter().ExecuteTemplate(ctx, "fee_charge", core.TemplateParams{`:                    "FOLLOW-UP: runnable once the fee bundle's seed is factored out of the Tier 2 harness -- it needs the fee_charge template installed and a funded holder",
	"svc.TemplateBatchExecutor().ExecuteTemplateBatch(ctx, []core.TemplateExecutionRequest{":         "FOLLOW-UP: same seed as the fee_charge block above, plus a second template",
	"type StripeAdapter struct{ secret string }":                                                     "a channel.Adapter implementation sketch: the interface it satisfies is pinned by channel/*_test.go, and running it would need an inbound HTTP request with a real signature",
	"err := svc.RunInTx(ctx, func(tx *ledger.Service) error {":                                       "FOLLOW-UP: runnable with the Tier 1 seed; examples/tx-compose is the executed version of this recipe today",
	`key := ledger.NewIdempotencyKey("deposit")`:                                                     "a key-construction line; idempotency_test.go covers the helper",
	`err := ledger.RetryIdempotent(ctx, "deposit", 3, func(ctx context.Context, key string) error {`: "retries a caller-supplied closure; idempotency_test.go drives the helper with a failing closure, which a README run could not do without inventing one",
	"func TestMyAnchorConformance(t *testing.T) {":                                                   "a test a CONSUMER writes against anchortest.RunConformance; anchortest/conformance.go is exercised by anchors/r2 and anchordev",
}

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
// edits to the rest of the block, and readable in an exemption table.
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

func TestREADMEGoBlocksAreAllClassified(t *testing.T) {
	blocks := readmeGoBlocks(t)

	anchored := map[string]bool{}
	usedExemptions := map[string]bool{}
	for _, block := range blocks {
		fingerprint := blockFingerprint(block)

		runnable := false
		for _, anchor := range readmeRunnableAnchors {
			if strings.Contains(block, anchor) {
				anchored[anchor] = true
				runnable = true
			}
		}
		if runnable {
			continue
		}
		if _, ok := readmeBlockExemptions[fingerprint]; ok {
			usedExemptions[fingerprint] = true
			continue
		}
		t.Errorf("README ```go block starting %q is neither run by a test in this file nor classified in readmeBlockExemptions.\n\n"+
			"A block with no runtime gate can compile, be wrong, and stay wrong: the promo_grant recipe's second leg read an AmountKey "+
			"no caller supplies, which only fails when the template is USED (M-12). Either add a runnable test (the six above are the "+
			"shape) or add an entry saying why this block cannot be executed.", fingerprint)
	}

	// Every anchor must still match a block: a README rewrite that moves an
	// anchored block must not leave its test silently pointing at nothing.
	// (extractREADMEGoBlock also Fatals in that case, but only when that
	// test runs, and only if it is not skipped for a missing database.)
	for _, anchor := range readmeRunnableAnchors {
		if !anchored[anchor] {
			t.Errorf("runnable anchor %q matches no ```go block in README.md any more -- the test using it is pointing at nothing", anchor)
		}
	}
	for fingerprint, reason := range readmeBlockExemptions {
		if !usedExemptions[fingerprint] {
			t.Errorf("stale readmeBlockExemptions entry %q (%s): no README block starts with that line any more -- delete it (or update it, if the block was edited)", fingerprint, reason)
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
			if c == '\\' {
				i++
			} else if c == '"' {
				inString = false
			}
		case inRune:
			if c == '\\' {
				i++
			} else if c == '\'' {
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
