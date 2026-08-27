package service

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
)

// checkpointScanPageSize is the page size used when paginating through
// distinct (account_holder, currency_id) pairs for check #2's fleet-wide
// checkpoint-vs-entries scan.
const checkpointScanPageSize = 200

// checkpointBalanceCheckName identifies check #2 both in CheckResult.Name and
// as the persisted resume-cursor key (reconcile_scan_cursors.check_name), so
// the two never drift apart.
const checkpointBalanceCheckName = "checkpoint_balance"

// cursorStartHolder and cursorStartCurrency are the keyset start used for
// both check #2's scan and its persisted resume cursor. They must be
// math.MinInt64, not zero: system holders are the negation of user holders
// (core.SystemHolder), so a zero start would permanently exclude every
// negative holder from the very first page (see
// docs/bugs/2026-08-21-reconcile-coverage-blind-spots.md, B1).
const (
	cursorStartHolder   int64 = math.MinInt64
	cursorStartCurrency int64 = math.MinInt64
)

// ---------------------------------------------------------------------------
// Shared data-transfer types used by ReconcileQuerier and FullReconciliationService
// ---------------------------------------------------------------------------

// OrphanEntrySample is a (entry_id, journal_id) pair for an orphan entry.
type OrphanEntrySample struct {
	EntryID   int64 `json:"entry_id"`
	JournalID int64 `json:"journal_id"`
}

// AccountingEquationRow holds per-(currency, classification) sums.
type AccountingEquationRow struct {
	CurrencyID       int64
	ClassificationID int64
	NormalSide       string
	TotalDebit       decimal.Decimal
	TotalCredit      decimal.Decimal
}

// SettlementNettingViolation is a currency_id whose settlement net is non-zero.
type SettlementNettingViolation struct {
	CurrencyID int64
	NetBalance decimal.Decimal
}

// NegativeBalanceAccount is a user account with a negative balance.
type NegativeBalanceAccount struct {
	AccountHolder    int64
	CurrencyID       int64
	ClassificationID int64
	NormalSide       string
	Balance          decimal.Decimal
}

// RoleLessLiability is a (holder, currency, classification) with a nonzero
// balance on a non-system, user-side classification that carries no
// balance_role (M-4 fix): GetTotalUserSideBalance (I-37) excludes it from
// SolvencyReport.Liability by construction, so a nonzero balance here is a
// real, currently-invisible understatement of what the platform owes. Not
// scoped to credit-normal -- this library's own convention has real
// liabilities on both sides (main_wallet is debit-normal) -- balance_role
// alone, not NormalSide, is what would have distinguished this from a
// legitimate role-less memo account (BalanceRoleMemo).
type RoleLessLiability struct {
	AccountHolder    int64
	CurrencyID       int64
	ClassificationID int64
	NormalSide       string
	Balance          decimal.Decimal
}

// UntaggedHolderKindJournalType is a journal type that shows up in the
// holder-facing transaction view (M-7 fix, docs/INVARIANTS.md I-44) but has
// never been tagged with a core.HolderTxKind -- its transactions read as
// the generic 'other' bucket. UID/Code/Name are the public-safe fields the
// query itself already selects (no internal id, I-18) -- see
// runCheckUntaggedHolderKind's doc for why this is detection, not a
// financial-correctness signal.
type UntaggedHolderKindJournalType struct {
	UID  string
	Code string
	Name string
}

// OrphanReservation is a reservation whose journal_id does not resolve.
type OrphanReservation struct {
	ID            int64
	UID           string
	AccountHolder int64
	CurrencyID    int64
	Status        string
	JournalID     int64
}

// StaleRollupItem is a rollup_queue row with an expired claimed_until lease.
type StaleRollupItem struct {
	ID               int64
	AccountHolder    int64
	CurrencyID       int64
	ClassificationID int64
	ClaimedUntil     string
	FailedAttempts   int
}

// DuplicateIdempotencyKey reports journals sharing an idempotency_key.
type DuplicateIdempotencyKey struct {
	IdempotencyKey string
	Occurrences    int64
	FirstID        int64
	LastID         int64
}

// CheckpointAccountKey identifies a distinct (account_holder, currency_id)
// pair that has at least one row in balance_checkpoints. Used to drive
// check #2's fleet-wide checkpoint-vs-entries scan.
type CheckpointAccountKey struct {
	AccountHolder int64
	CurrencyID    int64
}

// UnbalancedJournal is a (journal_id, currency_id) pair whose entries do not
// net to zero -- a genuine per-journal balance violation. Drives the
// journal_dr_cr check (M1 fix): the check historically named "journal_dr_cr"
// only verified a GLOBAL debit==credit equality (now "global_dr_cr_equality",
// see runCheck1JournalBalance), which cannot see two journals that are each
// individually unbalanced but happen to net to zero in aggregate.
// journal_id/currency_id are internal ids and must never reach a public
// Finding string verbatim (I-18).
type UnbalancedJournal struct {
	JournalID  int64
	CurrencyID int64
	Drift      decimal.Decimal
}

// SystemRollupRow is one (currency_id, classification_id) row from
// system_rollups, in internal-id space (see ListSystemRollupsRaw). Used by
// the system_rollup_integrity check to compare the stored rollup against a
// fresh entries-based recompute (M4/I-23) — balance_checkpoints never enters
// this comparison.
type SystemRollupRow struct {
	CurrencyID       int64
	ClassificationID int64
	TotalBalance     decimal.Decimal
}

// SnapshotDriftRow is a balance_snapshots row (for the most recent
// snapshot_date) whose stored balance disagrees with a fresh entries-based
// recompute as of that date. Used by the snapshot_integrity check (M4/I-23).
type SnapshotDriftRow struct {
	AccountHolder     int64
	CurrencyID        int64
	ClassificationID  int64
	SnapshotDate      time.Time
	StoredBalance     decimal.Decimal
	RecomputedBalance decimal.Decimal
}

// ---------------------------------------------------------------------------
// ReconcileQuerier — the port consumed by FullReconciliationService
// ---------------------------------------------------------------------------

// ReconcileQuerier is the database-facing interface for the extended
// reconciliation checks beyond #1-#2. Defined on the consumer side (service/)
// following hexagonal convention. Implemented by postgres.ReconcileAdapter.
type ReconcileQuerier interface {
	// Check #3
	OrphanEntriesCount(ctx context.Context) (int64, error)
	OrphanEntriesSample(ctx context.Context) ([]OrphanEntrySample, error)
	// Check #4
	AccountingEquationRows(ctx context.Context) ([]AccountingEquationRow, error)
	// Check #5
	SettlementNettingViolations(ctx context.Context, classCode string, windowMinutes int) ([]SettlementNettingViolation, error)
	// Check #6
	NegativeBalanceAccounts(ctx context.Context, pageLimit int) ([]NegativeBalanceAccount, error)
	// role_less_liability (M-4 fix): user-side, non-system
	// classifications with a nonzero balance and no balance_role -- silently
	// excluded from SolvencyReport.Liability. Not scoped to credit-normal;
	// see RoleLessLiability's doc for why.
	RoleLessLiabilities(ctx context.Context, pageLimit int) ([]RoleLessLiability, error)
	// untagged_holder_kind (M-7 follow-up, Team Lead 2026-08-27, board #49):
	// journal types visible in the holder transaction view but never tagged
	// with a core.HolderTxKind -- detection only, see
	// UntaggedHolderKindJournalType's doc.
	UntaggedHolderKindJournalTypes(ctx context.Context, pageLimit int) ([]UntaggedHolderKindJournalType, error)
	// Check #7
	OrphanReservations(ctx context.Context) ([]OrphanReservation, error)
	// Check #9
	DuplicateIdempotencyKeys(ctx context.Context) ([]DuplicateIdempotencyKey, error)
	// Check #10
	StaleRollupItems(ctx context.Context, thresholdMinutes int) ([]StaleRollupItem, error)
	// Check #2 — keyset pagination over distinct (holder, currency) pairs
	// with at least one checkpoint row. Pass (0, 0) for the first page;
	// subsequent pages pass the last row's (AccountHolder, CurrencyID).
	ListCheckpointAccountsPage(ctx context.Context, afterHolder, afterCurrency int64, pageLimit int) ([]CheckpointAccountKey, error)
	// journal_dr_cr (M1 fix) — genuine per-journal, per-currency balance
	// scan; see queries/integrity_balance.sql.
	UnbalancedJournalsCount(ctx context.Context) (int64, error)
	UnbalancedJournalsSample(ctx context.Context) ([]UnbalancedJournal, error)
	// GetScanCursor returns the persisted resume cursor for the named check
	// (C4b), plus lapDirty: whether an earlier segment of the current lap
	// already found a violation (so the run that completes the lap can still
	// report Passed=false), and lapScanned: the cumulative number of
	// account/currency pairs actually walked by every run of the CURRENT lap
	// so far (M-1 fix — see the lapScanned doc on SetScanCursor). Implementations
	// must return (cursorStartHolder, cursorStartCurrency, false, 0, nil) when
	// no cursor has been persisted yet — that is a normal "first run" state,
	// not an error.
	GetScanCursor(ctx context.Context, checkName string) (afterHolder, afterCurrency int64, lapDirty bool, lapScanned int64, err error)
	// SetScanCursor persists the resume cursor, lapDirty flag, and lapScanned
	// (cumulative pairs verified so far this lap) for the named check.
	// lapScanned resets to 0 exactly when the cursor resets to the fresh-lap
	// sentinel (a new lap starting); otherwise it carries forward
	// lapScannedAtStart + this run's own scanned count, so a lap spanning
	// several capped/timed-out runs accumulates a trustworthy total instead
	// of forgetting how much of the fleet earlier runs in the same lap
	// already covered.
	SetScanCursor(ctx context.Context, checkName string, afterHolder, afterCurrency int64, lapDirty bool, lapScanned int64) error
	// CountCheckpointAccountPairs returns the number of distinct
	// (account_holder, currency_id) pairs that currently have at least one
	// row in balance_checkpoints — the same population ListCheckpointAccountsPage
	// paginates over. Used by check #2 (M-1 fix) to cross-check a resumed
	// lap's cumulative lapScanned against a value the resumed cursor's own
	// position cannot influence, so "no more rows after this cursor" cannot
	// by itself certify full coverage on a resumed lap.
	CountCheckpointAccountPairs(ctx context.Context) (int64, error)
	// system_rollup_integrity — raw (internal-id) read of system_rollups,
	// compared against AccountingEquationRows (the same entries-based
	// recompute the accounting_equation check uses) so system_rollups is
	// validated directly against journal_entries, never through
	// balance_checkpoints (M4/I-23).
	ListSystemRollupsRaw(ctx context.Context) ([]SystemRollupRow, error)
	// snapshot_integrity — balance_snapshots rows for the most recent
	// snapshot_date whose stored balance disagrees with a fresh
	// entries-based recompute as of that date, up to pageLimit rows
	// (M4/I-23).
	LatestSnapshotDrift(ctx context.Context, pageLimit int) ([]SnapshotDriftRow, error)
}

// ---------------------------------------------------------------------------
// FullReconciliationConfig — tuneable parameters
// ---------------------------------------------------------------------------

// FullReconciliationConfig holds configurable thresholds for each check.
// All durations default to sensible values when zero.
type FullReconciliationConfig struct {
	// EquationTolerance is the maximum acceptable absolute drift per-currency
	// for the accounting equation check (default 1e-12).
	EquationTolerance decimal.Decimal

	// SettlementClassCode is the classification code used for settlement netting
	// (default "settlement"). Callers should set this to match their schema.
	SettlementClassCode string

	// SettlementWindow is the grace period for in-flight settlement entries
	// (default 30 minutes).
	SettlementWindow time.Duration

	// StaleRollupThreshold is how old an expired claimed_until must be before
	// flagging it as stale (default 5 minutes — one claim lease).
	StaleRollupThreshold time.Duration

	// NegativeBalancePageLimit caps the number of violations fetched per run
	// (default 200).
	NegativeBalancePageLimit int

	// Check2ScanLimit caps the number of distinct (holder, currency) account
	// pairs scanned per run for the checkpoint-vs-entries verification
	// (default 5000). Fleets larger than this are only partially covered in a
	// single run — the check reports how far it got instead of silently
	// claiming full coverage.
	Check2ScanLimit int

	// Check2Timeout bounds the wall-clock time spent on the check #2 scan
	// (default 2 minutes). Whichever limit — count or time — is hit first
	// stops the scan for this run; the check reports the partial coverage.
	Check2Timeout time.Duration

	// SnapshotIntegrityPageLimit caps the number of drifting balance_snapshots
	// rows fetched per run for check #12 (default 200, mirroring
	// NegativeBalancePageLimit). Reaching the cap marks the check incomplete
	// rather than silently truncating the finding list.
	SnapshotIntegrityPageLimit int

	// UnauthorizedJournalsPageLimit caps how many of the oldest journals the
	// unauthorized_journals check (contracts §W2-2, I-32) scans per run
	// (default 2000). The check has no persisted resume cursor yet (unlike
	// checkpoint_balance's C4b) -- reaching the cap marks Complete=false
	// honestly rather than silently claiming full coverage; it does not
	// silently rescan the same oldest slice forever without SAYING that is
	// what happened.
	UnauthorizedJournalsPageLimit int

	// UntaggedHolderKindPageLimit caps the number of untagged journal types
	// fetched per run for the untagged_holder_kind check (M-7 follow-up,
	// default 200, mirroring NegativeBalancePageLimit). This is a
	// cardinality bound on distinct journal types, not on journals or
	// entries -- deployments rarely have more than a few dozen -- so
	// reaching the cap is not expected in practice.
	UntaggedHolderKindPageLimit int
}

func (c *FullReconciliationConfig) withDefaults() FullReconciliationConfig {
	out := *c
	if out.EquationTolerance.IsZero() {
		out.EquationTolerance = decimal.NewFromFloat(1e-12)
	}
	if out.SettlementClassCode == "" {
		out.SettlementClassCode = "settlement"
	}
	if out.SettlementWindow == 0 {
		out.SettlementWindow = 30 * time.Minute
	}
	if out.StaleRollupThreshold == 0 {
		out.StaleRollupThreshold = 5 * time.Minute
	}
	if out.NegativeBalancePageLimit == 0 {
		out.NegativeBalancePageLimit = 200
	}
	if out.Check2ScanLimit == 0 {
		out.Check2ScanLimit = 5000
	}
	if out.Check2Timeout == 0 {
		out.Check2Timeout = 2 * time.Minute
	}
	if out.SnapshotIntegrityPageLimit == 0 {
		out.SnapshotIntegrityPageLimit = 200
	}
	if out.UnauthorizedJournalsPageLimit == 0 {
		out.UnauthorizedJournalsPageLimit = 2000
	}
	if out.UntaggedHolderKindPageLimit == 0 {
		out.UntaggedHolderKindPageLimit = 200
	}
	return out
}

// ---------------------------------------------------------------------------
// FullReconciliationService — implements core.FullReconciler
// ---------------------------------------------------------------------------

// FullReconciliationService runs the full reconciliation suite. Checks #1-#2
// reuse the existing ReconciliationService logic; the rest use the
// ReconcileQuerier port. The exact check count is a fact of
// RunFullReconciliation's body, not of this comment -- see the
// TestFullReconciliation_AllPass assertion for the machine-checked count.
type FullReconciliationService struct {
	basic   *ReconciliationService
	querier ReconcileQuerier
	cfg     FullReconciliationConfig
	logger  core.Logger
	metrics core.Metrics

	// journals/verifier back the unauthorized_journals check (contracts
	// §W2-2, I-32). Both nil (the default -- SetAuthCheck was never called)
	// means the check is skipped outright (Complete=false): a deployment
	// that never configured signing has nothing for this check to verify,
	// and every unsigned journal in that state is expected, not evidence of
	// tampering.
	journals core.QueryProvider
	verifier core.AuthVerifier
}

// SetAuthCheck wires the unauthorized_journals check (contracts §W2-2,
// I-32) to a journal reader and the core.AuthVerifier configured via
// ledger.WithAttestor. Call once after NewFullReconciliationService;
// omitting this call leaves the check skipped (Complete=false), never
// silently "passed" (working-agreements §3).
func (s *FullReconciliationService) SetAuthCheck(journals core.QueryProvider, verifier core.AuthVerifier) {
	s.journals = journals
	s.verifier = verifier
}

// Compile-time assertion.
var _ core.FullReconciler = (*FullReconciliationService)(nil)

// externalCurrencyRef resolves an internal currency id to its uid for report
// strings. Internal BIGSERIAL ids appear in no public contract (I-18) — the
// reconcile report is returned verbatim by POST /reconcile/full, so every id
// woven into a Description/Detail is a contract leak. Falls back to a
// non-identifying placeholder (never the raw id) when resolution fails; the
// id itself goes to logs only.
func (s *FullReconciliationService) externalCurrencyRef(ctx context.Context, id int64) string {
	if s.basic == nil || s.basic.classifications == nil {
		return "unresolved-currency"
	}
	uid, err := s.basic.classifications.CurrencyUIDByID(ctx, id)
	if err != nil {
		s.logger.Warn("service: reconcile: currency uid resolution failed", "currency_id", id, "error", err)
		return "unresolved-currency"
	}
	return uid
}

// externalClassificationRef resolves an internal classification id to its
// code (the human-facing public identifier) for report strings. Same I-18
// rationale as externalCurrencyRef.
func (s *FullReconciliationService) externalClassificationRef(ctx context.Context, id int64) string {
	if s.basic == nil || s.basic.classifications == nil {
		return "unresolved-classification"
	}
	dims, err := s.basic.classifications.ClassificationDims(ctx)
	if err != nil {
		s.logger.Warn("service: reconcile: classification resolution failed", "classification_id", id, "error", err)
		return "unresolved-classification"
	}
	for _, d := range dims {
		if d.ID == id {
			return d.Code
		}
	}
	return "unresolved-classification"
}

// NewFullReconciliationService builds a FullReconciliationService.
func NewFullReconciliationService(
	basic *ReconciliationService,
	querier ReconcileQuerier,
	cfg FullReconciliationConfig,
	engine *core.Engine,
) *FullReconciliationService {
	return &FullReconciliationService{
		basic:   basic,
		querier: querier,
		cfg:     cfg.withDefaults(),
		logger:  engine.Logger(),
		metrics: engine.Metrics(),
	}
}

// RunFullReconciliation executes every check in the suite. Each check runs
// independently; an error in one is recorded as a Finding, not a hard
// failure that aborts the rest.
func (s *FullReconciliationService) RunFullReconciliation(ctx context.Context) (*core.ReconcileReport, error) {
	now := time.Now()
	checks := make([]core.CheckResult, 0, 15)

	// --- Check #1: global debit == credit equality ---
	checks = append(checks, s.runCheck1JournalBalance(ctx))

	// --- Check #2: Checkpoint balance vs entry sum ---
	// ReconcileAccount is per-account and too expensive to enumerate for a
	// full-fleet scan on every run, so it is paginated separately here rather
	// than folded into check #1.
	checks = append(checks, s.runCheck2GlobalBalance(ctx))

	// --- Check #3: Orphan entries ---
	checks = append(checks, s.runCheck3OrphanEntries(ctx))

	// --- Check #4: Accounting equation A = L + E ---
	checks = append(checks, s.runCheck4AccountingEquation(ctx))

	// --- Check #5: Settlement netting ---
	checks = append(checks, s.runCheck5SettlementNetting(ctx))

	// --- Check #6: Non-negative user balances ---
	checks = append(checks, s.runCheck6NonNegativeBalances(ctx))

	// --- role_less_liability: user-side credit-normal classification missing balance_role (M-4 fix) ---
	checks = append(checks, s.runCheckRoleLessLiability(ctx))

	// --- untagged_holder_kind: journal type visible to holders but never tagged (M-7 follow-up) ---
	checks = append(checks, s.runCheckUntaggedHolderKind(ctx))

	// --- Check #7: Orphan reservations ---
	checks = append(checks, s.runCheck7OrphanReservations(ctx))

	// Check #8 ("pending_journal_timeout") used to live here: it required a
	// journals.status field that was never added to the schema, so it could
	// structurally never run -- runCheck8PendingJournalTimeout always
	// returned Complete=false unconditionally, for every run, forever. Its
	// single vote permanently poisoned FullCoverage below to false (operability.md
	// Major: "full_coverage 永远为假"), which made the whole point of adding
	// Complete/FullCoverage moot -- a signal that can never be true carries no
	// information (working-agreements §3: a check that can never run is not
	// a check). Removed rather than patched around: a check belongs in this
	// suite only once it can actually execute. The pending work item
	// (journals.status field, then re-add a real check against it) is
	// tracked in the audit remediation backlog, not as a permanently-red
	// placeholder in the suite that reports itself.

	// --- Check #9: Idempotency uniqueness audit ---
	checks = append(checks, s.runCheck9IdempotencyAudit(ctx))

	// --- Check #10: Stale rollup queue ---
	checks = append(checks, s.runCheck10StaleRollup(ctx))

	// --- journal_dr_cr: genuine per-journal balance (M1 fix) ---
	checks = append(checks, s.runCheck11JournalBalance(ctx))

	// --- system_rollup_integrity: system_rollups vs entries (M4/I-23) ---
	checks = append(checks, s.runCheckSystemRollupIntegrity(ctx))

	// --- snapshot_integrity: balance_snapshots vs entries (M4/I-23) ---
	checks = append(checks, s.runCheckSnapshotIntegrity(ctx))

	// --- unauthorized_journals: per-journal P5 signature validity (contracts §W2-2, I-32) ---
	checks = append(checks, s.runCheckUnauthorizedJournals(ctx))

	// Compute overall result. Violations found and coverage achieved are
	// tracked separately: a run that examined half the fleet and found
	// nothing is not the same as a run that verified everything.
	overallPassed := true
	fullCoverage := true
	for _, c := range checks {
		if !c.Passed {
			overallPassed = false
		}
		if !c.Complete {
			fullCoverage = false
		}
	}

	switch {
	case overallPassed && fullCoverage:
		s.logger.Info("reconcile: full suite passed")
	case overallPassed:
		s.logger.Warn("reconcile: full suite found no violations but coverage was incomplete")
	default:
		s.logger.Warn("reconcile: full suite has failures")
	}

	for _, c := range checks {
		// Report Passed && Complete so anything alerting on this metric fails
		// closed: an incomplete or skipped check must not look green.
		s.metrics.ReconcileCheckResult(c.Name, c.Passed && c.Complete)
	}

	return &core.ReconcileReport{
		Checks:        checks,
		OverallPassed: overallPassed,
		FullCoverage:  fullCoverage,
		RunAt:         now,
	}, nil
}

// runCheck1JournalBalance wraps the existing GlobalSummer DR=CR logic.
// We reuse ReconciliationService.CheckAccountingEquation, which sums debits
// and credits GLOBALLY across all entries. This is deliberately named
// "global_dr_cr_equality", not "journal_dr_cr": it does NOT verify any
// individual journal balances, only that the fleet-wide totals match. Two
// journals that are each individually unbalanced by currency but net to
// zero in aggregate pass this check undetected -- that gap is M1
// (docs/plans/2026-08-21-tamper-evident-ledger-design.md §2), closed by the
// genuine per-journal check below (runCheck11JournalBalance, which now owns
// the "journal_dr_cr" name). The two checks catch different failure modes
// and are kept independent; neither substitutes for the other.
func (s *FullReconciliationService) runCheck1JournalBalance(ctx context.Context) core.CheckResult {
	result := core.CheckResult{Name: "global_dr_cr_equality", Passed: true, Complete: true, Findings: []core.Finding{}, CheckedAt: time.Now()}

	r, err := s.basic.CheckAccountingEquation(ctx)
	if err != nil {
		result.Passed = false
		result.Findings = append(result.Findings, core.Finding{
			Description: "global DR=CR equality check failed to execute",
			Detail:      err.Error(),
		})
		return result
	}

	result.CheckedAt = r.CheckedAt
	if !r.Balanced {
		result.Passed = false
		for _, d := range r.Details {
			result.Findings = append(result.Findings, core.Finding{
				Description: fmt.Sprintf("currency %s: global debit/credit imbalance", d.CurrencyUID),
				Detail:      fmt.Sprintf("debit=%s credit=%s gap=%s", d.Expected, d.Actual, d.Drift),
			})
		}
	}
	return result
}

// runCheck11JournalBalance is the genuine per-journal, per-currency balance
// check (M1 fix). It owns the "journal_dr_cr" name that runCheck1JournalBalance
// (now "global_dr_cr_equality") used to hold under a global-equality
// implementation that could not see this class of violation -- see
// docs/plans/2026-08-21-tamper-evident-ledger-design.md §2 M1 and §5.
//
// This scan is a bulk defense-in-depth complement to the DB-layer deferred
// constraint trigger added in migration 044: the trigger only validates rows
// written after it existed (constraint triggers are not retroactive), so
// this check is what would catch a violation written during the 018→044 gap
// window, or via any future bypass of the trigger.
func (s *FullReconciliationService) runCheck11JournalBalance(ctx context.Context) core.CheckResult {
	result := core.CheckResult{Name: "journal_dr_cr", Passed: true, Complete: true, Findings: []core.Finding{}, CheckedAt: time.Now()}

	count, err := s.querier.UnbalancedJournalsCount(ctx)
	if err != nil {
		result.Passed = false
		result.Findings = append(result.Findings, core.Finding{
			Description: "per-journal balance query failed",
			Detail:      err.Error(),
		})
		return result
	}
	if count == 0 {
		return result
	}

	result.Passed = false
	result.Findings = append(result.Findings, core.Finding{
		Description: fmt.Sprintf("%d journal/currency pair(s) fail per-journal balance", count),
	})

	samples, err := s.querier.UnbalancedJournalsSample(ctx)
	if err != nil {
		result.Findings = append(result.Findings, core.Finding{
			Description: "could not fetch unbalanced journal samples",
			Detail:      err.Error(),
		})
		return result
	}
	// Per-sample forensics carry internal row ids — ops-log material, not
	// report material (I-18: the report is an API response body).
	for _, u := range samples {
		s.logger.Warn("service: reconcile: unbalanced journal sample",
			"journal_id", u.JournalID,
			"currency_id", u.CurrencyID,
			"drift", u.Drift.String(),
		)
	}
	result.Findings = append(result.Findings, core.Finding{
		Description: fmt.Sprintf("%d unbalanced journal sample(s) recorded in server logs", len(samples)),
	})
	return result
}

// runCheck2GlobalBalance verifies that each account's checkpointed balance
// matches a full recomputation from journal_entries (ReconcileAccount), for
// every distinct (holder, currency) pair that has a checkpoint. Pairs are
// discovered via keyset pagination (ListCheckpointAccountsPage) so memory
// stays bounded regardless of fleet size.
//
// A full-fleet scan can be expensive on a large ledger, so the scan is capped
// by cfg.Check2ScanLimit (account pairs) and cfg.Check2Timeout (wall clock),
// whichever is hit first. When capped, the check reports an explicit
// "incomplete" Finding with the resume cursor instead of silently reporting
// success as if full coverage had been verified.
func (s *FullReconciliationService) runCheck2GlobalBalance(ctx context.Context) core.CheckResult {
	result := core.CheckResult{Name: checkpointBalanceCheckName, Passed: true, Complete: true, Findings: []core.Finding{}, CheckedAt: time.Now()}

	scanCtx, cancel := context.WithTimeout(ctx, s.cfg.Check2Timeout)
	defer cancel()

	// C4b: resume from the persisted cursor instead of always restarting at
	// the top. Without this, any fleet larger than Check2ScanLimit had its
	// tail permanently unscanned -- every run re-verified the same prefix and
	// never reached the rest (docs/bugs/2026-08-21-reconcile-coverage-blind-spots.md,
	// "未解决" section). GetScanCursor returns (cursorStartHolder,
	// cursorStartCurrency) on a fresh install (no row persisted yet) -- NOT
	// (0, 0): system holders are the negation of user holders
	// (core.SystemHolder), so a zero start would exclude every negative
	// holder on the first page, permanently (B1 in the same bug report).
	// Cursor reads/writes use ctx, not scanCtx: they must still succeed after
	// scanCtx's own deadline has been reached (that deadline bounds the scan
	// loop below, not bookkeeping around it).
	afterHolder, afterCurrency, lapDirtyAtStart, lapScannedAtStart, err := s.querier.GetScanCursor(ctx, checkpointBalanceCheckName)
	if err != nil {
		result.Passed = false
		result.Complete = false
		result.Findings = append(result.Findings, core.Finding{
			Description: "checkpoint scan cursor read failed",
			Detail:      err.Error(),
		})
		return result
	}
	// resumedLap records whether this run's starting cursor was mid-lap
	// (persisted by an earlier, capped/timed-out run) rather than the
	// fresh-lap sentinel. Captured before the loop below reassigns
	// afterHolder/afterCurrency as it advances -- see the "0 pairs on a
	// resumed lap" handling after the loop for why this distinction exists
	// (threat-model.md Major, §4-3: reconcile_scan_cursors has no DB-level
	// mutation guard against ledger_app, so a resumed lap that finds
	// literally nothing left is not automatically trustworthy the way a
	// fresh lap finding nothing is).
	resumedLap := afterHolder != cursorStartHolder || afterCurrency != cursorStartCurrency
	scanned := 0
	partialReason := ""

pageLoop:
	for scanned < s.cfg.Check2ScanLimit {
		pageSize := checkpointScanPageSize
		if remaining := s.cfg.Check2ScanLimit - scanned; remaining < pageSize {
			pageSize = remaining
		}

		pairs, err := s.querier.ListCheckpointAccountsPage(scanCtx, afterHolder, afterCurrency, pageSize)
		if err != nil {
			if scanCtx.Err() != nil {
				partialReason = fmt.Sprintf("scan timed out after %s", s.cfg.Check2Timeout)
				break pageLoop
			}
			result.Passed = false
			result.Findings = append(result.Findings, core.Finding{
				Description: "checkpoint account page query failed",
				Detail:      err.Error(),
			})
			return result
		}
		if len(pairs) == 0 {
			break
		}

		for _, p := range pairs {
			if scanCtx.Err() != nil {
				partialReason = fmt.Sprintf("scan timed out after %s", s.cfg.Check2Timeout)
				break pageLoop
			}

			pairCurrencyUID, err := s.basic.classifications.CurrencyUIDByID(scanCtx, p.CurrencyID)
			var acctResult *core.ReconcileResult
			if err == nil {
				acctResult, err = s.basic.ReconcileAccount(scanCtx, p.AccountHolder, pairCurrencyUID)
			}
			if err != nil {
				if scanCtx.Err() != nil {
					partialReason = fmt.Sprintf("scan timed out after %s", s.cfg.Check2Timeout)
					break pageLoop
				}
				result.Passed = false
				result.Findings = append(result.Findings, core.Finding{
					Description: fmt.Sprintf("checkpoint reconcile failed for holder %d currency %s", p.AccountHolder, s.externalCurrencyRef(scanCtx, p.CurrencyID)),
					Detail:      err.Error(),
				})
				afterHolder, afterCurrency = p.AccountHolder, p.CurrencyID
				scanned++
				continue
			}

			if !acctResult.Balanced {
				result.Passed = false
				for _, d := range acctResult.Details {
					result.Findings = append(result.Findings, core.Finding{
						Description: fmt.Sprintf("holder %d currency %s classification %s: checkpoint balance drift", d.AccountHolder, d.CurrencyUID, d.ClassificationUID),
						Detail:      fmt.Sprintf("expected=%s actual(checkpoint)=%s drift=%s", d.Expected, d.Actual, d.Drift),
					})
				}
			}

			afterHolder, afterCurrency = p.AccountHolder, p.CurrencyID
			scanned++
		}

		if len(pairs) < pageSize {
			break
		}
	}

	if scanned >= s.cfg.Check2ScanLimit && partialReason == "" {
		partialReason = fmt.Sprintf("scan limit reached (%d account pairs)", s.cfg.Check2ScanLimit)
	}

	// sliceDirty is this run's own outcome, before folding in whatever an
	// earlier segment of the same (possibly multi-run) lap already found.
	sliceDirty := !result.Passed
	lapDirty := lapDirtyAtStart || sliceDirty
	if lapDirtyAtStart && !sliceDirty {
		// This slice was itself clean, but an earlier segment of this lap
		// already found a violation -- the lap as a whole is not clean, and
		// the check must say so even though nothing new turned up just now.
		// Without this, the run that happens to complete a multi-run lap
		// could report Passed=true purely because ITS OWN slice was clean,
		// silently burying an earlier run's finding -- the same "looks green
		// when it isn't" shape P0 fixed for the single-run case.
		result.Passed = false
		result.Findings = append(result.Findings, core.Finding{
			Description: "checkpoint scan: an earlier segment of this lap already found a violation",
		})
	}

	// lapScanned is this lap's cumulative coverage: every pair actually
	// walked by every run since the cursor last reset to the fresh-lap
	// sentinel, including this run's own scanned count. It is the M-1 fix's
	// independent signal -- unlike afterHolder/afterCurrency, which the
	// resumed cursor's OWN starting position directly determines (and which
	// threat-model.md's §4-3 established has no DB-level mutation guard
	// against ledger_app), lapScanned only ever grows by this loop's own
	// verified page-query results, never by a resumed run trusting where it
	// was told to start.
	lapScanned := lapScannedAtStart + int64(scanned)

	if partialReason != "" {
		// Coverage was not achieved. Passed may still be true (nothing was
		// wrong in the subset we examined, and no prior segment of this lap
		// was dirty either) but the check must not testify about the pairs
		// it never reached.
		result.Complete = false
		result.Findings = append(result.Findings, core.Finding{
			Description: fmt.Sprintf("checkpoint scan incomplete: %s", partialReason),
			Detail:      fmt.Sprintf("scanned %d account/currency pairs before stopping; the next run resumes from the persisted cursor (holder %d)", scanned, afterHolder),
		})
		if setErr := s.querier.SetScanCursor(ctx, checkpointBalanceCheckName, afterHolder, afterCurrency, lapDirty, lapScanned); setErr != nil {
			result.Passed = false
			result.Findings = append(result.Findings, core.Finding{
				Description: "checkpoint scan cursor persist failed",
				Detail:      setErr.Error(),
			})
		}
	} else if scanned == 0 && resumedLap {
		// The lap "completed" on its very first page fetch of this run, but
		// that page was fetched starting from a cursor already mid-lap, not
		// from the fresh sentinel -- and it came back completely empty.
		// This is EXACTLY the shape a tampered cursor produces (threat-model.md:
		// `UPDATE reconcile_scan_cursors SET after_holder = <huge>, lap_dirty
		// = false` makes every subsequent page query return zero rows, and
		// nothing about "the page was empty" on its own says whether that is
		// because the fleet is genuinely exhausted at this exact point or
		// because the cursor was moved there by something other than this
		// scan loop actually walking through the data). Reporting
		// Complete=true here would be indistinguishable, on the wire, from a
		// full-fleet scan that legitimately found nothing -- the same
		// "looks green when it isn't" shape P0 fixed for capped/timed-out
		// runs, just one level deeper (a scan that never advanced the cursor
		// itself, only inherited someone else's).
		//
		// The cursor still resets to the fresh sentinel below (same as the
		// genuinely-clean branch): a legitimate lap that happens to finish
		// exactly on a page boundary self-corrects on the VERY NEXT run
		// (which will then start from the fresh sentinel and, if the fleet
		// really is empty, correctly report Complete=true) -- one
		// conservative run of under-claimed coverage is a cheap price for
		// closing the "reset the cursor before every scheduled run" attack
		// this finding describes: with this check in place, that attack
		// keeps FullCoverage honestly false for as long as it is repeated,
		// instead of reading as a continuously clean bill of health.
		result.Complete = false
		result.Findings = append(result.Findings, core.Finding{
			Description: "checkpoint scan: resumed from a non-fresh cursor and found zero pairs on the first page",
			Detail:      "cannot distinguish a lap that legitimately finished exactly at the persisted resume point from a cursor advanced by something other than this scan (see docs/RUNBOOK.md); not counted as full coverage",
		})
		if setErr := s.querier.SetScanCursor(ctx, checkpointBalanceCheckName, cursorStartHolder, cursorStartCurrency, false, 0); setErr != nil {
			result.Passed = false
			result.Findings = append(result.Findings, core.Finding{
				Description: "checkpoint scan cursor reset failed",
				Detail:      setErr.Error(),
			})
		}
	} else if resumedLap {
		// The page loop reached the natural end of the data ("no more rows
		// after this cursor") AND found at least one pair this run (the
		// scanned == 0 case above already handles zero) -- but this run's
		// starting cursor still was not the fresh-lap sentinel. This is
		// M-1's exact finding: the branch above only ever distrusted a
		// resumed run that found LITERALLY NOTHING, so a cursor tampered to
		// leave exactly one (or any small number of) pairs unscanned sailed
		// straight through it and reported Complete=true, resetting the
		// cursor back to fresh -- one forged UPDATE bought one permanently
		// clean-looking run. "No more rows after this cursor" cannot
		// certify coverage on a resumed lap at ANY pair count, because the
		// resumed cursor's own starting position is exactly what an
		// attacker (or anything else rewriting reconcile_scan_cursors)
		// controls. The independent check: how many pairs has this LAP
		// verified in total, across every run since it last started fresh,
		// against how many pairs the checkpoint fleet actually has right
		// now.
		total, cerr := s.querier.CountCheckpointAccountPairs(ctx)
		if cerr != nil {
			result.Passed = false
			result.Findings = append(result.Findings, core.Finding{
				Description: "checkpoint account pair count failed",
				Detail:      cerr.Error(),
			})
			return result
		}
		if lapScanned < total {
			result.Complete = false
			result.Findings = append(result.Findings, core.Finding{
				Description: "checkpoint scan: resumed lap ended without reaching the ledger's full pair count",
				Detail: fmt.Sprintf(
					"resumed from a non-fresh cursor and verified %d account/currency pairs cumulatively this lap, but %d pairs currently exist; cannot distinguish a lap that legitimately finished exactly at the persisted resume point from a cursor advanced by something other than this scan (see docs/RUNBOOK.md); not counted as full coverage",
					lapScanned, total),
			})
			// Restart the lap from scratch rather than trust the current
			// (possibly tampered) position for a continuation: the next run
			// re-walks from the true beginning, and lapScanned resets to 0
			// so it does not inherit a count this run could not verify.
			if setErr := s.querier.SetScanCursor(ctx, checkpointBalanceCheckName, cursorStartHolder, cursorStartCurrency, false, 0); setErr != nil {
				result.Passed = false
				result.Findings = append(result.Findings, core.Finding{
					Description: "checkpoint scan cursor reset failed",
					Detail:      setErr.Error(),
				})
			}
		} else {
			result.Findings = append(result.Findings, core.Finding{
				Description: fmt.Sprintf("checkpoint scan complete: %d account/currency pairs verified this run", scanned),
			})
			if setErr := s.querier.SetScanCursor(ctx, checkpointBalanceCheckName, cursorStartHolder, cursorStartCurrency, false, 0); setErr != nil {
				result.Passed = false
				result.Findings = append(result.Findings, core.Finding{
					Description: "checkpoint scan cursor reset failed",
					Detail:      setErr.Error(),
				})
			}
		}
	} else {
		result.Findings = append(result.Findings, core.Finding{
			Description: fmt.Sprintf("checkpoint scan complete: %d account/currency pairs verified this run", scanned),
		})
		// A full lap just completed: reset the cursor, lap_dirty flag, and
		// lapScanned counter so the next run starts a fresh lap instead of
		// replaying this resume point (or a stale dirty flag/count) forever.
		if setErr := s.querier.SetScanCursor(ctx, checkpointBalanceCheckName, cursorStartHolder, cursorStartCurrency, false, 0); setErr != nil {
			result.Passed = false
			result.Findings = append(result.Findings, core.Finding{
				Description: "checkpoint scan cursor reset failed",
				Detail:      setErr.Error(),
			})
		}
	}

	return result
}

// runCheck3OrphanEntries checks for entries whose journal_id is not in journals.
func (s *FullReconciliationService) runCheck3OrphanEntries(ctx context.Context) core.CheckResult {
	result := core.CheckResult{Name: "orphan_entries", Passed: true, Complete: true, Findings: []core.Finding{}, CheckedAt: time.Now()}

	count, err := s.querier.OrphanEntriesCount(ctx)
	if err != nil {
		result.Passed = false
		result.Findings = append(result.Findings, core.Finding{
			Description: "orphan entry count query failed",
			Detail:      err.Error(),
		})
		return result
	}

	if count == 0 {
		return result
	}

	result.Passed = false
	result.Findings = append(result.Findings, core.Finding{
		Description: fmt.Sprintf("%d orphan entries found (journal_id references missing journal)", count),
	})

	samples, err := s.querier.OrphanEntriesSample(ctx)
	if err != nil {
		result.Findings = append(result.Findings, core.Finding{
			Description: "could not fetch orphan entry samples",
			Detail:      err.Error(),
		})
		return result
	}
	// Per-sample forensics carry internal row ids — ops-log material, not
	// report material (I-18: the report is an API response body).
	for _, o := range samples {
		s.logger.Warn("service: reconcile: orphan entry sample",
			"entry_id", o.EntryID,
			"journal_id", o.JournalID,
		)
	}
	result.Findings = append(result.Findings, core.Finding{
		Description: fmt.Sprintf("%d orphan entry sample(s) recorded in server logs", len(samples)),
	})
	return result
}

// runCheck4AccountingEquation verifies A = L + E per currency using NormalSide.
// "Asset" classifications are debit-normal; "Liability/Equity/Revenue" are credit-normal.
// Sum(debit-normal net) should equal Sum(credit-normal net) per currency
// (the accounting equation expressed as DR totals == CR totals per currency).
func (s *FullReconciliationService) runCheck4AccountingEquation(ctx context.Context) core.CheckResult {
	result := core.CheckResult{Name: "accounting_equation", Passed: true, Complete: true, Findings: []core.Finding{}, CheckedAt: time.Now()}

	rows, err := s.querier.AccountingEquationRows(ctx)
	if err != nil {
		result.Passed = false
		result.Findings = append(result.Findings, core.Finding{
			Description: "accounting equation query failed",
			Detail:      err.Error(),
		})
		return result
	}

	// Group by currency: sum net balance per NormalSide.
	// For each currency: SUM(debit-normal nets) should equal SUM(credit-normal nets).
	// Equivalently: the net of ALL classifications should be zero (because every
	// journal is balanced — debits == credits — and the equation holds globally).
	type currencyNet struct {
		debitNormalNet  decimal.Decimal
		creditNormalNet decimal.Decimal
	}
	perCurrency := make(map[int64]*currencyNet)
	for _, r := range rows {
		// Net delta and normal-side classification both route through
		// core.Delta / core.NormalSide.IsValid — the sole authority for
		// interpreting normal_side (I-43). An unknown normal_side surfaces
		// as a failing Finding rather than silently defaulting into the
		// credit-normal bucket.
		ns := core.NormalSide(r.NormalSide)
		net, err := core.Delta(ns, r.TotalDebit, r.TotalCredit)
		if err != nil {
			result.Passed = false
			result.Findings = append(result.Findings, core.Finding{
				Description: fmt.Sprintf("currency %s classification %s: %s",
					s.externalCurrencyRef(ctx, r.CurrencyID), s.externalClassificationRef(ctx, r.ClassificationID), err.Error()),
			})
			continue
		}
		cn := perCurrency[r.CurrencyID]
		if cn == nil {
			cn = &currencyNet{}
			perCurrency[r.CurrencyID] = cn
		}
		if ns == core.NormalSideDebit {
			cn.debitNormalNet = cn.debitNormalNet.Add(net)
		} else {
			cn.creditNormalNet = cn.creditNormalNet.Add(net)
		}
	}

	// Sort currency IDs for deterministic output.
	currencyIDs := make([]int64, 0, len(perCurrency))
	for cid := range perCurrency {
		currencyIDs = append(currencyIDs, cid)
	}
	sort.Slice(currencyIDs, func(i, j int) bool { return currencyIDs[i] < currencyIDs[j] })

	for _, cid := range currencyIDs {
		cn := perCurrency[cid]
		diff := cn.debitNormalNet.Sub(cn.creditNormalNet)
		if diff.Abs().GreaterThan(s.cfg.EquationTolerance) {
			result.Passed = false
			result.Findings = append(result.Findings, core.Finding{
				Description: fmt.Sprintf("currency %s: accounting equation imbalance", s.externalCurrencyRef(ctx, cid)),
				Detail: fmt.Sprintf("debit-normal net=%s credit-normal net=%s diff=%s",
					cn.debitNormalNet, cn.creditNormalNet, diff),
			})
		}
	}
	return result
}

// runCheck5SettlementNetting verifies that the settlement classification nets
// to zero per currency outside the configured grace window.
func (s *FullReconciliationService) runCheck5SettlementNetting(ctx context.Context) core.CheckResult {
	result := core.CheckResult{Name: "settlement_netting", Passed: true, Complete: true, Findings: []core.Finding{}, CheckedAt: time.Now()}

	windowMins := int(s.cfg.SettlementWindow.Minutes())
	violations, err := s.querier.SettlementNettingViolations(ctx, s.cfg.SettlementClassCode, windowMins)
	if err != nil {
		result.Passed = false
		result.Findings = append(result.Findings, core.Finding{
			Description: "settlement netting query failed",
			Detail:      err.Error(),
		})
		return result
	}

	for _, v := range violations {
		result.Passed = false
		result.Findings = append(result.Findings, core.Finding{
			Description: fmt.Sprintf("currency %s: settlement classification net balance is non-zero", s.externalCurrencyRef(ctx, v.CurrencyID)),
			Detail:      fmt.Sprintf("net=%s (expected 0, excluding last %d min)", v.NetBalance, windowMins),
		})
	}
	return result
}

// runCheck6NonNegativeBalances verifies no user account (holder > 0) has a
// negative balance for any classification.
func (s *FullReconciliationService) runCheck6NonNegativeBalances(ctx context.Context) core.CheckResult {
	result := core.CheckResult{Name: "non_negative_balances", Passed: true, Complete: true, Findings: []core.Finding{}, CheckedAt: time.Now()}

	accounts, err := s.querier.NegativeBalanceAccounts(ctx, s.cfg.NegativeBalancePageLimit)
	if err != nil {
		result.Passed = false
		result.Findings = append(result.Findings, core.Finding{
			Description: "non-negative balance scan failed",
			Detail:      err.Error(),
		})
		return result
	}

	for _, acc := range accounts {
		result.Passed = false
		result.Findings = append(result.Findings, core.Finding{
			Description: fmt.Sprintf("holder %d currency %s classification %s has negative balance",
				acc.AccountHolder, s.externalCurrencyRef(ctx, acc.CurrencyID), s.externalClassificationRef(ctx, acc.ClassificationID)),
			Detail: fmt.Sprintf("balance=%s (normal_side=%s)", acc.Balance, acc.NormalSide),
		})
	}
	return result
}

// runCheckRoleLessLiability verifies every user-side (holder > 0), non-system
// classification with a nonzero balance carries a balance_role (M-4 fix):
// GetTotalUserSideBalance (I-37) sums liability only from role-bearing
// classifications, so a classification mistagged without a role is a real,
// currently-invisible understatement of SolvencyReport.Liability -- the
// "false surplus" direction, the most dangerous one for a solvency check to
// be wrong in. Not scoped to credit-normal: this library's own convention
// has real liabilities on both sides (main_wallet, the canonical liability,
// is debit-normal), so normal_side cannot distinguish a mistagged liability
// from a legitimate role-less memo account the way balance_role does
// (BalanceRoleMemo, migration 011) -- see RoleLessLiability's doc.
func (s *FullReconciliationService) runCheckRoleLessLiability(ctx context.Context) core.CheckResult {
	result := core.CheckResult{Name: "role_less_liability", Passed: true, Complete: true, Findings: []core.Finding{}, CheckedAt: time.Now()}

	rows, err := s.querier.RoleLessLiabilities(ctx, s.cfg.NegativeBalancePageLimit)
	if err != nil {
		result.Passed = false
		result.Findings = append(result.Findings, core.Finding{
			Description: "role-less liability scan failed",
			Detail:      err.Error(),
		})
		return result
	}

	for _, r := range rows {
		result.Passed = false
		result.Findings = append(result.Findings, core.Finding{
			Description: fmt.Sprintf("holder %d currency %s classification %s is user-side and non-system, but carries no balance_role",
				r.AccountHolder, s.externalCurrencyRef(ctx, r.CurrencyID), s.externalClassificationRef(ctx, r.ClassificationID)),
			Detail: fmt.Sprintf("balance=%s (normal_side=%s) is excluded from SolvencyReport.Liability by GetTotalUserSideBalance's balance_role filter -- tag this classification's balance_role (available/pending/locked if it is a real liability, memo if it is a deliberate non-liability memo/cost account) or confirm it should be is_system", r.Balance, r.NormalSide),
		})
	}
	return result
}

// runCheckUntaggedHolderKind surfaces journal types visible in the holder
// transaction view that were never tagged with a core.HolderTxKind (M-7
// follow-up, Team Lead 2026-08-27, board #49, docs/INVARIANTS.md I-44).
//
// This is NOT the same shape as role_less_liability above: an untagged
// holder_kind has no financial consequence (the read path already falls
// back to the disclosed 'other' bucket, never a raw internal identifier
// and never a silently wrong number), so core.JournalTypeInput.Validate
// deliberately does not refuse it at creation the way ClassificationInput.Validate
// refuses an empty balance_role. What was still missing is visibility: a
// deployer who forgets to tag their own journal type has no way to
// discover that fact other than a user noticing "other" in their
// transaction list. This check closes that gap -- detection only, exactly
// like role_less_liability; it does not change what `kind` resolves to on
// the wire, and the fix for a Finding here is a
// JournalTypeStore.SetHolderKind call, not a migration.
func (s *FullReconciliationService) runCheckUntaggedHolderKind(ctx context.Context) core.CheckResult {
	result := core.CheckResult{Name: "untagged_holder_kind", Passed: true, Complete: true, Findings: []core.Finding{}, CheckedAt: time.Now()}

	rows, err := s.querier.UntaggedHolderKindJournalTypes(ctx, s.cfg.UntaggedHolderKindPageLimit)
	if err != nil {
		result.Passed = false
		result.Findings = append(result.Findings, core.Finding{
			Description: "untagged holder_kind scan failed",
			Detail:      err.Error(),
		})
		return result
	}

	for _, r := range rows {
		result.Passed = false
		result.Findings = append(result.Findings, core.Finding{
			Description: fmt.Sprintf("journal type %q (%s) appears in the holder transaction view but has no holder_kind",
				r.Code, r.UID),
			Detail: "its transactions currently read kind=\"other\" (the disclosed fallback, docs/INVARIANTS.md I-44) -- tag it via JournalTypeStore.SetHolderKind if a more specific kind applies, or leave it if \"other\" is correct",
		})
	}
	return result
}

// runCheck7OrphanReservations checks for reservation rows whose journal_id (>0)
// does not point to an existing journal.
func (s *FullReconciliationService) runCheck7OrphanReservations(ctx context.Context) core.CheckResult {
	result := core.CheckResult{Name: "orphan_reservations", Passed: true, Complete: true, Findings: []core.Finding{}, CheckedAt: time.Now()}

	orphans, err := s.querier.OrphanReservations(ctx)
	if err != nil {
		result.Passed = false
		result.Findings = append(result.Findings, core.Finding{
			Description: "orphan reservation query failed",
			Detail:      err.Error(),
		})
		return result
	}

	for _, o := range orphans {
		result.Passed = false
		s.logger.Warn("service: reconcile: orphan reservation",
			"reservation_id", o.ID,
			"journal_id", o.JournalID,
		)
		result.Findings = append(result.Findings, core.Finding{
			Description: fmt.Sprintf("reservation %s (holder=%d, status=%s) references a non-existent journal",
				o.UID, o.AccountHolder, o.Status),
		})
	}
	return result
}

// runCheck9IdempotencyAudit scans for duplicate idempotency_key values in the
// journals table. The UNIQUE index should prevent any, but we verify defensively.
func (s *FullReconciliationService) runCheck9IdempotencyAudit(ctx context.Context) core.CheckResult {
	result := core.CheckResult{Name: "idempotency_uniqueness", Passed: true, Complete: true, Findings: []core.Finding{}, CheckedAt: time.Now()}

	dupes, err := s.querier.DuplicateIdempotencyKeys(ctx)
	if err != nil {
		result.Passed = false
		result.Findings = append(result.Findings, core.Finding{
			Description: "idempotency uniqueness audit query failed",
			Detail:      err.Error(),
		})
		return result
	}

	for _, d := range dupes {
		result.Passed = false
		s.logger.Warn("service: reconcile: duplicate idempotency key",
			"idempotency_key", d.IdempotencyKey,
			"first_journal_id", d.FirstID,
			"last_journal_id", d.LastID,
		)
		result.Findings = append(result.Findings, core.Finding{
			Description: fmt.Sprintf("idempotency_key %q appears %d times", d.IdempotencyKey, d.Occurrences),
		})
	}
	return result
}

// runCheck10StaleRollup checks for rollup_queue items whose claimed_until lease
// has expired, indicating a worker that crashed mid-process.
func (s *FullReconciliationService) runCheck10StaleRollup(ctx context.Context) core.CheckResult {
	result := core.CheckResult{Name: "stale_rollup_queue", Passed: true, Complete: true, Findings: []core.Finding{}, CheckedAt: time.Now()}

	thresholdMins := int(s.cfg.StaleRollupThreshold.Minutes())
	items, err := s.querier.StaleRollupItems(ctx, thresholdMins)
	if err != nil {
		result.Passed = false
		result.Findings = append(result.Findings, core.Finding{
			Description: "stale rollup queue query failed",
			Detail:      err.Error(),
		})
		return result
	}

	for _, item := range items {
		result.Passed = false
		s.logger.Warn("service: reconcile: stale rollup queue item", "item_id", item.ID)
		result.Findings = append(result.Findings, core.Finding{
			Description: fmt.Sprintf("rollup dimension (holder=%d, currency=%s, classification=%s) has stale lease (claimed_until=%s, failed=%d)",
				item.AccountHolder, s.externalCurrencyRef(ctx, item.CurrencyID), s.externalClassificationRef(ctx, item.ClassificationID),
				item.ClaimedUntil, item.FailedAttempts),
		})
	}
	return result
}

// runCheckSystemRollupIntegrity verifies system_rollups.total_balance
// against a fresh recompute straight from journal_entries (M4/I-23).
// RefreshSystemRollups populates this table via
// AggregateCheckpointsByClassification, which sums balance_checkpoints — so
// system_rollups inherits any checkpoint tampering wholesale if compared only
// against itself or against the checkpoints it was built from. This check
// never references balance_checkpoints: the "ground truth" it compares
// against is AccountingEquationRows, the same entries-only recompute check #4
// already performs.
func (s *FullReconciliationService) runCheckSystemRollupIntegrity(ctx context.Context) core.CheckResult {
	result := core.CheckResult{Name: "system_rollup_integrity", Passed: true, Complete: true, Findings: []core.Finding{}, CheckedAt: time.Now()}

	rollups, err := s.querier.ListSystemRollupsRaw(ctx)
	if err != nil {
		result.Passed = false
		result.Findings = append(result.Findings, core.Finding{
			Description: "system rollup list query failed",
			Detail:      err.Error(),
		})
		return result
	}

	equationRows, err := s.querier.AccountingEquationRows(ctx)
	if err != nil {
		result.Passed = false
		result.Findings = append(result.Findings, core.Finding{
			Description: "system rollup integrity: accounting equation query failed",
			Detail:      err.Error(),
		})
		return result
	}

	type dimKey struct {
		currencyID       int64
		classificationID int64
	}
	expected := make(map[dimKey]decimal.Decimal, len(equationRows))
	for _, r := range equationRows {
		// core.Delta is the sole authority for this computation (I-43); an
		// unknown normal_side surfaces as a failing Finding, not a silent
		// credit-normal default.
		balance, err := core.Delta(core.NormalSide(r.NormalSide), r.TotalDebit, r.TotalCredit)
		if err != nil {
			result.Passed = false
			result.Findings = append(result.Findings, core.Finding{
				Description: fmt.Sprintf("system rollup integrity: currency %s classification %s: %s",
					s.externalCurrencyRef(ctx, r.CurrencyID), s.externalClassificationRef(ctx, r.ClassificationID), err.Error()),
			})
			continue
		}
		expected[dimKey{r.CurrencyID, r.ClassificationID}] = balance
	}

	for _, roll := range rollups {
		// No matching AccountingEquationRow means journal_entries has zero
		// rows for this (currency, classification) pair — the true balance
		// is 0, not "unknown"; a non-zero system_rollups here is exactly the
		// M5 fabrication scenario (a rollup entry with no backing entries at
		// all).
		exp := expected[dimKey{roll.CurrencyID, roll.ClassificationID}]
		drift := roll.TotalBalance.Sub(exp)
		if drift.Abs().GreaterThan(s.cfg.EquationTolerance) {
			result.Passed = false
			result.Findings = append(result.Findings, core.Finding{
				Description: fmt.Sprintf("currency %s classification %s: system_rollups drift from entries",
					s.externalCurrencyRef(ctx, roll.CurrencyID), s.externalClassificationRef(ctx, roll.ClassificationID)),
				Detail: fmt.Sprintf("system_rollups=%s entries_recompute=%s drift=%s", roll.TotalBalance, exp, drift),
			})
		}
	}

	if len(result.Findings) == 0 {
		result.Findings = append(result.Findings, core.Finding{
			Description: fmt.Sprintf("system rollup integrity: %d rows verified against entries", len(rollups)),
		})
	}

	return result
}

// runCheckSnapshotIntegrity verifies the most recent balance_snapshots date
// against a fresh recompute straight from journal_entries (M4/I-23),
// bypassing balance_checkpoints entirely. Scoped to the latest snapshot_date
// (not full history) to bound cost — see
// docs/plans/2026-08-21-tamper-evident-ledger-design.md M4. Historical dates
// can be re-verified and repaired on demand via
// SnapshotBackfillService.BackfillSnapshots, which already recomputes from
// entries for an explicit date range; this check adds the missing
// *detection* half for the date most likely to still be actively read.
func (s *FullReconciliationService) runCheckSnapshotIntegrity(ctx context.Context) core.CheckResult {
	result := core.CheckResult{Name: "snapshot_integrity", Passed: true, Complete: true, Findings: []core.Finding{}, CheckedAt: time.Now()}

	rows, err := s.querier.LatestSnapshotDrift(ctx, s.cfg.SnapshotIntegrityPageLimit)
	if err != nil {
		result.Passed = false
		result.Findings = append(result.Findings, core.Finding{
			Description: "snapshot integrity query failed",
			Detail:      err.Error(),
		})
		return result
	}

	for _, r := range rows {
		result.Passed = false
		result.Findings = append(result.Findings, core.Finding{
			Description: fmt.Sprintf("holder %d currency %s classification %s: balance_snapshots drift from entries on %s",
				r.AccountHolder, s.externalCurrencyRef(ctx, r.CurrencyID), s.externalClassificationRef(ctx, r.ClassificationID),
				r.SnapshotDate.Format("2006-01-02")),
			Detail: fmt.Sprintf("stored=%s entries_recompute=%s drift=%s", r.StoredBalance, r.RecomputedBalance, r.StoredBalance.Sub(r.RecomputedBalance)),
		})
	}

	if len(rows) >= s.cfg.SnapshotIntegrityPageLimit {
		// The page limit was hit: there may be more drifting rows for this
		// date than we fetched, so this check cannot claim full coverage —
		// same fail-closed-by-construction shape as check #2's Complete field.
		result.Complete = false
		result.Findings = append(result.Findings, core.Finding{
			Description: fmt.Sprintf("snapshot integrity scan incomplete: hit page limit (%d rows)", s.cfg.SnapshotIntegrityPageLimit),
		})
	} else if len(result.Findings) == 0 {
		result.Findings = append(result.Findings, core.Finding{
			Description: "snapshot integrity: no drift found for the most recent snapshot date",
		})
	}

	return result
}

// runCheckUnauthorizedJournals is the unauthorized_journals check
// (contracts §W2-2, docs/INVARIANTS.md I-32): it samples up to
// cfg.UnauthorizedJournalsPageLimit journals (oldest first -- there is no
// persisted resume cursor yet, unlike checkpoint_balance's C4b) and, for
// every one that CLAIMS to be signed (auth_key_id non-empty), recomputes
// its canonical digest and confirms the stored signature still verifies.
//
// Deliberately scoped like ledger-cli verify's step 4
// (service/attest_verify.go), not like VerifiedBalanceReader: a journal
// with an EMPTY auth_key_id is a coverage gap (never signed -- pre-P5
// history, or a write path that still posts unsigned), not tamper
// evidence, and is skipped rather than flagged. Flagging every unsigned
// journal here would make this check permanently red on any fleet with
// pre-P5 history, drowning out the signal this check exists to surface:
// a journal that CLAIMS a signature but no longer verifies means either
// its stored fields were corrupted/edited after the fact, or it was
// forged with a mismatched key -- both real tamper evidence.
//
// If SetAuthCheck was never called (journals or verifier nil), the check
// is skipped outright (Complete=false) -- there is nothing to verify
// against, and reporting Passed=true would be indistinguishable from
// "everything was checked and found clean" (working-agreements §3).
func (s *FullReconciliationService) runCheckUnauthorizedJournals(ctx context.Context) core.CheckResult {
	result := core.CheckResult{Name: "unauthorized_journals", Passed: true, Complete: true, Findings: []core.Finding{}, CheckedAt: time.Now()}

	if s.journals == nil || s.verifier == nil {
		result.Complete = false
		result.Findings = append(result.Findings, core.Finding{
			Description: "check skipped: no AuthVerifier configured (ledger.WithAttestor was never called, or SetAuthCheck was never wired)",
		})
		return result
	}

	journalList, _, err := s.journals.ListJournals(ctx, "", int32(s.cfg.UnauthorizedJournalsPageLimit))
	if err != nil {
		result.Passed = false
		result.Findings = append(result.Findings, core.Finding{
			Description: "unauthorized journals scan failed",
			Detail:      err.Error(),
		})
		return result
	}

	checked := 0
	for _, j := range journalList {
		if j.AuthKeyID == "" {
			continue
		}
		checked++

		_, entries, err := s.journals.GetJournal(ctx, j.UID)
		if err != nil {
			result.Passed = false
			result.Findings = append(result.Findings, core.Finding{
				Description: "unauthorized journals scan: refetch for authorization check failed",
				Detail:      err.Error(),
			})
			continue
		}

		input := core.JournalInputFromRecord(j, entries)
		if err := core.VerifyJournalAuth(ctx, s.verifier, input, j.EffectiveAt, j.AuthDigest, j.AuthSignature, j.AuthKeyID); err != nil {
			result.Passed = false
			s.logger.Warn("service: reconcile: unauthorized journal", "journal_uid", j.UID, "error", err)
			result.Findings = append(result.Findings, core.Finding{
				Description: fmt.Sprintf("journal %s: claims a signature but fails authorization verification", j.UID),
			})
		}
	}

	if len(journalList) >= s.cfg.UnauthorizedJournalsPageLimit {
		// No resume cursor exists yet, so this is not "partial progress
		// through the fleet" like checkpoint_balance's C4b -- it is the
		// SAME oldest slice every run. Complete=false says so honestly
		// instead of implying broader coverage than this check can
		// currently provide.
		result.Complete = false
		result.Findings = append(result.Findings, core.Finding{
			Description: fmt.Sprintf("unauthorized journals scan incomplete: hit page limit (%d journals, oldest-first, no resume cursor yet)", s.cfg.UnauthorizedJournalsPageLimit),
		})
	} else if result.Passed {
		result.Findings = append(result.Findings, core.Finding{
			Description: fmt.Sprintf("unauthorized journals scan: %d signed journal(s) verified out of %d scanned", checked, len(journalList)),
		})
	}

	return result
}

type CurrencyReconcileTotals struct {
	CurrencyID int64
	Debit      decimal.Decimal
	Credit     decimal.Decimal
}

// GlobalSummer sums all debits and credits globally, grouped by currency.
type GlobalSummer interface {
	SumGlobalDebitCreditByCurrency(ctx context.Context) ([]CurrencyReconcileTotals, error)
}

// AccountEntrySummer sums a specific account's entries, bounded per
// classification by that classification's checkpoint watermark
// (id <= last_entry_id) — see ReconcileAccount.
type AccountEntrySummer interface {
	SumEntriesByAccountClassification(ctx context.Context, holder, currencyID int64) (debitByClass, creditByClass map[int64]decimal.Decimal, err error)
}

// CheckpointReader reads checkpoints for reconciliation.
type CheckpointReader interface {
	GetCheckpoints(ctx context.Context, holder, currencyID int64) ([]BalanceCheckpoint, error)
}

// ReconciliationService verifies accounting integrity.
type ReconciliationService struct {
	global          GlobalSummer
	accountEntries  AccountEntrySummer
	checkpoints     CheckpointReader
	classifications ClassificationLister
	logger          core.Logger
	metrics         core.Metrics
}

// NewReconciliationService creates a new ReconciliationService.
func NewReconciliationService(
	global GlobalSummer,
	accountEntries AccountEntrySummer,
	checkpoints CheckpointReader,
	classifications ClassificationLister,
	engine *core.Engine,
) *ReconciliationService {
	return &ReconciliationService{
		global:          global,
		accountEntries:  accountEntries,
		checkpoints:     checkpoints,
		classifications: classifications,
		logger:          engine.Logger(),
		metrics:         engine.Metrics(),
	}
}

// CheckAccountingEquation verifies SUM(all debits) == SUM(all credits).
func (s *ReconciliationService) CheckAccountingEquation(ctx context.Context) (*core.ReconcileResult, error) {
	totals, err := s.global.SumGlobalDebitCreditByCurrency(ctx)
	if err != nil {
		return nil, fmt.Errorf("service: reconcile: sum global: %w", err)
	}

	result := &core.ReconcileResult{
		Balanced:  true,
		Gap:       decimal.Zero,
		CheckedAt: time.Now(),
	}

	for _, total := range totals {
		gap := total.Debit.Sub(total.Credit)
		if gap.IsZero() {
			continue
		}

		result.Balanced = false
		result.Gap = result.Gap.Add(gap.Abs())
		currencyUID, uidErr := s.classifications.CurrencyUIDByID(ctx, total.CurrencyID)
		if uidErr != nil {
			return nil, fmt.Errorf("service: reconcile: resolve currency %d: %w", total.CurrencyID, uidErr)
		}
		result.Details = append(result.Details, core.ReconcileDetail{
			CurrencyUID: currencyUID,
			Expected:    total.Debit,
			Actual:      total.Credit,
			Drift:       gap,
		})

		s.logger.Warn("service: reconcile: accounting equation imbalance",
			"currency_id", total.CurrencyID,
			"debit_total", total.Debit.String(),
			"credit_total", total.Credit.String(),
			"gap", gap.String(),
		)
		s.metrics.ReconcileGap(total.CurrencyID, gap)
	}

	s.metrics.ReconcileCompleted(result.Balanced)
	return result, nil
}

// ReconcileAccount verifies checkpoint balances vs actual entry sums for a
// specific account. The entry sums are bounded per classification by that
// classification's checkpoint watermark (id <= last_entry_id, enforced in the
// adapter query), so the comparison is exact and immune to in-flight rollups
// — entries posted after the checkpoint was materialized are not "drift".
func (s *ReconciliationService) ReconcileAccount(ctx context.Context, holder int64, currencyUID string) (*core.ReconcileResult, error) {
	currencyID, err := s.classifications.CurrencyIDByUID(ctx, currencyUID)
	if err != nil {
		return nil, fmt.Errorf("service: reconcile account: resolve currency: %w", err)
	}
	// Get classifications for normal_side
	clsList, err := s.classifications.ClassificationDims(ctx)
	if err != nil {
		return nil, fmt.Errorf("service: reconcile account: list classifications: %w", err)
	}
	normalSides := make(map[int64]core.NormalSide, len(clsList))
	classDimByID := make(map[int64]ClassificationDim, len(clsList))
	for _, c := range clsList {
		normalSides[c.ID] = c.NormalSide
		classDimByID[c.ID] = c
	}

	// Get checkpoints
	cps, err := s.checkpoints.GetCheckpoints(ctx, holder, currencyID)
	if err != nil {
		return nil, fmt.Errorf("service: reconcile account: get checkpoints: %w", err)
	}

	// Get actual entry sums
	debitByClass, creditByClass, err := s.accountEntries.SumEntriesByAccountClassification(ctx, holder, currencyID)
	if err != nil {
		return nil, fmt.Errorf("service: reconcile account: sum entries: %w", err)
	}

	result := &core.ReconcileResult{
		Balanced:  true,
		Gap:       decimal.Zero,
		CheckedAt: time.Now(),
	}

	checkpointByClass := make(map[int64]BalanceCheckpoint, len(cps))
	classificationSet := make(map[int64]struct{}, len(cps)+len(debitByClass)+len(creditByClass))
	for _, cp := range cps {
		checkpointByClass[cp.ClassificationID] = cp
		classificationSet[cp.ClassificationID] = struct{}{}
	}
	for classID := range debitByClass {
		classificationSet[classID] = struct{}{}
	}
	for classID := range creditByClass {
		classificationSet[classID] = struct{}{}
	}

	classificationIDs := make([]int64, 0, len(classificationSet))
	for classID := range classificationSet {
		classificationIDs = append(classificationIDs, classID)
	}
	sort.Slice(classificationIDs, func(i, j int) bool { return classificationIDs[i] < classificationIDs[j] })

	// For each classification referenced by either checkpoints or entries, compute the
	// expected balance from entries and compare it to the checkpointed balance.
	for _, classID := range classificationIDs {
		debit := debitByClass[classID]
		credit := creditByClass[classID]

		// core.Delta is the sole authority for this computation (I-43).
		ns := normalSides[classID]
		expected, err := core.Delta(ns, debit, credit)
		if err != nil {
			return nil, fmt.Errorf("service: reconcile account: classification %d: %w", classID, err)
		}

		actual := decimal.Zero
		if cp, ok := checkpointByClass[classID]; ok {
			actual = cp.Balance
		}

		drift := actual.Sub(expected)
		if !drift.IsZero() {
			result.Balanced = false
			result.Gap = result.Gap.Add(drift.Abs())
			classUID := ""
			if dim, ok := classDimByID[classID]; ok {
				classUID = dim.UID
			}
			result.Details = append(result.Details, core.ReconcileDetail{
				AccountHolder:     holder,
				CurrencyUID:       currencyUID,
				ClassificationUID: classUID,
				Expected:          expected,
				Actual:            actual,
				Drift:             drift,
			})

			s.logger.Warn("service: reconcile account: checkpoint drift",
				"holder", holder,
				"currency_id", currencyID,
				"classification_id", classID,
				"expected", expected.String(),
				"actual", actual.String(),
				"drift", drift.String(),
			)
			s.metrics.ReconcileGap(currencyID, drift)
		}
	}

	s.metrics.ReconcileCompleted(result.Balanced)
	return result, nil
}
