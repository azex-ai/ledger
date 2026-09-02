package ledger_test

import (
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
	block := stripLeadingImport(extractREADMEGoBlock(t, "j, err := svc.JournalWriter().PostJournal(ctx, core.JournalInput{"))

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
	block := extractREADMEGoBlock(t, `// 8 bundles, see "Built-in Presets" below`)

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
	block := extractREADMEGoBlock(t, `"kyc_review"`)

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
	block := extractREADMEGoBlock(t, `"promotion_credit"`)

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
	block := extractREADMEGoBlock(t, `CreateJournalType(ctx, core.JournalTypeInput{`)

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
