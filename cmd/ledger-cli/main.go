// Command ledger-cli is a read-only investigation tool for ops + auditors.
//
// Connects directly to the ledger Postgres database (DATABASE_URL) and runs
// the same query interfaces the HTTP server uses. Useful for:
//
//   - Reconciliation triage (`ledger-cli reconcile --full`).
//   - Solvency check (`ledger-cli solvency --currency 1`).
//   - Trace a single booking end-to-end (`ledger-cli trace --booking-id 42`).
//   - List recent journals or events (`ledger-cli journals --since 1h`).
//   - Pull a balance snapshot for one account (`ledger-cli balance --holder 42 --currency 1`).
//
// Read-only by design: the CLI never posts journals or mutates state.
// For one-off corrections, use the HTTP API or write a migration.
//
// Build:
//
//	go build -o ledger-cli ./cmd/ledger-cli
//
// Examples:
//
//	export DATABASE_URL="postgres://user:pass@host:5432/ledger?sslmode=disable"
//	ledger-cli balance --holder 42 --currency 1 --class main_wallet
//	ledger-cli journals --limit 20
//	ledger-cli trace --booking-id 17
//	ledger-cli reconcile --full
//	ledger-cli solvency --currency 1
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/service"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

const usage = `ledger-cli — read-only ledger investigation tool

usage:
  ledger-cli <command> [flags]

commands:
  balance     show balance for one account dimension
  balances    show all balances for a holder
  journals    list recent journals
  journal     show one journal with entries
  trace       trace a booking through events and journals
  reconcile   run reconciliation checks
  solvency    show solvency report for a currency
  health      show system health metrics

env:
  DATABASE_URL   postgres connection string (required)

run "ledger-cli <command> -h" for command flags.
`

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}
	cmd, rest := args[0], args[1:]

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" && cmd != "help" && cmd != "-h" && cmd != "--help" {
		return fmt.Errorf("DATABASE_URL is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var pool *pgxpool.Pool
	var svc *ledger.Service
	if dbURL != "" {
		var err error
		pool, err = pgxpool.New(ctx, dbURL)
		if err != nil {
			return fmt.Errorf("pgxpool: %w", err)
		}
		defer pool.Close()
		svc, err = ledger.New(pool)
		if err != nil {
			return fmt.Errorf("ledger.New: %w", err)
		}
	}

	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	case "balance":
		return cmdBalance(ctx, svc, rest)
	case "balances":
		return cmdBalances(ctx, svc, rest)
	case "journals":
		return cmdJournals(ctx, svc, rest)
	case "journal":
		return cmdJournal(ctx, svc, rest)
	case "trace":
		return cmdTrace(ctx, svc, rest)
	case "reconcile":
		return cmdReconcile(ctx, svc, rest)
	case "solvency":
		return cmdSolvency(ctx, svc, rest)
	case "health":
		return cmdHealth(ctx, svc)
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
}

func cmdBalance(ctx context.Context, svc *ledger.Service, args []string) error {
	fs := flag.NewFlagSet("balance", flag.ExitOnError)
	holder := fs.Int64("holder", 0, "account holder (positive = user, negative = system)")
	currency := fs.Int64("currency", 0, "currency id")
	class := fs.String("class", "", "classification code (e.g. main_wallet)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *holder == 0 || *currency == 0 || *class == "" {
		return fmt.Errorf("--holder, --currency, --class are all required")
	}

	c, err := svc.Classifications().GetByCode(ctx, *class)
	if err != nil {
		return fmt.Errorf("classification %q: %w", *class, err)
	}
	bal, err := svc.BalanceReader().GetBalance(ctx, *holder, *currency, c.ID)
	if err != nil {
		return err
	}
	return jsonOut(map[string]any{
		"holder":         *holder,
		"currency_id":    *currency,
		"classification": *class,
		"balance":        bal.String(),
	})
}

func cmdBalances(ctx context.Context, svc *ledger.Service, args []string) error {
	fs := flag.NewFlagSet("balances", flag.ExitOnError)
	holder := fs.Int64("holder", 0, "account holder")
	currency := fs.Int64("currency", 0, "currency id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *holder == 0 || *currency == 0 {
		return fmt.Errorf("--holder and --currency are required")
	}
	bs, err := svc.BalanceReader().GetBalances(ctx, *holder, *currency)
	if err != nil {
		return err
	}
	return jsonOut(bs)
}

func cmdJournals(ctx context.Context, svc *ledger.Service, args []string) error {
	fs := flag.NewFlagSet("journals", flag.ExitOnError)
	cursor := fs.Int64("cursor", 0, "starting journal id (descending); 0 = latest")
	limit := fs.Int("limit", 20, "max journals to return")
	if err := fs.Parse(args); err != nil {
		return err
	}
	js, err := svc.Queries().ListJournals(ctx, *cursor, int32(*limit))
	if err != nil {
		return err
	}
	return jsonOut(js)
}

func cmdJournal(ctx context.Context, svc *ledger.Service, args []string) error {
	fs := flag.NewFlagSet("journal", flag.ExitOnError)
	id := fs.Int64("id", 0, "journal id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == 0 {
		return fmt.Errorf("--id is required")
	}
	j, entries, err := svc.Queries().GetJournal(ctx, *id)
	if err != nil {
		return err
	}
	return jsonOut(map[string]any{"journal": j, "entries": entries})
}

func cmdTrace(ctx context.Context, svc *ledger.Service, args []string) error {
	fs := flag.NewFlagSet("trace", flag.ExitOnError)
	bookingID := fs.Int64("booking-id", 0, "booking id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *bookingID == 0 {
		return fmt.Errorf("--booking-id is required")
	}
	tr, err := svc.Audit().TraceBooking(ctx, *bookingID)
	if err != nil {
		return err
	}
	return jsonOut(tr)
}

func cmdReconcile(ctx context.Context, svc *ledger.Service, args []string) error {
	fs := flag.NewFlagSet("reconcile", flag.ExitOnError)
	full := fs.Bool("full", false, "run all 10 reconcile checks; default is just the global accounting equation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*full {
		// Stick to global reconciliation through the service-level helper.
		return fmt.Errorf("--full is currently the only supported mode")
	}
	cfg := service.FullReconciliationConfig{}
	rec := svc.FullReconciler(cfg)
	report, err := rec.RunFullReconciliation(ctx)
	if err != nil {
		return err
	}
	return jsonOut(report)
}

func cmdSolvency(ctx context.Context, svc *ledger.Service, args []string) error {
	fs := flag.NewFlagSet("solvency", flag.ExitOnError)
	currency := fs.Int64("currency", 0, "currency id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *currency == 0 {
		return fmt.Errorf("--currency is required")
	}
	report, err := svc.SolvencyChecker().SolvencyCheck(ctx, *currency)
	if err != nil {
		return err
	}
	return jsonOut(report)
}

func cmdHealth(ctx context.Context, svc *ledger.Service) error {
	hm, err := svc.Queries().GetHealthMetrics(ctx)
	if err != nil {
		return err
	}
	return jsonOut(hm)
}

func jsonOut(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// keep imported types referenced so the file fails fast if any drift.
var _ = strings.TrimSpace
var _ = core.NormalSideDebit
