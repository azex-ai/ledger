// Command ledger-cli is an investigation tool for ops + auditors, read-only
// with two narrow, explicit exceptions (I-M6):
//
//   - `reconcile --full` persists its resume cursor to
//     reconcile_scan_cursors (service.FullReconciliationService's check #2
//     lap tracking) -- it is not a pure read, and DR / forensic use against
//     a suspect or restored database must run it against a CLONE, never the
//     database being examined, exactly as any other write tool would
//     (docs/DR.md).
//   - `rollup reset-claim` is one of the two operator write actions this
//     tool exposes on purpose (B-m10): there is no other way to un-stick a
//     rollup_queue item that exhausted its retry budget.
//   - `reorgs resolve` is the other (M-6): a deposit chain anomaly is closed
//     out only by an operator's explicit resolution, and until this command
//     existed the only way to do it was hand-written UPDATE -- so the
//     library's own recheck loop re-alerted on every tick forever after
//     on-call had already dealt with it, an alarm nailed to ON.
//
// Every other command only reads.
//
// Connects directly to the ledger Postgres database (DATABASE_URL) and runs
// the same query interfaces the HTTP server uses. Useful for:
//
//   - Reconciliation triage (`ledger-cli reconcile --full`).
//   - Solvency check (`ledger-cli solvency --currency <uid>`).
//   - Trace a single booking end-to-end (`ledger-cli trace --booking-uid <uid>`).
//   - List recent journals or events (`ledger-cli journals --limit 20`).
//   - Pull a balance snapshot for one account (`ledger-cli balance --holder 42 --currency <uid>`).
//   - Un-stick a rollup queue item (`ledger-cli rollup reset-claim --id <id>`).
//   - Triage deposits the ledger refused to book (`ledger-cli dead-letters list`).
//   - Work the deposit chain-anomaly queue (`ledger-cli reorgs list` / `reorgs resolve`).
//
// Build:
//
//	go build -o ledger-cli ./cmd/ledger-cli
//
// Examples:
//
//	export DATABASE_URL="postgres://user:pass@host:5432/ledger?sslmode=disable"
//	ledger-cli balance --holder 42 --currency <uid> --class main_wallet
//	ledger-cli journals --limit 20
//	ledger-cli trace --booking-uid <uid>
//	ledger-cli reconcile --full
//	ledger-cli reconcile --full --pubkey-hex <hex> --key-id <id>  # cover unauthorized_journals too
//	ledger-cli solvency --currency <uid>
//	ledger-cli currencies
//	ledger-cli classifications
//	ledger-cli rollup reset-claim --id <id>
//	ledger-cli dead-letters list --limit 20
//	ledger-cli dead-letters show --uid <uid>
//	ledger-cli reorgs list
//	ledger-cli reorgs resolve --booking-uid <uid> --kind deep_reorg --note "reversed per RUNBOOK 12"
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/anchordev"
	"github.com/azex-ai/ledger/authdev"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// reconcileFullFlagUsage is the --full flag's help text, pulled out to a
// named constant (rather than an inline literal in cmdReconcile) so it can
// be pinned by test without invoking the flag package's ExitOnError parsing
// path. See TestReconcileFullFlagUsage_DoesNotHardcodeACheckCount.
const reconcileFullFlagUsage = "run the full reconcile check suite (see the report's checks[] array for exactly which checks ran); default is just the global accounting equation"

const usage = `ledger-cli — ledger investigation tool (read-only except reconcile --full's resume cursor, rollup reset-claim and reorgs resolve; see package doc)

usage:
  ledger-cli <command> [flags]

commands:
  balance         show balance for one account dimension
  balances        show all balances for a holder
  journals        list recent journals
  journal         show one journal with entries
  trace           trace a booking through events and journals
  reconcile       run reconciliation checks
  solvency        show solvency report for a currency
  trial-balance   show trial balance report for a currency
  health          show system health metrics
  verify          verify the P6 batch attestation chain + a P5 journal signature sample
  currencies      list currencies
  classifications list classifications
  rollup          rollup queue admin (reset-claim)
  dead-letters    deposits the ledger refused to book, and never scanned again (list, show)
  reorgs          deposit chain anomalies awaiting an operator's close-out (list, resolve)
  config-history  forensic trail: who changed account policies / config tables / reconcile scan cursors, and when

env:
  DATABASE_URL   postgres connection string (required)

flags common to every command:
  --timeout   overrides this command's default context deadline (Go duration, e.g. 90s, 10m)

run "ledger-cli <command> -h" for command flags.
`

// defaultTimeoutForCmd returns the context deadline a command gets when
// --timeout is not given (I-M7). The 30s the whole CLI used to share was
// below service.FullReconciliationConfig's own default Check2Timeout (2m),
// so a large fleet's `reconcile --full` was cancelled by the CLI's own
// context before a single check finished, producing a report full of
// timeout-shaped failures that had nothing to do with the ledger's health.
// Commands whose underlying check(s) can legitimately run for minutes get a
// longer default; simple point lookups keep the original budget.
func defaultTimeoutForCmd(cmd string) time.Duration {
	switch cmd {
	case "reconcile", "verify", "solvency", "trial-balance":
		return 10 * time.Minute
	default:
		return 30 * time.Second
	}
}

// scanFlagValue looks up --name / --name=value / -name / -name=value
// anywhere in args, without consuming or validating the rest of args --
// each subcommand's own flag.FlagSet still parses (and validates) the same
// flag normally. Used for the handful of values (--timeout, --pubkey-hex,
// --key-id) needed before a subcommand's FlagSet exists yet: --timeout
// determines the context passed into every cmdXxx function, and
// --pubkey-hex/--key-id determine whether ledger.New is given a verifier
// (reconcile's unauthorized_journals check needs one wired in at
// construction time -- see I-R2).
func scanFlagValue(args []string, name string) string {
	eq1, eq2 := "--"+name+"=", "-"+name+"="
	for i, a := range args {
		switch {
		case strings.HasPrefix(a, eq1):
			return strings.TrimPrefix(a, eq1)
		case strings.HasPrefix(a, eq2):
			return strings.TrimPrefix(a, eq2)
		case (a == "--"+name || a == "-"+name) && i+1 < len(args):
			return args[i+1]
		}
	}
	return ""
}

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

	timeout := defaultTimeoutForCmd(cmd)
	if raw := scanFlagValue(rest, "timeout"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("--timeout: %w", err)
		}
		timeout = parsed
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// reconcile's unauthorized_journals check (contracts §W2-2) only runs
	// when this Service was built WithAttestor's verifier half -- ledger-cli
	// otherwise has no way to reach it at all (I-R2), leaving
	// FullCoverage permanently false for every CLI-driven run regardless of
	// how the ledger itself is configured.
	var svcOpts []ledger.Option
	if cmd == "reconcile" {
		pubkeyHex := scanFlagValue(rest, "pubkey-hex")
		keyID := scanFlagValue(rest, "key-id")
		if pubkeyHex != "" && keyID != "" {
			pubkeyBytes, err := hex.DecodeString(pubkeyHex)
			if err != nil {
				return fmt.Errorf("--pubkey-hex: %w", err)
			}
			if len(pubkeyBytes) != ed25519.PublicKeySize {
				return fmt.Errorf("--pubkey-hex must decode to %d bytes, got %d", ed25519.PublicKeySize, len(pubkeyBytes))
			}
			verifier := authdev.NewLocalVerifier(ed25519.PublicKey(pubkeyBytes), keyID)
			svcOpts = append(svcOpts, ledger.WithAttestor(nil, verifier))
		}
	}

	var pool *pgxpool.Pool
	var svc *ledger.Service
	if dbURL != "" {
		var err error
		pool, err = pgxpool.New(ctx, dbURL)
		if err != nil {
			return fmt.Errorf("pgxpool: %w", err)
		}
		defer pool.Close()
		svc, err = ledger.New(pool, svcOpts...)
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
	case "trial-balance":
		return cmdTrialBalance(ctx, svc, rest)
	case "health":
		return cmdHealth(ctx, svc)
	case "verify":
		return cmdVerify(ctx, pool, svc, rest)
	case "currencies":
		return cmdCurrencies(ctx, svc, rest)
	case "classifications":
		return cmdClassifications(ctx, svc, rest)
	case "rollup":
		return cmdRollup(ctx, pool, rest)
	case "dead-letters":
		return cmdDeadLetters(ctx, pool, rest)
	case "reorgs":
		return cmdReorgs(ctx, pool, rest)
	case "config-history":
		return cmdConfigHistory(ctx, svc, rest)
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
}

func cmdBalance(ctx context.Context, svc *ledger.Service, args []string) error {
	fs := flag.NewFlagSet("balance", flag.ExitOnError)
	holder := fs.Int64("holder", 0, "account holder (positive = user, negative = system)")
	currency := fs.String("currency", "", "currency uid")
	class := fs.String("class", "", "classification code (e.g. main_wallet)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *holder == 0 || *currency == "" || *class == "" {
		return fmt.Errorf("--holder, --currency, --class are all required")
	}

	c, err := svc.Classifications().GetByCode(ctx, *class)
	if err != nil {
		return fmt.Errorf("classification %q: %w", *class, err)
	}
	bal, err := svc.BalanceReader().GetBalance(ctx, *holder, *currency, c.UID)
	if err != nil {
		return err
	}
	return jsonOut(map[string]any{
		"holder":         *holder,
		"currency_uid":   *currency,
		"classification": *class,
		"balance":        bal.String(),
	})
}

func cmdBalances(ctx context.Context, svc *ledger.Service, args []string) error {
	fs := flag.NewFlagSet("balances", flag.ExitOnError)
	holder := fs.Int64("holder", 0, "account holder")
	currency := fs.String("currency", "", "currency uid")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *holder == 0 || *currency == "" {
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
	cursor := fs.String("cursor", "", "opaque page cursor from a previous run; empty = latest")
	limit := fs.Int("limit", 20, "max journals to return")
	if err := fs.Parse(args); err != nil {
		return err
	}
	js, nextCursor, err := svc.Queries().ListJournals(ctx, *cursor, int32(*limit))
	if err != nil {
		return err
	}
	return jsonOut(map[string]any{"list": js, "next_cursor": nextCursor})
}

func cmdJournal(ctx context.Context, svc *ledger.Service, args []string) error {
	fs := flag.NewFlagSet("journal", flag.ExitOnError)
	uid := fs.String("uid", "", "journal uid")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *uid == "" {
		return fmt.Errorf("--uid is required")
	}
	j, entries, err := svc.Queries().GetJournal(ctx, *uid)
	if err != nil {
		return err
	}
	return jsonOut(map[string]any{"journal": j, "entries": entries})
}

func cmdTrace(ctx context.Context, svc *ledger.Service, args []string) error {
	fs := flag.NewFlagSet("trace", flag.ExitOnError)
	bookingUID := fs.String("booking-uid", "", "booking uid")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *bookingUID == "" {
		return fmt.Errorf("--booking-uid is required")
	}
	tr, err := svc.Audit().TraceBooking(ctx, *bookingUID)
	if err != nil {
		return err
	}
	return jsonOut(tr)
}

func cmdReconcile(ctx context.Context, svc *ledger.Service, args []string) error {
	fs := flag.NewFlagSet("reconcile", flag.ExitOnError)
	full := fs.Bool("full", false, reconcileFullFlagUsage)
	// pubkey-hex/key-id are read by run() (via scanFlagValue) BEFORE this
	// FlagSet exists, because they must be known at ledger.New time (I-R2).
	// Declared here too, unused, purely so `-h` documents them and Parse
	// does not choke on an unrecognized flag.
	_ = fs.String("pubkey-hex", "", "hex-encoded ed25519 public key -- when given with --key-id, covers the unauthorized_journals check too")
	_ = fs.String("key-id", "", "the key id --pubkey-hex corresponds to")
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
	if err := jsonOut(report); err != nil {
		return err
	}
	if !report.FullCoverage {
		// I-R2: point at the reason instead of leaving the operator to find
		// it themselves inside checks[].findings -- most commonly this means
		// --pubkey-hex/--key-id were not given, so unauthorized_journals had
		// no core.AuthVerifier to run with (see that check's own finding for
		// the exact reason in THIS report).
		fmt.Fprintln(os.Stderr, "note: full_coverage=false -- see the checks[] entry whose findings explain why "+
			"(commonly: pass --pubkey-hex and --key-id to also cover unauthorized_journals)")
	}
	return nil
}

func cmdSolvency(ctx context.Context, svc *ledger.Service, args []string) error {
	fs := flag.NewFlagSet("solvency", flag.ExitOnError)
	currency := fs.String("currency", "", "currency uid")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *currency == "" {
		return fmt.Errorf("--currency is required")
	}
	report, err := svc.SolvencyChecker().SolvencyCheck(ctx, *currency)
	if err != nil {
		return err
	}
	return jsonOut(report)
}

func cmdTrialBalance(ctx context.Context, svc *ledger.Service, args []string) error {
	fs := flag.NewFlagSet("trial-balance", flag.ExitOnError)
	currency := fs.String("currency", "", "currency uid")
	asOf := fs.String("as-of", "", "RFC3339 cutoff (inclusive); default now")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *currency == "" {
		return fmt.Errorf("--currency is required")
	}

	cutoff := time.Now()
	if *asOf != "" {
		parsed, err := time.Parse(time.RFC3339, *asOf)
		if err != nil {
			return fmt.Errorf("--as-of must be RFC3339: %w", err)
		}
		cutoff = parsed
	}

	report, err := svc.TrialBalanceReader().TrialBalance(ctx, *currency, cutoff)
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

// cmdCurrencies lists currencies (I-M4: `solvency --currency` and
// `balance --currency` both take a currency UID, not a numeric id -- this
// is where an operator who only has the id finds the uid).
func cmdCurrencies(ctx context.Context, svc *ledger.Service, args []string) error {
	fs := flag.NewFlagSet("currencies", flag.ExitOnError)
	activeOnly := fs.Bool("active-only", false, "only list active currencies")
	if err := fs.Parse(args); err != nil {
		return err
	}
	list, err := svc.Currencies().ListCurrencies(ctx, *activeOnly)
	if err != nil {
		return err
	}
	return jsonOut(list)
}

// cmdClassifications lists classifications (I-M4, same reasoning as
// cmdCurrencies: `balance --class` takes a code, but the uid this prints
// alongside it is what other commands and the HTTP API expect elsewhere).
func cmdClassifications(ctx context.Context, svc *ledger.Service, args []string) error {
	fs := flag.NewFlagSet("classifications", flag.ExitOnError)
	activeOnly := fs.Bool("active-only", false, "only list active classifications")
	if err := fs.Parse(args); err != nil {
		return err
	}
	list, err := svc.Classifications().ListClassifications(ctx, *activeOnly)
	if err != nil {
		return err
	}
	return jsonOut(list)
}

// cmdRollup is the rollup_queue admin surface (B-m10). reset-claim is the
// only write action anywhere in this CLI: a rollup_queue item with
// failed_attempts >= 10 is excluded from DequeueRollupBatch forever
// (postgres/sql/queries/checkpoints.sql) and, before this, had no path back
// in short of hand-written SQL. Takes *pgxpool.Pool directly rather than
// *ledger.Service because core.Metrics.StuckRollups' write counterpart
// (ResetRollupClaim) is a postgres.RollupAdapter method, not something the
// facade exposes -- ledger.Service intentionally has no admin/write
// surface beyond the ordinary financial operations every consumer already
// has (see CLAUDE.md's package contract on postgres.* direct use from this
// binary).
func cmdRollup(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ledger-cli rollup reset-claim --id <rollup_queue.id>")
	}
	switch args[0] {
	case "reset-claim":
		fs := flag.NewFlagSet("rollup reset-claim", flag.ExitOnError)
		id := fs.Int64("id", 0, "rollup_queue row id to reset (see docs/RUNBOOK.md's stuck-rollup-items section)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *id == 0 {
			return fmt.Errorf("--id is required")
		}
		adapter := postgres.NewRollupAdapter(pool)
		if err := adapter.ResetRollupClaim(ctx, *id); err != nil {
			return err
		}
		return jsonOut(map[string]any{"reset": true, "id": *id})
	default:
		return fmt.Errorf("unknown rollup subcommand %q (want: reset-claim)", args[0])
	}
}

// cmdDeadLetters is the read side of the deposit-ingest dead-letter queue
// (docs/RUNBOOK.md §18). A dead letter is a transfer that is ON CHAIN, to a
// registered address, in a whitelisted token, that the ingestion path
// refused and the forward scan then moved past: no booking exists, so no
// recheck loop revisits it, and the cursor is past its block, so no scan
// sees it again. Until this command existed the table had no reader at all
// -- `ListDeadLetters`' own doc comment said "for on-call triage (RUNBOOK)"
// while nothing called it and the runbook had no such section (C-2).
//
// Read-only on purpose, and this is the one place in this tool where a
// missing verb is a deliberate design decision rather than an omission:
// REPLAYING a dead letter calls IngestDeposit, which needs the chain set --
// a token's currency code and its auto-credit ceilings live in the
// consumer's Go composition root, not in the database. This binary holds
// only DATABASE_URL, so it could only offer a replay by asking the operator
// to re-type the mint bounds, which puts the money fence on the wrong side
// of the keyboard. The replay is therefore served by the process that
// already holds the configuration: POST
// /api/v1/deposits/dead-letters/{uid}/replay, or
// service.Onchain.ReplayDeadLetter in library mode.
//
// Takes *pgxpool.Pool directly rather than *ledger.Service for the same
// reason cmdRollup does: this is an ops store, not part of the facade's
// financial surface.
func cmdDeadLetters(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	const usage = `usage:
  ledger-cli dead-letters list [--limit N] [--cursor C] [--unbooked-only]
  ledger-cli dead-letters show --uid <uid>

to replay one after fixing its cause (needs the chain config, so not this tool):
  POST /api/v1/deposits/dead-letters/{uid}/replay   (capability: deposit_review)
  or service.Onchain.ReplayDeadLetter(ctx, uid)     (library mode)`
	if len(args) == 0 {
		return errors.New(usage)
	}
	store := postgres.NewIngestDeadLetterStore(pool)
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("dead-letters list", flag.ExitOnError)
		limit := fs.Int("limit", 20, "max rows to return")
		cursor := fs.String("cursor", "", "opaque page cursor from a previous run; empty = newest page")
		unbookedOnly := fs.Bool("unbooked-only", false, "only rows whose deposit still has no booking -- the ones that still need action")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		letters, nextCursor, err := store.ListDeadLetters(ctx, *cursor, int32(*limit))
		if err != nil {
			return err
		}
		if *unbookedOnly {
			kept := make([]core.IngestDeadLetter, 0, len(letters))
			for _, dl := range letters {
				if !dl.Booked {
					kept = append(kept, dl)
				}
			}
			letters = kept
		}
		return jsonOut(map[string]any{"list": letters, "next_cursor": nextCursor})
	case "show":
		fs := flag.NewFlagSet("dead-letters show", flag.ExitOnError)
		uid := fs.String("uid", "", "dead letter uid (from `dead-letters list`)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *uid == "" {
			return errors.New("--uid is required")
		}
		dl, err := store.GetDeadLetter(ctx, *uid)
		if err != nil {
			return err
		}
		return jsonOut(dl)
	default:
		return fmt.Errorf("unknown dead-letters subcommand %q\n\n%s", args[0], usage)
	}
}

// cmdReorgs is the operator surface over deposit chain anomalies
// (core.DepositReorg, docs/RUNBOOK.md §12): a confirmed deposit whose
// transaction left the canonical chain, and a deposit this watcher failed as
// a shallow reorg whose transaction came back.
//
// `resolve` is the second write action in this tool (see the package doc).
// It exists because the anomaly queue is deliberately NOT self-clearing:
// after on-call reverses a deep-reorged credit the transaction is still off
// chain, so the recheck loop still finds the anomaly true and re-alerts on
// every tick -- forever. postgres.DepositReorgStore.ResolveReorg was the
// only way off that queue and had zero callers anywhere, no endpoint and no
// command, which left "hand-written UPDATE" as the documented-nowhere
// procedure (M-6).
func cmdReorgs(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	const usage = `usage:
  ledger-cli reorgs list [--limit N]
  ledger-cli reorgs resolve --booking-uid <uid> --kind <deep_reorg|shallow_reorg_failed> --note "<what you did>"`
	if len(args) == 0 {
		return errors.New(usage)
	}
	store := postgres.NewDepositReorgStore(pool)
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("reorgs list", flag.ExitOnError)
		limit := fs.Int("limit", 50, "max rows to return")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		list, err := store.ListOpenReorgs(ctx, int32(*limit))
		if err != nil {
			return err
		}
		return jsonOut(map[string]any{"list": list})
	case "resolve":
		fs := flag.NewFlagSet("reorgs resolve", flag.ExitOnError)
		bookingUID := fs.String("booking-uid", "", "the anomaly's booking uid (from `reorgs list`)")
		kind := fs.String("kind", "", "deep_reorg | shallow_reorg_failed")
		note := fs.String("note", "", "what was done about it -- recorded on the row, required")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *bookingUID == "" || *kind == "" {
			return errors.New("--booking-uid and --kind are required")
		}
		if *note == "" {
			// The note is the only record of WHY an alert stopped firing.
			// Closing an anomaly with no explanation is how "we looked at
			// it" becomes indistinguishable from "somebody silenced it".
			return errors.New("--note is required: it is the only record of what was done about this anomaly")
		}
		if *kind != core.ReorgKindDeepReorg && *kind != core.ReorgKindShallowReorgFailed {
			return fmt.Errorf("--kind must be %q or %q", core.ReorgKindDeepReorg, core.ReorgKindShallowReorgFailed)
		}
		if err := store.ResolveReorg(ctx, *kind, *bookingUID, *note); err != nil {
			return err
		}
		return jsonOut(map[string]any{"resolved": true, "booking_uid": *bookingUID, "kind": *kind, "resolution": *note})
	default:
		return fmt.Errorf("unknown reorgs subcommand %q\n\n%s", args[0], usage)
	}
}

// cmdConfigHistory is the operator-facing read side of the forensic trail
// D-threat built (core.ConfigChangeReader, migration 006's
// config_table_changes / reconcile_scan_cursor_changes / account_policy_changes):
// a ledger that records tampering evidence into a table with no query, no
// store method, and no command an on-call engineer can actually run has not
// made tampering visible (docs/RUNBOOK.md's own "配置篡改取证" section
// documents this command's exact flags; keep the two in sync).
//
// Exactly one of --table / --check / --holder selects which of the three
// underlying trails to query -- they are different tables answering
// different questions (which config table changed / which reconcile check's
// scan cursor moved / whose account policy changed), not one table filtered
// three ways.
func cmdConfigHistory(ctx context.Context, svc *ledger.Service, args []string) error {
	fs := flag.NewFlagSet("config-history", flag.ExitOnError)
	table := fs.String("table", "", "config_table_changes.table_name to filter on (e.g. account_policies, classifications)")
	check := fs.String("check", "", "reconcile_scan_cursor_changes.check_name to filter on (e.g. checkpoint_balance)")
	holder := fs.Int64("holder", 0, "account_policy_changes.account_holder to filter on")
	since := fs.String("since", "", "lower bound, inclusive: RFC3339 timestamp or <N>d (e.g. 30d, 7d)")
	until := fs.String("until", "", "upper bound, inclusive: RFC3339 timestamp or <N>d")
	cursor := fs.String("cursor", "", "opaque page cursor from a previous run; empty = latest")
	limit := fs.Int("limit", 50, "max rows to return")
	if err := fs.Parse(args); err != nil {
		return err
	}

	selected := 0
	for _, v := range []string{*table, *check} {
		if v != "" {
			selected++
		}
	}
	if *holder != 0 {
		selected++
	}
	if selected != 1 {
		return fmt.Errorf("exactly one of --table, --check, --holder is required (they query three different trails, not one filtered three ways)")
	}

	sinceAt, err := parseHistoryBound(*since)
	if err != nil {
		return fmt.Errorf("--since: %w", err)
	}
	untilAt, err := parseHistoryBound(*until)
	if err != nil {
		return fmt.Errorf("--until: %w", err)
	}
	filter := core.ConfigChangeFilter{
		TableName:     *table,
		CheckName:     *check,
		AccountHolder: *holder,
		Since:         sinceAt,
		Until:         untilAt,
		Cursor:        *cursor,
		Limit:         int32(*limit),
	}

	reader := svc.ConfigHistory()
	switch {
	case *table != "":
		list, nextCursor, err := reader.ListConfigChanges(ctx, filter)
		if err != nil {
			return err
		}
		return jsonOut(map[string]any{"list": list, "next_cursor": nextCursor})
	case *check != "":
		list, nextCursor, err := reader.ListScanCursorChanges(ctx, filter)
		if err != nil {
			return err
		}
		return jsonOut(map[string]any{"list": list, "next_cursor": nextCursor})
	default: // --holder
		list, nextCursor, err := reader.ListAccountPolicyChanges(ctx, filter)
		if err != nil {
			return err
		}
		return jsonOut(map[string]any{"list": list, "next_cursor": nextCursor})
	}
}

// parseHistoryBound parses --since/--until: "" (no bound), "<N>d" (N days
// ago from now -- the RUNBOOK's documented shorthand for the common case of
// an on-call engineer bounding a forensic query to a recent window), or an
// absolute RFC3339 timestamp.
func parseHistoryBound(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return time.Time{}, fmt.Errorf("%q: want <N>d (e.g. 30d) or RFC3339: %w", s, err)
		}
		return time.Now().AddDate(0, 0, -n), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q: want <N>d (e.g. 30d) or RFC3339: %w", s, err)
	}
	return t, nil
}

// cmdVerify runs the P6 five-step verification (design doc §8.4):
// external anchor head, attestation chain continuity/signatures, batch
// digest recomputation, and a sample of P5 per-journal signatures.
// NOT_RUN, never a partial VERIFIED, when the public key or anchor file
// aren't supplied -- see service.VerifyLedger's doc comment.
func cmdVerify(ctx context.Context, pool *pgxpool.Pool, svc *ledger.Service, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	anchorFile := fs.String("anchor-file", "", "path to the local-file Anchor's state (see anchordev.LocalFileAnchor) -- required")
	pubkeyHex := fs.String("pubkey-hex", "", "hex-encoded ed25519 public key used for both P6 attestation and P5 journal signatures -- required")
	keyID := fs.String("key-id", "", "the key id this public key corresponds to -- required")
	sampleSize := fs.Int("sample-size", 20, "how many of the most recent journals to sample for a valid P5 signature")
	referenceDir := fs.String("reference-dir", "", "optional: directory of seq-<N>.json trusted-reference entry dumps (design doc §9.1 localization) -- when a TAMPERED seq has a matching file here, the report's mismatched_entry_ids names the exact entries; without it, TAMPERED seqs are still reported, just without an entry list")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *anchorFile == "" || *pubkeyHex == "" || *keyID == "" {
		// Fail fast here rather than let VerifyLedger report NOT_RUN for a
		// pure CLI usage error -- these three flags are unconditionally
		// required, not a "maybe missing" runtime condition.
		return fmt.Errorf("--anchor-file, --pubkey-hex, and --key-id are all required")
	}
	pubkeyBytes, err := hex.DecodeString(*pubkeyHex)
	if err != nil {
		return fmt.Errorf("--pubkey-hex: %w", err)
	}
	if len(pubkeyBytes) != ed25519.PublicKeySize {
		return fmt.Errorf("--pubkey-hex must decode to %d bytes, got %d", ed25519.PublicKeySize, len(pubkeyBytes))
	}

	anchor := anchordev.NewLocalFileAnchorForDevelopment(*anchorFile)
	verifier := authdev.NewLocalVerifier(ed25519.PublicKey(pubkeyBytes), *keyID)
	store := postgres.NewAttestationStore(pool)

	cfg := service.VerifyConfig{JournalSampleSize: int32(*sampleSize)}
	if *referenceDir != "" {
		cfg.ReferenceEntries = referenceEntriesFromDir(*referenceDir)
	}

	report := service.VerifyLedger(ctx, store, anchor, verifier, svc.Queries(), cfg)
	if err := jsonOut(report); err != nil {
		return err
	}
	if report.Status == service.VerifyStatusTampered || report.Status == service.VerifyStatusNotRun {
		os.Exit(1)
	}
	return nil
}

// referenceEntryFile is the on-disk shape of a seq-<N>.json trusted
// reference dump: a JSON array of entries as they existed at attestation
// time (design doc §9.1's "pre-incident snapshot restore, a second
// independent replica, a WAL-based point-in-time recovery" -- how the
// operator produces one is out of this tool's scope; this only defines
// the file format ledger-cli verify's --reference-dir reads). Fields
// mirror core.AttestedEntry; snake_case per api-contract.md's wire
// convention even though this is an operator-facing file, not a public
// API, for consistency with everything else this repo serializes.
type referenceEntryFile struct {
	EntryID          int64  `json:"entry_id"`
	JournalID        int64  `json:"journal_id"`
	AccountHolder    int64  `json:"account_holder"`
	CurrencyID       int64  `json:"currency_id"`
	ClassificationID int64  `json:"classification_id"`
	EntryType        string `json:"entry_type"`
	Amount           string `json:"amount"`
	EffectiveAt      string `json:"effective_at"`
}

// referenceEntriesFromDir returns a service.VerifyConfig.ReferenceEntries
// closure that reads dir/seq-<N>.json on demand (only for seqs
// VerifyLedger actually finds a content mismatch at -- not read
// eagerly). Returns ok=false for any seq with no file, a file that fails
// to parse, or an entry whose amount/effective_at fails to parse --
// fail-closed: a malformed reference must never silently produce a wrong
// localization, only no localization.
func referenceEntriesFromDir(dir string) func(seq int64) ([]core.AttestedEntry, bool) {
	return func(seq int64) ([]core.AttestedEntry, bool) {
		path := filepath.Join(dir, fmt.Sprintf("seq-%d.json", seq))
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, false
		}
		var files []referenceEntryFile
		if err := json.Unmarshal(raw, &files); err != nil {
			return nil, false
		}
		entries := make([]core.AttestedEntry, len(files))
		for i, f := range files {
			amount, err := decimal.NewFromString(f.Amount)
			if err != nil {
				return nil, false
			}
			effectiveAt, err := time.Parse(time.RFC3339Nano, f.EffectiveAt)
			if err != nil {
				return nil, false
			}
			entries[i] = core.AttestedEntry{
				EntryID:          f.EntryID,
				JournalID:        f.JournalID,
				AccountHolder:    f.AccountHolder,
				CurrencyID:       f.CurrencyID,
				ClassificationID: f.ClassificationID,
				EntryType:        core.EntryType(f.EntryType),
				Amount:           amount,
				EffectiveAt:      effectiveAt,
			}
		}
		return entries, true
	}
}

func jsonOut(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
