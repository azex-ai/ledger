// Command ledger-cli is a read-only investigation tool for ops + auditors.
//
// Connects directly to the ledger Postgres database (DATABASE_URL) and runs
// the same query interfaces the HTTP server uses. Useful for:
//
//   - Reconciliation triage (`ledger-cli reconcile --full`).
//   - Solvency check (`ledger-cli solvency --currency <uid>`).
//   - Trace a single booking end-to-end (`ledger-cli trace --booking-uid <uid>`).
//   - List recent journals or events (`ledger-cli journals --limit 20`).
//   - Pull a balance snapshot for one account (`ledger-cli balance --holder 42 --currency <uid>`).
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
//	ledger-cli balance --holder 42 --currency <uid> --class main_wallet
//	ledger-cli journals --limit 20
//	ledger-cli trace --booking-uid <uid>
//	ledger-cli reconcile --full
//	ledger-cli solvency --currency <uid>
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
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
  trial-balance  show trial balance report for a currency
  health      show system health metrics
  verify      verify the P6 batch attestation chain + a P5 journal signature sample

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
	case "trial-balance":
		return cmdTrialBalance(ctx, svc, rest)
	case "health":
		return cmdHealth(ctx, svc)
	case "verify":
		return cmdVerify(ctx, pool, svc, rest)
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

	anchor := anchordev.NewLocalFileAnchor(*anchorFile)
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

// keep imported types referenced so the file fails fast if any drift.
var _ = strings.TrimSpace
var _ = core.NormalSideDebit
