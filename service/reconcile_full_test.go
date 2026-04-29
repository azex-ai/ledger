package service

import (
	"context"
	"errors"
	"testing"

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

	// force errors
	errOrphanCount      error
	errOrphanSample     error
	errEquation         error
	errSettlement       error
	errNegBal           error
	errOrphanReservs    error
	errDupeKeys         error
	errStaleItems       error
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
