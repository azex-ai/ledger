package service

import (
	"context"
	"errors"
	"fmt"
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
	orphanReservs    []OrphanReservation
	staleItems       []StaleRollupItem
	dupeKeys         []DuplicateIdempotencyKey

	// checkpointAccounts must be pre-sorted ascending by (AccountHolder,
	// CurrencyID) — ListCheckpointAccountsPage paginates over it using the
	// same keyset semantics as the real SQL query.
	checkpointAccounts  []CheckpointAccountKey
	checkpointPageCalls int

	// force errors
	errOrphanCount    error
	errOrphanSample   error
	errEquation       error
	errSettlement     error
	errNegBal         error
	errOrphanReservs  error
	errDupeKeys       error
	errStaleItems     error
	errCheckpointPage error
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
	assert.Len(t, report.Checks, 10, "should run exactly 10 checks")
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
		passed, ok := metrics.results[c.Name]
		require.True(t, ok, "check %q must have emitted a metric", c.Name)
		assert.Equal(t, c.Passed, passed)
	}
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
	// 1 summary finding + 2 sample findings
	assert.Len(t, result.Findings, 3)
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
	assert.Contains(t, result.Findings[0].Description, "currency 1")
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
	assert.Contains(t, result.Findings[0].Description, "currency 2")
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
	assert.Contains(t, result.Findings[0].Description, "currency 1")
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
		{ID: 7, AccountHolder: 99, CurrencyID: 1, Status: "settled", JournalID: 42},
	}

	svc := buildFullSvc(t, nil, q, FullReconciliationConfig{})
	result := svc.runCheck7OrphanReservations(context.Background())
	assert.False(t, result.Passed)
	require.Len(t, result.Findings, 1)
	assert.Contains(t, result.Findings[0].Description, "reservation 7")
	assert.Contains(t, result.Findings[0].Description, "journal 42")
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
// Check #8 — Pending journal timeout (skipped)
// ---------------------------------------------------------------------------

func TestCheck8PendingJournalTimeout_Skipped(t *testing.T) {
	svc := buildFullSvc(t, nil, cleanQuerier(), FullReconciliationConfig{})
	result := svc.runCheck8PendingJournalTimeout()
	// Skipped check reports passed=true with an informational Finding.
	assert.True(t, result.Passed)
	require.Len(t, result.Findings, 1)
	assert.Contains(t, result.Findings[0].Description, "skipped")
	assert.Contains(t, result.Findings[0].Detail, "journals.status")
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
	assert.Contains(t, result.Findings[0].Description, "rollup_queue item 55")
	assert.Contains(t, result.Findings[0].Description, "failed=3")
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
		checkpoints: []core.BalanceCheckpoint{
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

func TestCheck2GlobalBalance_PaginatesAcrossMultiplePages(t *testing.T) {
	cls := &mockClassificationLister{
		classifications: []ClassificationDim{
			{ID: 10, UID: "cls-10", Code: "asset", NormalSide: core.NormalSideDebit},
		},
	}
	cpReader := &mockCheckpointReader{
		checkpoints: []core.BalanceCheckpoint{
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
		checkpoints: []core.BalanceCheckpoint{
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

	// No drift was found in the scanned subset, so Passed stays true — but
	// the scan must explicitly flag itself as incomplete, never silently
	// claim full coverage it didn't actually perform.
	assert.True(t, result.Passed)
	var partialFound bool
	for _, f := range result.Findings {
		if strings.Contains(f.Description, "checkpoint scan incomplete") {
			partialFound = true
			assert.Contains(t, f.Detail, "scanned 2 account/currency pairs")
		}
	}
	assert.True(t, partialFound, "capped scan must report itself as incomplete; got: %+v", result.Findings)
}
