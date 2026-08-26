package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// ---------------------------------------------------------------------------
// Mock implementation of ReconcileQuerier
// ---------------------------------------------------------------------------

type mockReconcileQuerier struct {
	orphanCount      int64
	orphanSamples    []OrphanEntrySample
	equationRows     []AccountingEquationRow
	settlementViols  []SettlementNettingViolation
	negativeAccounts []NegativeBalanceAccount
	// roleLessLiabilities drives the role_less_credit_liability check (M-4 fix).
	roleLessLiabilities []RoleLessCreditLiability
	orphanReservs       []OrphanReservation
	staleItems          []StaleRollupItem
	dupeKeys            []DuplicateIdempotencyKey

	// checkpointAccounts must be pre-sorted ascending by (AccountHolder,
	// CurrencyID) — ListCheckpointAccountsPage paginates over it using the
	// same keyset semantics as the real SQL query.
	checkpointAccounts  []CheckpointAccountKey
	checkpointPageCalls int

	// unbalancedJournals drives the journal_dr_cr check (M1 fix): genuine
	// per-journal balance violations, as opposed to global_dr_cr_equality's
	// global equality.
	unbalancedJournals []UnbalancedJournal

	// cursor* fields back GetScanCursor/SetScanCursor. cursorSet mirrors "a
	// row exists in reconcile_scan_cursors" — false means GetScanCursor must
	// return the (cursorStartHolder, cursorStartCurrency, false, 0) default,
	// the same behavior the real adapter gives on zero rows. cursorLapScanned
	// (M-1 fix) is the cumulative pairs-verified counter for the current lap.
	cursorSet           bool
	cursorAfterHolder   int64
	cursorAfterCurrency int64
	cursorLapDirty      bool
	cursorLapScanned    int64
	getScanCursorCalls  int
	// errCountCheckpointAccountPairs forces CountCheckpointAccountPairs to
	// fail, when set.
	errCountCheckpointAccountPairs error
	setScanCursorCalls             int

	systemRollups  []SystemRollupRow
	snapshotDrifts []SnapshotDriftRow

	// force errors
	errOrphanCount         error
	errOrphanSample        error
	errEquation            error
	errSettlement          error
	errNegBal              error
	errRoleLessLiabilities error
	errOrphanReservs       error
	errDupeKeys            error
	errStaleItems          error
	errCheckpointPage      error
	errUnbalancedCount     error
	errUnbalancedSample    error
	errGetScanCursor       error
	errSetScanCursor       error
	errSystemRollups       error
	errSnapshotDrifts      error
}

func (m *mockReconcileQuerier) OrphanEntriesCount(_ context.Context) (int64, error) {
	return m.orphanCount, m.errOrphanCount
}
func (m *mockReconcileQuerier) OrphanEntriesSample(_ context.Context) ([]OrphanEntrySample, error) {
	return m.orphanSamples, m.errOrphanSample
}
func (m *mockReconcileQuerier) AccountingEquationRows(_ context.Context) ([]AccountingEquationRow, error) {
	return m.equationRows, m.errEquation
}
func (m *mockReconcileQuerier) SettlementNettingViolations(_ context.Context, _ string, _ int) ([]SettlementNettingViolation, error) {
	return m.settlementViols, m.errSettlement
}
func (m *mockReconcileQuerier) NegativeBalanceAccounts(_ context.Context, _ int) ([]NegativeBalanceAccount, error) {
	return m.negativeAccounts, m.errNegBal
}
func (m *mockReconcileQuerier) RoleLessCreditLiabilities(_ context.Context, _ int) ([]RoleLessCreditLiability, error) {
	return m.roleLessLiabilities, m.errRoleLessLiabilities
}
func (m *mockReconcileQuerier) OrphanReservations(_ context.Context) ([]OrphanReservation, error) {
	return m.orphanReservs, m.errOrphanReservs
}
func (m *mockReconcileQuerier) DuplicateIdempotencyKeys(_ context.Context) ([]DuplicateIdempotencyKey, error) {
	return m.dupeKeys, m.errDupeKeys
}
func (m *mockReconcileQuerier) StaleRollupItems(_ context.Context, _ int) ([]StaleRollupItem, error) {
	return m.staleItems, m.errStaleItems
}
func (m *mockReconcileQuerier) ListCheckpointAccountsPage(_ context.Context, afterHolder, afterCurrency int64, pageLimit int) ([]CheckpointAccountKey, error) {
	m.checkpointPageCalls++
	if m.errCheckpointPage != nil {
		return nil, m.errCheckpointPage
	}
	var page []CheckpointAccountKey
	for _, k := range m.checkpointAccounts {
		if k.AccountHolder > afterHolder || (k.AccountHolder == afterHolder && k.CurrencyID > afterCurrency) {
			page = append(page, k)
			if len(page) >= pageLimit {
				break
			}
		}
	}
	return page, nil
}
func (m *mockReconcileQuerier) UnbalancedJournalsCount(_ context.Context) (int64, error) {
	if m.errUnbalancedCount != nil {
		return 0, m.errUnbalancedCount
	}
	return int64(len(m.unbalancedJournals)), nil
}
func (m *mockReconcileQuerier) UnbalancedJournalsSample(_ context.Context) ([]UnbalancedJournal, error) {
	return m.unbalancedJournals, m.errUnbalancedSample
}

func (m *mockReconcileQuerier) GetScanCursor(_ context.Context, _ string) (int64, int64, bool, int64, error) {
	m.getScanCursorCalls++
	if m.errGetScanCursor != nil {
		return 0, 0, false, 0, m.errGetScanCursor
	}
	if !m.cursorSet {
		// Mirrors the real adapter's zero-rows default.
		return math.MinInt64, math.MinInt64, false, 0, nil
	}
	return m.cursorAfterHolder, m.cursorAfterCurrency, m.cursorLapDirty, m.cursorLapScanned, nil
}

func (m *mockReconcileQuerier) SetScanCursor(_ context.Context, _ string, afterHolder, afterCurrency int64, lapDirty bool, lapScanned int64) error {
	m.setScanCursorCalls++
	if m.errSetScanCursor != nil {
		return m.errSetScanCursor
	}
	m.cursorSet = true
	m.cursorAfterHolder = afterHolder
	m.cursorAfterCurrency = afterCurrency
	m.cursorLapDirty = lapDirty
	m.cursorLapScanned = lapScanned
	return nil
}

// CountCheckpointAccountPairs mirrors the real adapter's semantics: the
// total distinct pairs currently reachable via ListCheckpointAccountsPage,
// i.e. the same checkpointAccounts population the mock's paginator walks.
func (m *mockReconcileQuerier) CountCheckpointAccountPairs(_ context.Context) (int64, error) {
	if m.errCountCheckpointAccountPairs != nil {
		return 0, m.errCountCheckpointAccountPairs
	}
	return int64(len(m.checkpointAccounts)), nil
}

func (m *mockReconcileQuerier) ListSystemRollupsRaw(_ context.Context) ([]SystemRollupRow, error) {
	return m.systemRollups, m.errSystemRollups
}

func (m *mockReconcileQuerier) LatestSnapshotDrift(_ context.Context, pageLimit int) ([]SnapshotDriftRow, error) {
	if m.errSnapshotDrifts != nil {
		return nil, m.errSnapshotDrifts
	}
	if len(m.snapshotDrifts) > pageLimit {
		return m.snapshotDrifts[:pageLimit], nil
	}
	return m.snapshotDrifts, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func buildFullSvc(t *testing.T, global GlobalSummer, querier ReconcileQuerier, cfg FullReconciliationConfig) *FullReconciliationService {
	t.Helper()
	engine := core.NewEngine()
	basic := NewReconciliationService(global, nil, nil, nil, engine)
	return NewFullReconciliationService(basic, querier, cfg, engine)
}

// cleanQuerier returns a querier that reports no violations for any check.
func cleanQuerier() *mockReconcileQuerier {
	return &mockReconcileQuerier{}
}

// balancedGlobalSummer reports globally balanced debits/credits.
func balancedGlobalSummer() *mockGlobalSummer {
	return &mockGlobalSummer{
		totals: []CurrencyReconcileTotals{
			{CurrencyID: 1, Debit: decimal.NewFromInt(1000), Credit: decimal.NewFromInt(1000)},
		},
	}
}

// ---------------------------------------------------------------------------
// RunFullReconciliation — overall structure
// ---------------------------------------------------------------------------

func TestFullReconciliation_AllPass(t *testing.T) {
	svc := buildFullSvc(t, balancedGlobalSummer(), cleanQuerier(), FullReconciliationConfig{})
	report, err := svc.RunFullReconciliation(context.Background())
	require.NoError(t, err)
	assert.True(t, report.OverallPassed)
	assert.Len(t, report.Checks, 14, "should run exactly 14 checks (M-4 fix added role_less_credit_liability)")

	// OverallPassed reports violations found; it is NOT a clean bill of
	// health. unauthorized_journals is skipped (buildFullSvc never calls
	// SetAuthCheck, contracts §W2-2's own "skip rather than run with no
	// verifier" contract) -- so the run must admit incomplete coverage
	// rather than let a never-executed check count as verified.
	assert.False(t, report.FullCoverage,
		"unauthorized_journals is skipped, so the suite cannot claim full coverage")
	skippedChecks := map[string]bool{"unauthorized_journals": true}
	for _, c := range report.Checks {
		if skippedChecks[c.Name] {
			assert.False(t, c.Complete, "a skipped check is never complete")
		} else {
			assert.True(t, c.Complete, "check %s ran but did not report coverage", c.Name)
		}
	}
}

// TestFullReconciliation_FullCoverageCanBeTrue pins the fix for operability.md's
// "full_coverage 永远为假" Major: with every check wired (including
// unauthorized_journals via SetAuthCheck) and nothing capped or skipped,
// FullCoverage must actually be able to read true. Before this fix, the
// permanently-skipped check #8 ("pending_journal_timeout") one-voted
// FullCoverage to false on every possible run, no matter how the rest of the
// suite was configured -- this test could not have passed against the old
// code no matter what it wired up, which is exactly the "signal that can
// never be true carries no information" failure the removal fixes.
func TestFullReconciliation_FullCoverageCanBeTrue(t *testing.T) {
	q := cleanQuerier()
	engine := core.NewEngine()
	basic := NewReconciliationService(balancedGlobalSummer(), nil, nil, nil, engine)
	svc := NewFullReconciliationService(basic, q, FullReconciliationConfig{}, engine)
	svc.SetAuthCheck(&fakeJournalQueryProvider{}, alwaysValidVerifier{})

	report, err := svc.RunFullReconciliation(context.Background())
	require.NoError(t, err)
	assert.True(t, report.OverallPassed)
	assert.True(t, report.FullCoverage,
		"every check ran to completion with nothing capped or skipped -- FullCoverage must be able to be true")
	for _, c := range report.Checks {
		assert.True(t, c.Complete, "check %s: expected Complete=true", c.Name)
	}
}

// fakeJournalQueryProvider satisfies core.QueryProvider with an empty
// journal list, enough to drive runCheckUnauthorizedJournals's "wired but
// nothing to check" path for TestFullReconciliation_FullCoverageCanBeTrue.
// Only the two methods FullReconciliationService actually calls
// (ListJournals/GetJournal) need real behavior; the rest of the composed
// interface is never reached by that check.
type fakeJournalQueryProvider struct{ core.QueryProvider }

func (fakeJournalQueryProvider) ListJournals(_ context.Context, _ string, _ int32) ([]core.Journal, string, error) {
	return nil, "", nil
}

func (fakeJournalQueryProvider) GetJournal(_ context.Context, _ string) (*core.Journal, []core.Entry, error) {
	return nil, nil, nil
}

// alwaysValidVerifier is a core.AuthVerifier that is never actually invoked
// in TestFullReconciliation_FullCoverageCanBeTrue (no journals claim a
// signature), but must be non-nil for SetAuthCheck to arm the check at all.
type alwaysValidVerifier struct{}

func (alwaysValidVerifier) Verify(_ context.Context, _, _ []byte, _ string) error {
	return fmt.Errorf("alwaysValidVerifier: unexpected call")
}

// recordingReconcileMetrics captures ReconcileCheckResult calls for testing.
type recordingReconcileMetrics struct {
	core.Metrics
	results map[string]bool
}

func (m *recordingReconcileMetrics) ReconcileCheckResult(checkName string, passed bool) {
	if m.results == nil {
		m.results = make(map[string]bool)
	}
	m.results[checkName] = passed
}

// TestFullReconciliation_EmitsMetricsPerCheck verifies that
// FullReconciliationService.metrics — previously injected but never used — is
// now exercised: one ReconcileCheckResult call per check in the suite.
func TestFullReconciliation_EmitsMetricsPerCheck(t *testing.T) {
	metrics := &recordingReconcileMetrics{Metrics: core.NopMetrics()}
	engine := core.NewEngine(core.WithMetrics(metrics))
	basic := NewReconciliationService(balancedGlobalSummer(), nil, nil, nil, engine)
	svc := NewFullReconciliationService(basic, cleanQuerier(), FullReconciliationConfig{}, engine)

	report, err := svc.RunFullReconciliation(context.Background())
	require.NoError(t, err)

	require.Len(t, metrics.results, len(report.Checks), "one ReconcileCheckResult call per check")
	for _, c := range report.Checks {
		green, ok := metrics.results[c.Name]
		require.True(t, ok, "check %q must have emitted a metric", c.Name)
		// The metric reports Passed && Complete so alerting fails closed: an
		// incomplete or skipped check must never look green on a dashboard.
		assert.Equal(t, c.Passed && c.Complete, green,
			"check %q metric must fold coverage into the green signal", c.Name)
	}

	// Concretely: unauthorized_journals is skipped in this config (no
	// SetAuthCheck call), so its metric must be false even though its
	// CheckResult.Passed is true.
	assert.False(t, metrics.results["unauthorized_journals"],
		"a skipped check must not emit a green metric")
}

func TestFullReconciliation_OneFailureFlipsOverall(t *testing.T) {
	q := cleanQuerier()
	q.orphanCount = 5
	q.orphanSamples = []OrphanEntrySample{{EntryID: 42, JournalID: 99}}

	svc := buildFullSvc(t, balancedGlobalSummer(), q, FullReconciliationConfig{})
	report, err := svc.RunFullReconciliation(context.Background())
	require.NoError(t, err)
	assert.False(t, report.OverallPassed, "overall should fail when orphan check fails")
}

// ---------------------------------------------------------------------------
// Check #3 — Orphan entries
// ---------------------------------------------------------------------------

func TestCheck3OrphanEntries_Clean(t *testing.T) {
	svc := buildFullSvc(t, nil, cleanQuerier(), FullReconciliationConfig{})
	result := svc.runCheck3OrphanEntries(context.Background())
	assert.True(t, result.Passed)
	assert.Empty(t, result.Findings)
}

func TestCheck3OrphanEntries_Violation(t *testing.T) {
	q := cleanQuerier()
	q.orphanCount = 2
	q.orphanSamples = []OrphanEntrySample{
		{EntryID: 10, JournalID: 99},
		{EntryID: 11, JournalID: 100},
	}

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheck3OrphanEntries(context.Background())
	assert.False(t, result.Passed)
	// 1 summary finding + 1 "samples recorded in logs" finding. Per-sample
	// internal row ids are ops-log material, not report material (I-18).
	assert.Len(t, result.Findings, 2)
	for _, f := range result.Findings {
		assert.NotContains(t, f.Description, "99", "internal journal id must not leak into the report")
	}
}

func TestCheck3OrphanEntries_QueryError(t *testing.T) {
	q := cleanQuerier()
	q.errOrphanCount = errors.New("db error")

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheck3OrphanEntries(context.Background())
	assert.False(t, result.Passed)
	require.Len(t, result.Findings, 1)
	assert.Contains(t, result.Findings[0].Detail, "db error")
}

// ---------------------------------------------------------------------------
// Check #4 — Accounting equation
// ---------------------------------------------------------------------------

func TestCheck4AccountingEquation_Balanced(t *testing.T) {
	q := cleanQuerier()
	// One debit-normal and one credit-normal classification in currency 1.
	// Debit-normal net = 1000 - 0 = 1000
	// Credit-normal net = 1000 - 0 = 1000
	// 1000 == 1000 → balanced.
	q.equationRows = []AccountingEquationRow{
		{CurrencyID: 1, ClassificationID: 1, NormalSide: "debit", TotalDebit: decimal.NewFromInt(1000), TotalCredit: decimal.Zero},
		{CurrencyID: 1, ClassificationID: 2, NormalSide: "credit", TotalDebit: decimal.Zero, TotalCredit: decimal.NewFromInt(1000)},
	}

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheck4AccountingEquation(context.Background())
	assert.True(t, result.Passed)
	assert.Empty(t, result.Findings)
}

func TestCheck4AccountingEquation_Imbalance(t *testing.T) {
	q := cleanQuerier()
	// Debit-normal net = 1000; credit-normal net = 900 → diff = 100
	q.equationRows = []AccountingEquationRow{
		{CurrencyID: 1, ClassificationID: 1, NormalSide: "debit", TotalDebit: decimal.NewFromInt(1000), TotalCredit: decimal.Zero},
		{CurrencyID: 1, ClassificationID: 2, NormalSide: "credit", TotalDebit: decimal.Zero, TotalCredit: decimal.NewFromInt(900)},
	}

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheck4AccountingEquation(context.Background())
	assert.False(t, result.Passed)
	require.Len(t, result.Findings, 1)
	assert.Contains(t, result.Findings[0].Description, "accounting equation imbalance")
	assert.NotContains(t, result.Findings[0].Description, "currency 1", "internal currency id must not leak (I-18)")
}

func TestCheck4AccountingEquation_MultipleCurrencies(t *testing.T) {
	q := cleanQuerier()
	// Currency 1 balanced, Currency 2 imbalanced.
	q.equationRows = []AccountingEquationRow{
		{CurrencyID: 1, ClassificationID: 1, NormalSide: "debit", TotalDebit: decimal.NewFromInt(500), TotalCredit: decimal.Zero},
		{CurrencyID: 1, ClassificationID: 2, NormalSide: "credit", TotalDebit: decimal.Zero, TotalCredit: decimal.NewFromInt(500)},
		{CurrencyID: 2, ClassificationID: 3, NormalSide: "debit", TotalDebit: decimal.NewFromInt(200), TotalCredit: decimal.Zero},
		{CurrencyID: 2, ClassificationID: 4, NormalSide: "credit", TotalDebit: decimal.Zero, TotalCredit: decimal.NewFromInt(150)},
	}

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheck4AccountingEquation(context.Background())
	assert.False(t, result.Passed)
	require.Len(t, result.Findings, 1)
	assert.Contains(t, result.Findings[0].Description, "accounting equation imbalance")
	assert.NotContains(t, result.Findings[0].Description, "currency 2", "internal currency id must not leak (I-18)")
}

func TestCheck4AccountingEquation_QueryError(t *testing.T) {
	q := cleanQuerier()
	q.errEquation = errors.New("timeout")

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheck4AccountingEquation(context.Background())
	assert.False(t, result.Passed)
	assert.Contains(t, result.Findings[0].Detail, "timeout")
}

// ---------------------------------------------------------------------------
// Check #5 — Settlement netting
// ---------------------------------------------------------------------------

func TestCheck5SettlementNetting_Clean(t *testing.T) {
	svc := buildFullSvc(t, nil, cleanQuerier(), FullReconciliationConfig{})
	result := svc.runCheck5SettlementNetting(context.Background())
	assert.True(t, result.Passed)
}

func TestCheck5SettlementNetting_Violation(t *testing.T) {
	q := cleanQuerier()
	q.settlementViols = []SettlementNettingViolation{
		{CurrencyID: 1, NetBalance: decimal.NewFromFloat(0.5)},
	}

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheck5SettlementNetting(context.Background())
	assert.False(t, result.Passed)
	require.Len(t, result.Findings, 1)
	assert.Contains(t, result.Findings[0].Description, "settlement classification net balance is non-zero")
	assert.NotContains(t, result.Findings[0].Description, "currency 1", "internal currency id must not leak (I-18)")
}

func TestCheck5SettlementNetting_QueryError(t *testing.T) {
	q := cleanQuerier()
	q.errSettlement = errors.New("conn refused")

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheck5SettlementNetting(context.Background())
	assert.False(t, result.Passed)
	assert.Contains(t, result.Findings[0].Detail, "conn refused")
}

// ---------------------------------------------------------------------------
// Check #6 — Non-negative user balances
// ---------------------------------------------------------------------------

func TestCheck6NonNegativeBalances_Clean(t *testing.T) {
	svc := buildFullSvc(t, nil, cleanQuerier(), FullReconciliationConfig{})
	result := svc.runCheck6NonNegativeBalances(context.Background())
	assert.True(t, result.Passed)
}

func TestCheck6NonNegativeBalances_Violation(t *testing.T) {
	q := cleanQuerier()
	q.negativeAccounts = []NegativeBalanceAccount{
		{AccountHolder: 42, CurrencyID: 1, ClassificationID: 5, NormalSide: "credit", Balance: decimal.NewFromFloat(-10)},
	}

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheck6NonNegativeBalances(context.Background())
	assert.False(t, result.Passed)
	require.Len(t, result.Findings, 1)
	assert.Contains(t, result.Findings[0].Description, "holder 42")
	assert.Contains(t, result.Findings[0].Detail, "-10")
}

func TestCheck6NonNegativeBalances_QueryError(t *testing.T) {
	q := cleanQuerier()
	q.errNegBal = errors.New("scan failed")

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheck6NonNegativeBalances(context.Background())
	assert.False(t, result.Passed)
	assert.Contains(t, result.Findings[0].Detail, "scan failed")
}

// ---------------------------------------------------------------------------
// role_less_credit_liability — mistagged liability classification (M-4 fix)
// ---------------------------------------------------------------------------

func TestRoleLessCreditLiability_Clean(t *testing.T) {
	svc := buildFullSvc(t, nil, cleanQuerier(), FullReconciliationConfig{})
	result := svc.runCheckRoleLessCreditLiability(context.Background())
	assert.True(t, result.Passed)
	assert.True(t, result.Complete)
}

// TestRoleLessCreditLiability_Violation pins the M-1-shaped danger direction
// M-4 closes: a nonzero balance on a credit-normal, non-system, user-side
// classification with no balance_role must surface as a Finding instead of
// silently understating SolvencyReport.Liability.
func TestRoleLessCreditLiability_Violation(t *testing.T) {
	q := cleanQuerier()
	q.roleLessLiabilities = []RoleLessCreditLiability{
		{AccountHolder: 42, CurrencyID: 1, ClassificationID: 7, Balance: decimal.NewFromInt(100)},
	}

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheckRoleLessCreditLiability(context.Background())
	assert.False(t, result.Passed)
	require.Len(t, result.Findings, 1)
	assert.Contains(t, result.Findings[0].Description, "holder 42")
	assert.Contains(t, result.Findings[0].Description, "no balance_role")
	assert.Contains(t, result.Findings[0].Detail, "100")
	assert.Contains(t, result.Findings[0].Detail, "SolvencyReport.Liability")
}

func TestRoleLessCreditLiability_QueryError(t *testing.T) {
	q := cleanQuerier()
	q.errRoleLessLiabilities = errors.New("scan failed")

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheckRoleLessCreditLiability(context.Background())
	assert.False(t, result.Passed)
	assert.Contains(t, result.Findings[0].Detail, "scan failed")
}

// ---------------------------------------------------------------------------
// Check #7 — Orphan reservations
// ---------------------------------------------------------------------------

func TestCheck7OrphanReservations_Clean(t *testing.T) {
	svc := buildFullSvc(t, nil, cleanQuerier(), FullReconciliationConfig{})
	result := svc.runCheck7OrphanReservations(context.Background())
	assert.True(t, result.Passed)
}

func TestCheck7OrphanReservations_Violation(t *testing.T) {
	q := cleanQuerier()
	q.orphanReservs = []OrphanReservation{
		{ID: 7, UID: "res-uid-7", AccountHolder: 99, CurrencyID: 1, Status: "settled", JournalID: 42},
	}

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheck7OrphanReservations(context.Background())
	assert.False(t, result.Passed)
	require.Len(t, result.Findings, 1)
	assert.Contains(t, result.Findings[0].Description, "reservation res-uid-7")
	assert.NotContains(t, result.Findings[0].Description, "journal 42", "internal journal id must not leak (I-18)")
}

func TestCheck7OrphanReservations_QueryError(t *testing.T) {
	q := cleanQuerier()
	q.errOrphanReservs = errors.New("timeout")

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheck7OrphanReservations(context.Background())
	assert.False(t, result.Passed)
	assert.Contains(t, result.Findings[0].Detail, "timeout")
}

// ---------------------------------------------------------------------------
// Check #9 — Idempotency uniqueness audit
// ---------------------------------------------------------------------------

func TestCheck9IdempotencyAudit_Clean(t *testing.T) {
	svc := buildFullSvc(t, nil, cleanQuerier(), FullReconciliationConfig{})
	result := svc.runCheck9IdempotencyAudit(context.Background())
	assert.True(t, result.Passed)
}

func TestCheck9IdempotencyAudit_Violation(t *testing.T) {
	q := cleanQuerier()
	q.dupeKeys = []DuplicateIdempotencyKey{
		{IdempotencyKey: "dup-key-1", Occurrences: 2, FirstID: 1, LastID: 2},
	}

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheck9IdempotencyAudit(context.Background())
	assert.False(t, result.Passed)
	require.Len(t, result.Findings, 1)
	assert.Contains(t, result.Findings[0].Description, "dup-key-1")
	assert.Contains(t, result.Findings[0].Description, "2 times")
}

func TestCheck9IdempotencyAudit_QueryError(t *testing.T) {
	q := cleanQuerier()
	q.errDupeKeys = errors.New("index scan failed")

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheck9IdempotencyAudit(context.Background())
	assert.False(t, result.Passed)
	assert.Contains(t, result.Findings[0].Detail, "index scan failed")
}

// ---------------------------------------------------------------------------
// Check #10 — Stale rollup queue
// ---------------------------------------------------------------------------

func TestCheck10StaleRollup_Clean(t *testing.T) {
	svc := buildFullSvc(t, nil, cleanQuerier(), FullReconciliationConfig{})
	result := svc.runCheck10StaleRollup(context.Background())
	assert.True(t, result.Passed)
}

func TestCheck10StaleRollup_Violation(t *testing.T) {
	q := cleanQuerier()
	q.staleItems = []StaleRollupItem{
		{ID: 55, AccountHolder: 10, CurrencyID: 1, ClassificationID: 3, ClaimedUntil: "2024-01-01T00:00:00Z", FailedAttempts: 3},
	}

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheck10StaleRollup(context.Background())
	assert.False(t, result.Passed)
	require.Len(t, result.Findings, 1)
	assert.Contains(t, result.Findings[0].Description, "stale lease")
	assert.Contains(t, result.Findings[0].Description, "failed=3")
	assert.NotContains(t, result.Findings[0].Description, "item 55", "internal queue id must not leak (I-18)")
}

func TestCheck10StaleRollup_QueryError(t *testing.T) {
	q := cleanQuerier()
	q.errStaleItems = errors.New("pg error")

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheck10StaleRollup(context.Background())
	assert.False(t, result.Passed)
	assert.Contains(t, result.Findings[0].Detail, "pg error")
}

// ---------------------------------------------------------------------------
// FullReconciliationConfig defaults
// ---------------------------------------------------------------------------

func TestFullReconciliationConfig_Defaults(t *testing.T) {
	cfg := FullReconciliationConfig{}
	out := cfg.withDefaults()
	assert.Equal(t, "settlement", out.SettlementClassCode)
	assert.Equal(t, 30*60, int(out.SettlementWindow.Seconds()))
	assert.Equal(t, 200, out.NegativeBalancePageLimit)
	assert.False(t, out.EquationTolerance.IsZero())
	assert.Equal(t, 5000, out.Check2ScanLimit)
	assert.Equal(t, 2*time.Minute, out.Check2Timeout)
	assert.Equal(t, 200, out.SnapshotIntegrityPageLimit)
}

// ---------------------------------------------------------------------------
// Tolerance boundary: equation check should not trip within tolerance
// ---------------------------------------------------------------------------

func TestCheck4AccountingEquation_WithinTolerance(t *testing.T) {
	q := cleanQuerier()
	// Difference of 1e-13, which is below the default 1e-12 tolerance.
	q.equationRows = []AccountingEquationRow{
		{CurrencyID: 1, ClassificationID: 1, NormalSide: "debit",
			TotalDebit: decimal.NewFromFloat(1000), TotalCredit: decimal.Zero},
		{CurrencyID: 1, ClassificationID: 2, NormalSide: "credit",
			TotalDebit: decimal.Zero, TotalCredit: decimal.NewFromFloat(999.9999999999999)},
	}

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheck4AccountingEquation(context.Background())
	assert.True(t, result.Passed, "diff within tolerance should not flag a violation")
}

func TestCheck4AccountingEquation_ExceedsTolerance(t *testing.T) {
	q := cleanQuerier()
	// Difference of 1 (well above tolerance).
	q.equationRows = []AccountingEquationRow{
		{CurrencyID: 1, ClassificationID: 1, NormalSide: "debit",
			TotalDebit: decimal.NewFromInt(1000), TotalCredit: decimal.Zero},
		{CurrencyID: 1, ClassificationID: 2, NormalSide: "credit",
			TotalDebit: decimal.Zero, TotalCredit: decimal.NewFromInt(999)},
	}

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheck4AccountingEquation(context.Background())
	assert.False(t, result.Passed)
}

// ---------------------------------------------------------------------------
// Check #2 — Checkpoint-vs-entries fleet scan
// ---------------------------------------------------------------------------

// buildFullSvcForCheck2 wires a FullReconciliationService whose `basic`
// ReconciliationService is fully wired (unlike buildFullSvc, which passes nil
// account-level dependencies) so runCheck2GlobalBalance can actually call
// ReconcileAccount per (holder, currency) pair.
func buildFullSvcForCheck2(t *testing.T, accountEntries AccountEntrySummer, cpReader CheckpointReader, cls ClassificationLister, querier ReconcileQuerier, cfg FullReconciliationConfig) *FullReconciliationService {
	t.Helper()
	engine := core.NewEngine()
	basic := NewReconciliationService(nil, accountEntries, cpReader, cls, engine)
	return NewFullReconciliationService(basic, querier, cfg, engine)
}

func TestCheck2GlobalBalance_NoCheckpoints(t *testing.T) {
	q := cleanQuerier()
	svc := buildFullSvcForCheck2(t, nil, nil, nil, q, FullReconciliationConfig{})

	result := svc.runCheck2GlobalBalance(context.Background())
	assert.True(t, result.Passed)
	assert.True(t, result.Complete, "an unrestricted scan over zero pairs is complete")
	require.Len(t, result.Findings, 1)
	assert.Contains(t, result.Findings[0].Description, "checkpoint scan complete: 0 account/currency pairs")
}

func TestCheck2GlobalBalance_QueryError(t *testing.T) {
	q := cleanQuerier()
	q.errCheckpointPage = errors.New("db unavailable")
	svc := buildFullSvcForCheck2(t, nil, nil, nil, q, FullReconciliationConfig{})

	result := svc.runCheck2GlobalBalance(context.Background())
	assert.False(t, result.Passed)
	require.Len(t, result.Findings, 1)
	assert.Contains(t, result.Findings[0].Detail, "db unavailable")
}

func TestCheck2GlobalBalance_DetectsDrift(t *testing.T) {
	cls := &mockClassificationLister{
		classifications: []ClassificationDim{
			{ID: 10, UID: "cls-10", Code: "asset", NormalSide: core.NormalSideDebit},
		},
	}
	cpReader := &mockCheckpointReader{
		checkpoints: []BalanceCheckpoint{
			{AccountHolder: 100, CurrencyID: 1, ClassificationID: 10, Balance: decimal.NewFromInt(500)},
		},
	}
	accountEntries := &mockAccountEntrySummer{
		debitByClass:  map[int64]decimal.Decimal{10: decimal.NewFromInt(600)},
		creditByClass: map[int64]decimal.Decimal{10: decimal.NewFromInt(200)},
	}
	// entries say 600-200=400, checkpoint says 500 -> drift of 100

	q := cleanQuerier()
	q.checkpointAccounts = []CheckpointAccountKey{{AccountHolder: 100, CurrencyID: 1}}

	svc := buildFullSvcForCheck2(t, accountEntries, cpReader, cls, q, FullReconciliationConfig{})
	result := svc.runCheck2GlobalBalance(context.Background())

	assert.False(t, result.Passed)
	var driftFound bool
	for _, f := range result.Findings {
		if strings.Contains(f.Description, "checkpoint balance drift") {
			driftFound = true
			assert.Contains(t, f.Description, "holder 100")
			assert.Contains(t, f.Detail, "100") // drift amount
		}
	}
	assert.True(t, driftFound, "expected a drift finding, got: %+v", result.Findings)
}

// TestCheck2GlobalBalance_ScansNegativeSystemHolders pins the fix for a keyset
// cursor that started at (0, 0). System holders are the negation of user
// holders (core.SystemHolder), so `account_holder > after_holder` with a zero
// start excluded every negative holder on the very first page — permanently,
// on every run. The entire custodial/system side of the ledger was therefore
// never verified against journal_entries, which is precisely where a forged
// custodial balance would hide.
func TestCheck2GlobalBalance_ScansNegativeSystemHolders(t *testing.T) {
	cls := &mockClassificationLister{
		classifications: []ClassificationDim{
			{ID: 10, UID: "cls-10", Code: "asset", NormalSide: core.NormalSideDebit},
		},
	}
	cpReader := &mockCheckpointReader{
		checkpoints: []BalanceCheckpoint{
			{AccountHolder: 1, CurrencyID: 1, ClassificationID: 10, Balance: decimal.NewFromInt(100)},
		},
	}
	accountEntries := &mockAccountEntrySummer{
		debitByClass:  map[int64]decimal.Decimal{10: decimal.NewFromInt(100)},
		creditByClass: map[int64]decimal.Decimal{},
	}
	// Every pair reconciles clean; this test is about which pairs get visited.

	q := cleanQuerier()
	// Pre-sorted ascending, spanning both sides of zero.
	q.checkpointAccounts = []CheckpointAccountKey{
		{AccountHolder: -100, CurrencyID: 1}, // system counterpart of holder 100
		{AccountHolder: -1, CurrencyID: 1},   // system counterpart of holder 1
		{AccountHolder: 1, CurrencyID: 1},
		{AccountHolder: 100, CurrencyID: 1},
	}

	svc := buildFullSvcForCheck2(t, accountEntries, cpReader, cls, q, FullReconciliationConfig{})
	result := svc.runCheck2GlobalBalance(context.Background())

	assert.True(t, result.Passed)
	assert.True(t, result.Complete)
	require.Len(t, result.Findings, 1)
	// All four pairs — not just the two positive ones.
	assert.Contains(t, result.Findings[0].Description,
		"checkpoint scan complete: 4 account/currency pairs",
		"negative (system) holders must be scanned, not skipped by the cursor")
}

func TestCheck2GlobalBalance_PaginatesAcrossMultiplePages(t *testing.T) {
	cls := &mockClassificationLister{
		classifications: []ClassificationDim{
			{ID: 10, UID: "cls-10", Code: "asset", NormalSide: core.NormalSideDebit},
		},
	}
	cpReader := &mockCheckpointReader{
		checkpoints: []BalanceCheckpoint{
			{AccountHolder: 1, CurrencyID: 1, ClassificationID: 10, Balance: decimal.NewFromInt(100)},
		},
	}
	accountEntries := &mockAccountEntrySummer{
		debitByClass:  map[int64]decimal.Decimal{10: decimal.NewFromInt(100)},
		creditByClass: map[int64]decimal.Decimal{},
	}
	// Every pair reconciles clean (same fixed mock result regardless of which
	// pair is queried) — this test is about pagination mechanics, not drift.

	q := cleanQuerier()
	const total = checkpointScanPageSize + 50 // forces at least 2 page fetches
	pairs := make([]CheckpointAccountKey, 0, total)
	for i := int64(1); i <= total; i++ {
		pairs = append(pairs, CheckpointAccountKey{AccountHolder: i, CurrencyID: 1})
	}
	q.checkpointAccounts = pairs

	svc := buildFullSvcForCheck2(t, accountEntries, cpReader, cls, q, FullReconciliationConfig{})
	result := svc.runCheck2GlobalBalance(context.Background())

	assert.True(t, result.Passed)
	require.GreaterOrEqual(t, q.checkpointPageCalls, 2, "must paginate across multiple page fetches")
	require.Len(t, result.Findings, 1)
	assert.Contains(t, result.Findings[0].Description, fmt.Sprintf("checkpoint scan complete: %d account/currency pairs", total))
}

func TestCheck2GlobalBalance_ScanLimitReportsPartialCoverage(t *testing.T) {
	cls := &mockClassificationLister{
		classifications: []ClassificationDim{
			{ID: 10, UID: "cls-10", Code: "asset", NormalSide: core.NormalSideDebit},
		},
	}
	cpReader := &mockCheckpointReader{
		checkpoints: []BalanceCheckpoint{
			{AccountHolder: 1, CurrencyID: 1, ClassificationID: 10, Balance: decimal.NewFromInt(100)},
		},
	}
	accountEntries := &mockAccountEntrySummer{
		debitByClass:  map[int64]decimal.Decimal{10: decimal.NewFromInt(100)},
		creditByClass: map[int64]decimal.Decimal{},
	}

	q := cleanQuerier()
	q.checkpointAccounts = []CheckpointAccountKey{
		{AccountHolder: 1, CurrencyID: 1},
		{AccountHolder: 2, CurrencyID: 1},
		{AccountHolder: 3, CurrencyID: 1},
		{AccountHolder: 4, CurrencyID: 1},
		{AccountHolder: 5, CurrencyID: 1},
	}

	svc := buildFullSvcForCheck2(t, accountEntries, cpReader, cls, q, FullReconciliationConfig{
		Check2ScanLimit: 2,
	})
	result := svc.runCheck2GlobalBalance(context.Background())

	// No drift was found in the scanned subset, so Passed stays true — Passed
	// only ever reports on what was examined. Coverage is a separate axis:
	// Complete must be false so the run cannot be read as a clean bill of
	// health for the pairs it never reached (working-agreements §3: a check
	// that did not run is never a pass).
	assert.True(t, result.Passed)
	assert.False(t, result.Complete, "a capped scan must not claim full coverage")
	var partialFound bool
	for _, f := range result.Findings {
		if strings.Contains(f.Description, "checkpoint scan incomplete") {
			partialFound = true
			assert.Contains(t, f.Detail, "scanned 2 account/currency pairs")
		}
	}
	assert.True(t, partialFound, "capped scan must report itself as incomplete; got: %+v", result.Findings)
}

// ---------------------------------------------------------------------------
// Check #2 — persisted resume cursor + lap_dirty (C4b)
// ---------------------------------------------------------------------------

// TestCheck2GlobalBalance_ResumesFromPersistedCursor pins C4b: the scan must
// start from whatever GetScanCursor returns, not always from the true
// beginning. Before this fix, Check2ScanLimit capped every run at the same
// prefix forever on a fleet larger than the limit — the tail was never
// scanned (docs/bugs/2026-08-21-reconcile-coverage-blind-spots.md, "未解决").
func TestCheck2GlobalBalance_ResumesFromPersistedCursor(t *testing.T) {
	cls := &mockClassificationLister{
		classifications: []ClassificationDim{
			{ID: 10, UID: "cls-10", Code: "asset", NormalSide: core.NormalSideDebit},
		},
	}
	cpReader := &mockCheckpointReader{
		checkpoints: []BalanceCheckpoint{
			{AccountHolder: 3, CurrencyID: 1, ClassificationID: 10, Balance: decimal.NewFromInt(100)},
		},
	}
	accountEntries := &mockAccountEntrySummer{
		debitByClass:  map[int64]decimal.Decimal{10: decimal.NewFromInt(100)},
		creditByClass: map[int64]decimal.Decimal{},
	}

	q := cleanQuerier()
	q.checkpointAccounts = []CheckpointAccountKey{
		{AccountHolder: 1, CurrencyID: 1},
		{AccountHolder: 2, CurrencyID: 1},
		{AccountHolder: 3, CurrencyID: 1},
	}
	// Pretend a previous (partial) run already advanced past holder 2 --
	// genuinely, having scanned holders 1 and 2 (M-1 fix: lapScanned must
	// reflect that real prior progress for this run's completion to be
	// trusted; see TestCheck2GlobalBalance_ResumedLapUndercountedIsIncomplete
	// for the same shape WITHOUT this genuine prior progress).
	q.cursorSet = true
	q.cursorAfterHolder = 2
	q.cursorAfterCurrency = 1
	q.cursorLapScanned = 2

	svc := buildFullSvcForCheck2(t, accountEntries, cpReader, cls, q, FullReconciliationConfig{})
	result := svc.runCheck2GlobalBalance(context.Background())

	assert.True(t, result.Passed)
	assert.True(t, result.Complete)
	require.Len(t, result.Findings, 1)
	// Only holder 3 lies past the persisted cursor -- holders 1 and 2 must
	// NOT be re-scanned.
	assert.Contains(t, result.Findings[0].Description, "checkpoint scan complete: 1 account/currency pairs verified this run")
}

// TestCheck2GlobalBalance_LapDirtyPersistsAcrossRuns pins the cross-run drift
// signal: a violation found in an earlier (partial) run of the same lap must
// still surface as Passed=false on the LATER run that happens to complete
// the lap, even though that later run's own slice is clean. Without folding
// in lapDirtyAtStart, the completing run would report Passed=true purely
// because its own slice had nothing new -- silently burying the earlier
// finding (the same "looks green when it isn't" shape P0 fixed for the
// single-run case).
func TestCheck2GlobalBalance_LapDirtyPersistsAcrossRuns(t *testing.T) {
	cls := &mockClassificationLister{
		classifications: []ClassificationDim{
			{ID: 10, UID: "cls-10", Code: "asset", NormalSide: core.NormalSideDebit},
		},
	}
	cpReader := &mockCheckpointReader{
		checkpoints: []BalanceCheckpoint{
			{AccountHolder: 6, CurrencyID: 1, ClassificationID: 10, Balance: decimal.NewFromInt(100)},
		},
	}
	accountEntries := &mockAccountEntrySummer{
		debitByClass:  map[int64]decimal.Decimal{10: decimal.NewFromInt(100)},
		creditByClass: map[int64]decimal.Decimal{},
	}

	q := cleanQuerier()
	// Simulates: an earlier run in this lap already found a violation and
	// persisted lap_dirty=true, then stopped (capped) at holder 5.
	q.cursorSet = true
	q.cursorAfterHolder = 5
	q.cursorAfterCurrency = 1
	q.cursorLapDirty = true
	// The remaining pairs in this run's slice are clean.
	q.checkpointAccounts = []CheckpointAccountKey{
		{AccountHolder: 6, CurrencyID: 1},
	}

	svc := buildFullSvcForCheck2(t, accountEntries, cpReader, cls, q, FullReconciliationConfig{})
	result := svc.runCheck2GlobalBalance(context.Background())

	assert.False(t, result.Passed, "an earlier segment's violation must not be buried by a clean final segment")
	assert.True(t, result.Complete, "the lap did complete -- coverage is a separate axis from cleanliness")
	var carriedFound bool
	for _, f := range result.Findings {
		if strings.Contains(f.Description, "earlier segment of this lap already found a violation") {
			carriedFound = true
		}
	}
	assert.True(t, carriedFound, "got: %+v", result.Findings)

	// The lap completed, so both the cursor and lap_dirty must reset for the
	// next lap -- otherwise every future run would report Passed=false
	// forever off a single stale finding.
	assert.Equal(t, 1, q.setScanCursorCalls)
	assert.False(t, q.cursorLapDirty, "lap_dirty must reset once the lap completes")
}

// TestCheck2GlobalBalance_PartialRunPersistsLapDirty pins the other half of
// the same mechanism: a run that itself finds a violation but does NOT
// complete the lap (capped) must persist lap_dirty=true for the next run to
// pick up, not just report Passed=false locally and forget.
func TestCheck2GlobalBalance_PartialRunPersistsLapDirty(t *testing.T) {
	cls := &mockClassificationLister{
		classifications: []ClassificationDim{
			{ID: 10, UID: "cls-10", Code: "asset", NormalSide: core.NormalSideDebit},
		},
	}
	cpReader := &mockCheckpointReader{
		checkpoints: []BalanceCheckpoint{
			{AccountHolder: 1, CurrencyID: 1, ClassificationID: 10, Balance: decimal.NewFromInt(500)},
		},
	}
	accountEntries := &mockAccountEntrySummer{
		debitByClass:  map[int64]decimal.Decimal{10: decimal.NewFromInt(100)},
		creditByClass: map[int64]decimal.Decimal{},
	}
	// entries say 100, checkpoint says 500 -> drift.

	q := cleanQuerier()
	q.checkpointAccounts = []CheckpointAccountKey{
		{AccountHolder: 1, CurrencyID: 1},
		{AccountHolder: 2, CurrencyID: 1},
	}

	svc := buildFullSvcForCheck2(t, accountEntries, cpReader, cls, q, FullReconciliationConfig{
		Check2ScanLimit: 1, // capped before reaching holder 2 -- lap incomplete
	})
	result := svc.runCheck2GlobalBalance(context.Background())

	assert.False(t, result.Passed)
	assert.False(t, result.Complete)
	require.Equal(t, 1, q.setScanCursorCalls)
	assert.True(t, q.cursorSet)
	assert.True(t, q.cursorLapDirty, "the violation found this run must survive into the persisted cursor")
	assert.Equal(t, int64(1), q.cursorAfterHolder)
}

// TestCheck2GlobalBalance_ResumedCursorZeroPairsIsIncomplete pins
// threat-model.md's §4-3 Major: "扫了 0 个不得报 Complete=true, Passed=true".
// reconcile_scan_cursors has no DB-level mutation guard against ledger_app
// (that half of the fix is a separate migration, out of this Go-only
// package's reach), so a run that resumes from a non-fresh cursor and finds
// zero pairs on its very first page is EXACTLY the shape
// `UPDATE reconcile_scan_cursors SET after_holder = <huge>, lap_dirty =
// false` produces -- and, without an independent count to cross-check
// against, is indistinguishable from a lap that genuinely finished at that
// exact point. Before this fix, this exact mock setup produced
// Complete=true (folded into the same green signal as a real, full,
// zero-violation scan); this test would have failed against that code.
func TestCheck2GlobalBalance_ResumedCursorZeroPairsIsIncomplete(t *testing.T) {
	q := cleanQuerier()
	// Simulates a cursor sitting mid-lap (not the fresh sentinel) with
	// nothing beyond it -- whether because a prior run legitimately
	// advanced it there, or because something else (accident or attacker)
	// wrote it there directly. checkpointAccounts is empty: the page query
	// starting from this position returns nothing either way.
	q.cursorSet = true
	q.cursorAfterHolder = 9223372036854775807 // math.MaxInt64
	q.cursorAfterCurrency = 9223372036854775807
	q.cursorLapDirty = false

	svc := buildFullSvcForCheck2(t, nil, nil, nil, q, FullReconciliationConfig{})
	result := svc.runCheck2GlobalBalance(context.Background())

	assert.True(t, result.Passed, "nothing was found wrong -- Passed reports only on what was examined")
	assert.False(t, result.Complete,
		"a resumed cursor finding zero pairs must not claim the same full-coverage signal a genuinely fresh, empty-fleet scan gets")

	var flagged bool
	for _, f := range result.Findings {
		if strings.Contains(f.Description, "resumed from a non-fresh cursor and found zero pairs") {
			flagged = true
		}
	}
	assert.True(t, flagged, "must name the ambiguity explicitly; got: %+v", result.Findings)

	// The cursor still resets to the fresh sentinel: a legitimately-finished
	// lap self-corrects on the very next run (which will then start fresh
	// and, if the fleet really is empty, correctly report Complete=true) --
	// see TestCheck2GlobalBalance_NoCheckpoints for that case.
	require.Equal(t, 1, q.setScanCursorCalls)
	assert.Equal(t, int64(math.MinInt64), q.cursorAfterHolder)
	assert.Equal(t, int64(math.MinInt64), q.cursorAfterCurrency)
	assert.False(t, q.cursorLapDirty)
}

// TestCheck2GlobalBalance_FreshCursorZeroPairsStillComplete is the
// non-regression companion: a FRESH lap (the sentinel start, e.g. a brand
// new install with zero checkpoints anywhere) finding zero pairs is the
// legitimate case this fix must not break -- see
// TestCheck2GlobalBalance_NoCheckpoints for the equivalent scenario via the
// default (unset) cursor; this variant pins it explicitly via an EXPLICITLY
// persisted fresh-sentinel cursor, so the "was this resumed" check is
// exercised on its true/false boundary rather than only on cleanQuerier's
// unset-cursor default.
func TestCheck2GlobalBalance_FreshCursorZeroPairsStillComplete(t *testing.T) {
	q := cleanQuerier()
	q.cursorSet = true
	q.cursorAfterHolder = math.MinInt64
	q.cursorAfterCurrency = math.MinInt64
	q.cursorLapDirty = false

	svc := buildFullSvcForCheck2(t, nil, nil, nil, q, FullReconciliationConfig{})
	result := svc.runCheck2GlobalBalance(context.Background())

	assert.True(t, result.Passed)
	assert.True(t, result.Complete, "a fresh lap that genuinely finds nothing is a legitimate clean bill of health")
}

// TestCheck2GlobalBalance_ResumedLapUndercountedIsIncomplete pins the M-1 fix
// (`.local/independent-review-2026-08-26.md`, docs/plans/2026-08-26-audit-remediation-contracts.md
// follow-on fix-backend-1 batch, board #43): the pre-fix code only distrusted
// a resumed lap that found LITERALLY ZERO pairs on its first page
// (TestCheck2GlobalBalance_ResumedCursorZeroPairsIsIncomplete, unchanged
// above). A cursor tampered to leave exactly one real pair unscanned sailed
// straight through that check -- the run below scans its one remaining pair
// (holder 5), finds nothing wrong locally, reaches the natural end of the
// data (fewer pairs than the page size), and resumedLap is true throughout.
// Before this fix this reported Complete=true, resetting the cursor and
// discarding the four pairs (holders 1-4) the tampering skipped, in one
// forged `UPDATE reconcile_scan_cursors SET after_holder = 4` -- repeatable
// indefinitely. The fix's independent signal (lapScanned, cumulative across
// this lap, vs. CountCheckpointAccountPairs) catches it: lapScanned is 1
// here (nothing about this cursor position was ever legitimately produced by
// a prior run of THIS scan loop), but 5 pairs actually exist.
func TestCheck2GlobalBalance_ResumedLapUndercountedIsIncomplete(t *testing.T) {
	cls := &mockClassificationLister{
		classifications: []ClassificationDim{
			{ID: 10, UID: "cls-10", Code: "asset", NormalSide: core.NormalSideDebit},
		},
	}
	cpReader := &mockCheckpointReader{
		checkpoints: []BalanceCheckpoint{
			{AccountHolder: 5, CurrencyID: 1, ClassificationID: 10, Balance: decimal.NewFromInt(100)},
		},
	}
	accountEntries := &mockAccountEntrySummer{
		debitByClass:  map[int64]decimal.Decimal{10: decimal.NewFromInt(100)},
		creditByClass: map[int64]decimal.Decimal{},
	}

	q := cleanQuerier()
	q.checkpointAccounts = []CheckpointAccountKey{
		{AccountHolder: 1, CurrencyID: 1},
		{AccountHolder: 2, CurrencyID: 1},
		{AccountHolder: 3, CurrencyID: 1},
		{AccountHolder: 4, CurrencyID: 1},
		{AccountHolder: 5, CurrencyID: 1},
	}
	// Simulates the exact attack: a single UPDATE moves the cursor to sit
	// one pair before the true end, WITHOUT any real prior run of this scan
	// loop having actually walked holders 1-4 (cursorLapScanned stays at its
	// zero default -- nothing legitimately advanced it).
	q.cursorSet = true
	q.cursorAfterHolder = 4
	q.cursorAfterCurrency = 1
	q.cursorLapDirty = false

	svc := buildFullSvcForCheck2(t, accountEntries, cpReader, cls, q, FullReconciliationConfig{})
	result := svc.runCheck2GlobalBalance(context.Background())

	assert.True(t, result.Passed, "nothing was found wrong in the one pair this run actually examined")
	assert.False(t, result.Complete,
		"a resumed lap whose cumulative lapScanned (1) falls short of the ledger's actual pair count (5) must not claim full coverage, even though the page query itself found no MORE rows")

	var flagged bool
	for _, f := range result.Findings {
		if strings.Contains(f.Description, "resumed lap ended without reaching the ledger's full pair count") {
			flagged = true
			assert.Contains(t, f.Detail, "verified 1 account/currency pairs")
			assert.Contains(t, f.Detail, "5 pairs currently exist")
		}
	}
	assert.True(t, flagged, "must name the undercount explicitly; got: %+v", result.Findings)

	// The lap restarts from scratch (same recovery as the zero-pairs case),
	// discarding the untrustworthy resumed position rather than treating it
	// as a valid continuation point.
	require.Equal(t, 1, q.setScanCursorCalls)
	assert.Equal(t, int64(math.MinInt64), q.cursorAfterHolder)
	assert.Equal(t, int64(math.MinInt64), q.cursorAfterCurrency)
	assert.False(t, q.cursorLapDirty)
	assert.Equal(t, int64(0), q.cursorLapScanned, "lapScanned must also reset with the cursor, not carry an unverifiable count into the next lap")
}

// TestCheck2GlobalBalance_ResumedLapReachesFullCoverageAcrossCappedRuns is
// the (b)-direction companion the M-1 fix must not break: a LEGITIMATE
// multi-run lap (capped by Check2ScanLimit, exactly the resumability C4b
// exists for) must still be able to report Complete=true on the run that
// finishes it, because its cumulative lapScanned genuinely reaches the
// ledger's pair count across the two real runs below -- unlike the previous
// test, where lapScanned never advanced through a real run of this loop at
// all.
func TestCheck2GlobalBalance_ResumedLapReachesFullCoverageAcrossCappedRuns(t *testing.T) {
	cls := &mockClassificationLister{
		classifications: []ClassificationDim{
			{ID: 10, UID: "cls-10", Code: "asset", NormalSide: core.NormalSideDebit},
		},
	}
	cpReader := &mockCheckpointReader{
		checkpoints: []BalanceCheckpoint{
			{AccountHolder: 1, CurrencyID: 1, ClassificationID: 10, Balance: decimal.NewFromInt(100)},
		},
	}
	accountEntries := &mockAccountEntrySummer{
		debitByClass:  map[int64]decimal.Decimal{10: decimal.NewFromInt(100)},
		creditByClass: map[int64]decimal.Decimal{},
	}

	q := cleanQuerier()
	q.checkpointAccounts = []CheckpointAccountKey{
		{AccountHolder: 1, CurrencyID: 1},
		{AccountHolder: 2, CurrencyID: 1},
		{AccountHolder: 3, CurrencyID: 1},
		{AccountHolder: 4, CurrencyID: 1},
		{AccountHolder: 5, CurrencyID: 1},
	}

	// Run 1: capped at 3 pairs (holders 1-3) -- genuinely scanned by this
	// loop, not forged.
	svc1 := buildFullSvcForCheck2(t, accountEntries, cpReader, cls, q, FullReconciliationConfig{Check2ScanLimit: 3})
	result1 := svc1.runCheck2GlobalBalance(context.Background())
	assert.True(t, result1.Passed)
	assert.False(t, result1.Complete, "run 1 is capped; two pairs remain")
	assert.Equal(t, int64(3), q.cursorLapScanned, "run 1's own genuine progress must be persisted cumulatively")
	assert.Equal(t, int64(3), q.cursorAfterHolder)

	// Run 2: resumes at holder 3, scans holders 4 and 5 -- reaches the
	// natural end of the data (2 pairs, fewer than the page size), with
	// resumedLap still true throughout.
	svc2 := buildFullSvcForCheck2(t, accountEntries, cpReader, cls, q, FullReconciliationConfig{Check2ScanLimit: 3})
	result2 := svc2.runCheck2GlobalBalance(context.Background())
	assert.True(t, result2.Passed)
	assert.True(t, result2.Complete,
		"run 2's cumulative lapScanned (3 from run 1 + 2 this run = 5) reaches the ledger's actual pair count (5), so this resumed completion is trustworthy")
	require.Len(t, result2.Findings, 1)
	assert.Contains(t, result2.Findings[0].Description, "checkpoint scan complete: 2 account/currency pairs verified this run")

	// The completed lap resets both the cursor and the cumulative counter
	// for the next lap.
	assert.Equal(t, int64(math.MinInt64), q.cursorAfterHolder)
	assert.Equal(t, int64(0), q.cursorLapScanned)
}

// TestCheck2GlobalBalance_CountCheckpointAccountPairsErrorIsReported pins the
// error path of the M-1 fix's new query: a resumed lap that reaches the
// natural end of the data must not silently fall back to trusting it when
// the independent pair-count check itself cannot be answered.
func TestCheck2GlobalBalance_CountCheckpointAccountPairsErrorIsReported(t *testing.T) {
	q := cleanQuerier()
	q.checkpointAccounts = []CheckpointAccountKey{{AccountHolder: 5, CurrencyID: 1}}
	q.cursorSet = true
	q.cursorAfterHolder = 4
	q.cursorAfterCurrency = 1
	q.errCountCheckpointAccountPairs = errors.New("db unavailable")

	cls := &mockClassificationLister{
		classifications: []ClassificationDim{
			{ID: 10, UID: "cls-10", Code: "asset", NormalSide: core.NormalSideDebit},
		},
	}
	cpReader := &mockCheckpointReader{
		checkpoints: []BalanceCheckpoint{
			{AccountHolder: 5, CurrencyID: 1, ClassificationID: 10, Balance: decimal.NewFromInt(100)},
		},
	}
	accountEntries := &mockAccountEntrySummer{
		debitByClass:  map[int64]decimal.Decimal{10: decimal.NewFromInt(100)},
		creditByClass: map[int64]decimal.Decimal{},
	}

	svc := buildFullSvcForCheck2(t, accountEntries, cpReader, cls, q, FullReconciliationConfig{})
	result := svc.runCheck2GlobalBalance(context.Background())

	assert.False(t, result.Passed)
	var found bool
	for _, f := range result.Findings {
		if strings.Contains(f.Description, "checkpoint account pair count failed") {
			found = true
			assert.Contains(t, f.Detail, "db unavailable")
		}
	}
	assert.True(t, found, "got: %+v", result.Findings)
}

// ---------------------------------------------------------------------------
// Check #11 — system_rollups vs entries (M4/I-23)
// ---------------------------------------------------------------------------

func TestCheckSystemRollupIntegrity_Clean(t *testing.T) {
	q := cleanQuerier()
	q.systemRollups = []SystemRollupRow{
		{CurrencyID: 1, ClassificationID: 10, TotalBalance: decimal.NewFromInt(1000)},
	}
	q.equationRows = []AccountingEquationRow{
		{CurrencyID: 1, ClassificationID: 10, NormalSide: "debit", TotalDebit: decimal.NewFromInt(1000), TotalCredit: decimal.Zero},
	}

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheckSystemRollupIntegrity(context.Background())
	assert.True(t, result.Passed)
	assert.True(t, result.Complete)
}

// TestCheckSystemRollupIntegrity_DetectsDrift pins M4/I-23: system_rollups
// must be checked directly against journal_entries. RefreshSystemRollups
// derives this table from balance_checkpoints
// (AggregateCheckpointsByClassification), so a system_rollups row that no
// longer matches the entries-based recompute is exactly the class of drift
// this check exists to catch -- including the case where checkpoints were
// tampered and system_rollups inherited the poison wholesale.
func TestCheckSystemRollupIntegrity_DetectsDrift(t *testing.T) {
	q := cleanQuerier()
	q.systemRollups = []SystemRollupRow{
		{CurrencyID: 1, ClassificationID: 10, TotalBalance: decimal.NewFromInt(1999)}, // poisoned +999
	}
	q.equationRows = []AccountingEquationRow{
		{CurrencyID: 1, ClassificationID: 10, NormalSide: "debit", TotalDebit: decimal.NewFromInt(1000), TotalCredit: decimal.Zero},
	}

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheckSystemRollupIntegrity(context.Background())
	assert.False(t, result.Passed)
	require.Len(t, result.Findings, 1)
	assert.Contains(t, result.Findings[0].Detail, "999")
}

// TestCheckSystemRollupIntegrity_FabricatedRowWithNoEntries pins the M5
// fabrication scenario: a system_rollups row with no backing entries at all
// (a rollup entry manufactured out of nothing) must be flagged, not treated
// as "unknown, skip".
func TestCheckSystemRollupIntegrity_FabricatedRowWithNoEntries(t *testing.T) {
	q := cleanQuerier()
	q.systemRollups = []SystemRollupRow{
		{CurrencyID: 1, ClassificationID: 99, TotalBalance: decimal.NewFromInt(5000)},
	}
	// No matching AccountingEquationRow for (currency=1, classification=99):
	// zero entries exist for that pair.

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheckSystemRollupIntegrity(context.Background())
	assert.False(t, result.Passed)
	require.Len(t, result.Findings, 1)
	assert.Contains(t, result.Findings[0].Detail, "5000")
}

func TestCheckSystemRollupIntegrity_QueryError(t *testing.T) {
	q := cleanQuerier()
	q.errSystemRollups = errors.New("db unavailable")

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheckSystemRollupIntegrity(context.Background())
	assert.False(t, result.Passed)
	assert.Contains(t, result.Findings[0].Detail, "db unavailable")
}

// ---------------------------------------------------------------------------
// Check #12 — balance_snapshots vs entries (M4/I-23)
// ---------------------------------------------------------------------------

func TestCheckSnapshotIntegrity_Clean(t *testing.T) {
	svc := buildFullSvc(t, nil, cleanQuerier(), FullReconciliationConfig{})
	result := svc.runCheckSnapshotIntegrity(context.Background())
	assert.True(t, result.Passed)
	assert.True(t, result.Complete)
}

func TestCheckSnapshotIntegrity_DetectsDrift(t *testing.T) {
	q := cleanQuerier()
	q.snapshotDrifts = []SnapshotDriftRow{
		{AccountHolder: 42, CurrencyID: 1, ClassificationID: 10,
			SnapshotDate:  time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC),
			StoredBalance: decimal.NewFromInt(500), RecomputedBalance: decimal.NewFromInt(100)},
	}

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheckSnapshotIntegrity(context.Background())
	assert.False(t, result.Passed)
	require.Len(t, result.Findings, 1)
	assert.Contains(t, result.Findings[0].Description, "holder 42")
	assert.Contains(t, result.Findings[0].Detail, "stored=500")
}

// TestCheckSnapshotIntegrity_PageLimitReportsIncomplete pins the
// fail-closed-by-construction requirement for this check's page cap: hitting
// the limit must mark Complete=false, not silently truncate the finding list
// (the same shape check #2's Complete field already enforces).
func TestCheckSnapshotIntegrity_PageLimitReportsIncomplete(t *testing.T) {
	q := cleanQuerier()
	for i := 0; i < 3; i++ {
		q.snapshotDrifts = append(q.snapshotDrifts, SnapshotDriftRow{
			AccountHolder: int64(i + 1), CurrencyID: 1, ClassificationID: 10,
			SnapshotDate:  time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC),
			StoredBalance: decimal.NewFromInt(1), RecomputedBalance: decimal.Zero,
		})
	}

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{SnapshotIntegrityPageLimit: 3})
	result := svc.runCheckSnapshotIntegrity(context.Background())
	assert.False(t, result.Passed)
	assert.False(t, result.Complete, "hitting the page limit must not claim full coverage")
}

func TestCheckSnapshotIntegrity_QueryError(t *testing.T) {
	q := cleanQuerier()
	q.errSnapshotDrifts = errors.New("timeout")

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheckSnapshotIntegrity(context.Background())
	assert.False(t, result.Passed)
	assert.Contains(t, result.Findings[0].Detail, "timeout")
}

// Mechanical I-18 pin for the reconcile report surface: no format string in
// reconcile.go may weave an internal id into a Finding. The existing
// server-side pin (TestContract_NoInternalIDKeysInJSON) only scans JSON tags
// and is structurally blind to ids embedded in free text — this closes that
// hole at the source.
func TestReconcileFindings_NoInternalIDPatternsInSource(t *testing.T) {
	src, err := os.ReadFile("reconcile.go")
	require.NoError(t, err)

	// Internal-id smells inside Sprintf formats destined for Findings:
	// "currency %d", "classification %d", "journal %d"/"journal IDs",
	// "reservation %d", "item %d", "currency=%d", "class=%d".
	banned := regexp.MustCompile(`(currency %d|classification %d|journal %d|journal IDs|reservation %d|queue item %d|currency=%d|class=%d|classification=%d)`)
	for i, line := range strings.Split(string(src), "\n") {
		if !strings.Contains(line, "Finding{") && !strings.Contains(line, "fmt.Sprintf") {
			continue
		}
		require.False(t, banned.MatchString(line),
			"reconcile.go:%d leaks an internal id pattern into a report string: %s", i+1, strings.TrimSpace(line))
	}
}
