package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/channel"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/server"
	"github.com/azex-ai/ledger/service"
)

// --- Mock implementations ---

type mockJournalWriter struct {
	postFn        func(ctx context.Context, input core.JournalInput) (*core.Journal, error)
	templateFn    func(ctx context.Context, code string, params core.TemplateParams) (*core.Journal, error)
	reverseFn     func(ctx context.Context, journalUID string, reason string) (*core.Journal, error)
	reverseFracFn func(ctx context.Context, journalUID string, num, den int64, reason, idempotencyKey string) (*core.Journal, error)
}

func (m *mockJournalWriter) PostJournal(ctx context.Context, input core.JournalInput) (*core.Journal, error) {
	if m.postFn != nil {
		return m.postFn(ctx, input)
	}
	return &core.Journal{UID: "uid-1", JournalTypeUID: input.JournalTypeUID, IdempotencyKey: input.IdempotencyKey, TotalDebit: decimal.NewFromInt(100), TotalCredit: decimal.NewFromInt(100), CreatedAt: time.Now()}, nil
}

func (m *mockJournalWriter) ExecuteTemplate(ctx context.Context, code string, params core.TemplateParams) (*core.Journal, error) {
	if m.templateFn != nil {
		return m.templateFn(ctx, code, params)
	}
	return &core.Journal{UID: "uid-2", JournalTypeUID: "jt-1", IdempotencyKey: params.IdempotencyKey, TotalDebit: decimal.NewFromInt(50), TotalCredit: decimal.NewFromInt(50), CreatedAt: time.Now()}, nil
}

func (m *mockJournalWriter) ReverseJournal(ctx context.Context, journalUID string, reason string) (*core.Journal, error) {
	if m.reverseFn != nil {
		return m.reverseFn(ctx, journalUID, reason)
	}
	return &core.Journal{UID: "uid-3", JournalTypeUID: "jt-1", IdempotencyKey: fmt.Sprintf("reversal:%s:%s", journalUID, reason), ReversalOfUID: journalUID, TotalDebit: decimal.NewFromInt(100), TotalCredit: decimal.NewFromInt(100), CreatedAt: time.Now()}, nil
}

func (m *mockJournalWriter) ReverseJournalFraction(ctx context.Context, journalUID string, num, den int64, reason, idempotencyKey string) (*core.Journal, error) {
	if m.reverseFracFn != nil {
		return m.reverseFracFn(ctx, journalUID, num, den, reason, idempotencyKey)
	}
	return &core.Journal{UID: "uid-4", JournalTypeUID: "jt-1", IdempotencyKey: idempotencyKey, ReversalOfUID: journalUID, TotalDebit: decimal.NewFromInt(100), TotalCredit: decimal.NewFromInt(100), CreatedAt: time.Now()}, nil
}

type mockBalanceReader struct{}

func (m *mockBalanceReader) GetBalance(ctx context.Context, holder int64, currencyUID, classificationUID string) (decimal.Decimal, error) {
	return decimal.NewFromInt(1000), nil
}

func (m *mockBalanceReader) GetBalances(ctx context.Context, holder int64, currencyUID string) ([]core.Balance, error) {
	return []core.Balance{
		{AccountHolder: holder, CurrencyUID: currencyUID, ClassificationUID: "cls-1", Balance: decimal.NewFromInt(500)},
		{AccountHolder: holder, CurrencyUID: currencyUID, ClassificationUID: "cls-2", Balance: decimal.NewFromInt(300)},
	}, nil
}

func (m *mockBalanceReader) GetBalanceBreakdown(ctx context.Context, holder int64, currencyUID string) (*core.BalanceBreakdown, error) {
	available := decimal.NewFromInt(80)
	pending := decimal.NewFromInt(100)
	locked := decimal.NewFromInt(20)
	return &core.BalanceBreakdown{
		AccountHolder: holder,
		CurrencyUID:   currencyUID,
		Available:     available,
		Pending:       pending,
		Locked:        locked,
		Total:         available.Add(locked).Add(pending),
	}, nil
}

func (m *mockBalanceReader) BatchGetBalances(ctx context.Context, holderIDs []int64, currencyUID string) (map[int64][]core.Balance, error) {
	result := make(map[int64][]core.Balance)
	for _, id := range holderIDs {
		result[id] = []core.Balance{
			{AccountHolder: id, CurrencyUID: currencyUID, ClassificationUID: "cls-1", Balance: decimal.NewFromInt(100)},
		}
	}
	return result, nil
}

type mockReserver struct {
	reserveFn            func(ctx context.Context, input core.ReserveInput) (*core.Reservation, error)
	settleFn             func(ctx context.Context, reservationUID string, actualAmount decimal.Decimal) error
	releaseFn            func(ctx context.Context, reservationUID string) error
	heldAmountFn         func(ctx context.Context, holder int64, currencyUID string) (decimal.Decimal, error)
	settlePartialFn      func(ctx context.Context, reservationUID string, amount decimal.Decimal) error
	finalizeSettlementFn func(ctx context.Context, reservationUID string) error
}

func (m *mockReserver) Reserve(ctx context.Context, input core.ReserveInput) (*core.Reservation, error) {
	if m.reserveFn != nil {
		return m.reserveFn(ctx, input)
	}
	return &core.Reservation{UID: "rsv-1", AccountHolder: input.AccountHolder, CurrencyUID: input.CurrencyUID, ReservedAmount: input.Amount, Status: core.ReservationStatusActive, IdempotencyKey: input.IdempotencyKey, ExpiresAt: time.Now().Add(15 * time.Minute), CreatedAt: time.Now(), UpdatedAt: time.Now()}, nil
}

func (m *mockReserver) Settle(ctx context.Context, input core.SettleInput) error {
	if m.settleFn != nil {
		return m.settleFn(ctx, input.ReservationUID, input.Amount)
	}
	return nil
}

func (m *mockReserver) Release(ctx context.Context, reservationUID string) error {
	if m.releaseFn != nil {
		return m.releaseFn(ctx, reservationUID)
	}
	return nil
}

func (m *mockReserver) HeldAmount(ctx context.Context, holder int64, currencyUID string) (decimal.Decimal, error) {
	if m.heldAmountFn != nil {
		return m.heldAmountFn(ctx, holder, currencyUID)
	}
	return decimal.Zero, nil
}

func (m *mockReserver) SettlePartial(ctx context.Context, input core.SettlePartialInput) error {
	if m.settlePartialFn != nil {
		return m.settlePartialFn(ctx, input.ReservationUID, input.Amount)
	}
	return nil
}

func (m *mockReserver) FinalizeSettlement(ctx context.Context, reservationUID string) error {
	if m.finalizeSettlementFn != nil {
		return m.finalizeSettlementFn(ctx, reservationUID)
	}
	return nil
}

type mockBooker struct {
	createFn     func(ctx context.Context, input core.CreateBookingInput) (*core.Booking, error)
	transitionFn func(ctx context.Context, input core.TransitionInput) (*core.Event, error)
}

func (m *mockBooker) CreateBooking(ctx context.Context, input core.CreateBookingInput) (*core.Booking, error) {
	if m.createFn != nil {
		return m.createFn(ctx, input)
	}
	return &core.Booking{
		UID: "bk-1", ClassificationUID: "cls-1", AccountHolder: input.AccountHolder,
		CurrencyUID: input.CurrencyUID, Amount: input.Amount, Status: "pending",
		ChannelName: input.ChannelName, IdempotencyKey: input.IdempotencyKey,
		Metadata: input.Metadata, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}, nil
}

func (m *mockBooker) Transition(ctx context.Context, input core.TransitionInput) (*core.Event, error) {
	if m.transitionFn != nil {
		return m.transitionFn(ctx, input)
	}
	return &core.Event{
		UID: "evt-1", ClassificationCode: "deposit", BookingUID: input.BookingUID,
		AccountHolder: 100, CurrencyUID: "cur-1",
		FromStatus: "pending", ToStatus: input.ToStatus,
		Amount: input.Amount, OccurredAt: time.Now(),
	}, nil
}

type mockBookingReader struct {
	getFn  func(ctx context.Context, uid string) (*core.Booking, error)
	listFn func(ctx context.Context, filter core.BookingFilter) ([]core.Booking, string, error)
}

func (m *mockBookingReader) GetBooking(ctx context.Context, uid string) (*core.Booking, error) {
	if m.getFn != nil {
		return m.getFn(ctx, uid)
	}
	return &core.Booking{
		UID: uid, ClassificationUID: "cls-1", AccountHolder: 100,
		CurrencyUID: "cur-1", Amount: decimal.NewFromInt(500), Status: "pending",
		ChannelName: "crypto", IdempotencyKey: "op-1",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}, nil
}

func (m *mockBookingReader) ListBookings(ctx context.Context, filter core.BookingFilter) ([]core.Booking, string, error) {
	if m.listFn != nil {
		return m.listFn(ctx, filter)
	}
	return []core.Booking{
		{UID: "bk-1", ClassificationUID: "cls-1", AccountHolder: 100, CurrencyUID: "cur-1", Amount: decimal.NewFromInt(500), Status: "pending", CreatedAt: time.Now(), UpdatedAt: time.Now()},
	}, "", nil
}

type mockEventReader struct {
	getFn  func(ctx context.Context, uid string) (*core.Event, error)
	listFn func(ctx context.Context, filter core.EventFilter) ([]core.Event, string, error)
}

func (m *mockEventReader) GetEvent(ctx context.Context, uid string) (*core.Event, error) {
	if m.getFn != nil {
		return m.getFn(ctx, uid)
	}
	return &core.Event{
		UID: uid, ClassificationCode: "deposit", BookingUID: "bk-1",
		AccountHolder: 100, CurrencyUID: "cur-1",
		FromStatus: "pending", ToStatus: "confirmed",
		Amount: decimal.NewFromInt(500), OccurredAt: time.Now(),
	}, nil
}

func (m *mockEventReader) ListEvents(ctx context.Context, filter core.EventFilter) ([]core.Event, string, error) {
	if m.listFn != nil {
		return m.listFn(ctx, filter)
	}
	return []core.Event{
		{UID: "evt-1", ClassificationCode: "deposit", BookingUID: "bk-1", AccountHolder: 100, CurrencyUID: "cur-1", FromStatus: "pending", ToStatus: "confirmed", Amount: decimal.NewFromInt(500), OccurredAt: time.Now()},
	}, "", nil
}

type mockClassificationStore struct{}

func (m *mockClassificationStore) CreateClassification(ctx context.Context, input core.ClassificationInput) (*core.Classification, error) {
	return &core.Classification{
		UID:        "cls-1",
		Code:       input.Code,
		Name:       input.Name,
		NormalSide: input.NormalSide,
		IsSystem:   input.IsSystem,
		IsActive:   true,
		Lifecycle:  input.Lifecycle,
		CreatedAt:  time.Now(),
	}, nil
}

func (m *mockClassificationStore) GetByCode(ctx context.Context, code string) (*core.Classification, error) {
	return &core.Classification{UID: "cls-1", Code: code, Name: code, NormalSide: core.NormalSideDebit, IsActive: true, CreatedAt: time.Now()}, nil
}

func (m *mockClassificationStore) SetBalanceRole(ctx context.Context, uid string, role core.BalanceRole) error {
	return nil
}

func (m *mockClassificationStore) SetDisplayLabelIfEmpty(ctx context.Context, uid string, label string) error {
	return nil
}

func (m *mockClassificationStore) DeactivateClassification(ctx context.Context, uid string) error {
	return nil
}

func (m *mockClassificationStore) ListClassifications(ctx context.Context, activeOnly bool) ([]core.Classification, error) {
	return []core.Classification{
		{UID: "cls-1", Code: "ASSET", Name: "Asset", NormalSide: core.NormalSideDebit, IsActive: true},
		{UID: "cls-2", Code: "LIABILITY", Name: "Liability", NormalSide: core.NormalSideCredit, IsActive: true},
	}, nil
}

type mockJournalTypeStore struct{}

func (m *mockJournalTypeStore) CreateJournalType(ctx context.Context, input core.JournalTypeInput) (*core.JournalType, error) {
	return &core.JournalType{UID: "jt-1", Code: input.Code, Name: input.Name, IsActive: true, CreatedAt: time.Now()}, nil
}

func (m *mockJournalTypeStore) GetJournalTypeByCode(ctx context.Context, code string) (*core.JournalType, error) {
	return &core.JournalType{UID: "jt-1", Code: code, Name: code, IsActive: true, CreatedAt: time.Now()}, nil
}

func (m *mockJournalTypeStore) SetDisplayLabelIfEmpty(ctx context.Context, uid string, label string) error {
	return nil
}

func (m *mockJournalTypeStore) DeactivateJournalType(ctx context.Context, uid string) error {
	return nil
}

func (m *mockJournalTypeStore) ListJournalTypes(ctx context.Context, activeOnly bool) ([]core.JournalType, error) {
	return []core.JournalType{{UID: "jt-1", Code: "DEPOSIT", Name: "Deposit", IsActive: true}}, nil
}

type mockTemplateStore struct{}

func (m *mockTemplateStore) CreateTemplate(ctx context.Context, input core.TemplateInput) (*core.EntryTemplate, error) {
	if err := input.Validate(); err != nil {
		return nil, err
	}
	return &core.EntryTemplate{UID: "tmpl-1", Code: input.Code, Name: input.Name, JournalTypeUID: input.JournalTypeUID, IsActive: true, CreatedAt: time.Now()}, nil
}

func (m *mockTemplateStore) DeactivateTemplate(ctx context.Context, uid string) error { return nil }

func (m *mockTemplateStore) GetTemplate(ctx context.Context, code string) (*core.EntryTemplate, error) {
	return &core.EntryTemplate{
		UID: "tmpl-1", Code: code, Name: "Test", JournalTypeUID: "jt-1", IsActive: true,
		Lines: []core.EntryTemplateLine{
			{ClassificationUID: "cls-1", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount"},
			{ClassificationUID: "cls-1", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount"},
		},
	}, nil
}

func (m *mockTemplateStore) ListTemplates(ctx context.Context, activeOnly bool) ([]core.EntryTemplate, error) {
	return []core.EntryTemplate{{UID: "tmpl-1", Code: "deposit", Name: "Deposit", JournalTypeUID: "jt-1", IsActive: true}}, nil
}

type mockCurrencyStore struct{}

func (m *mockCurrencyStore) CreateCurrency(ctx context.Context, input core.CurrencyInput) (*core.Currency, error) {
	return &core.Currency{UID: "cur-1", Code: input.Code, Name: input.Name}, nil
}

func (m *mockCurrencyStore) DeactivateCurrency(ctx context.Context, uid string) error {
	return nil
}

func (m *mockCurrencyStore) ListCurrencies(ctx context.Context, activeOnly bool) ([]core.Currency, error) {
	return []core.Currency{{UID: "cur-1", Code: "USDT", Name: "Tether", IsActive: true}}, nil
}

func (m *mockCurrencyStore) GetCurrency(ctx context.Context, uid string) (*core.Currency, error) {
	return &core.Currency{UID: uid, Code: "USDT", Name: "Tether"}, nil
}

type mockAccountPolicyStore struct{}

func (m *mockAccountPolicyStore) SetPolicy(ctx context.Context, input core.AccountPolicyInput) (*core.AccountPolicy, error) {
	return &core.AccountPolicy{
		UID:               "pol-1",
		AccountHolder:     input.AccountHolder,
		CurrencyUID:       input.CurrencyUID,
		ClassificationUID: input.ClassificationUID,
		Status:            input.Status,
		MinBalance:        input.MinBalance,
		EnforceMinBalance: input.EnforceMinBalance,
		Note:              input.Note,
	}, nil
}

func (m *mockAccountPolicyStore) GetPolicy(ctx context.Context, holder int64, currencyUID, classificationUID string) (*core.AccountPolicy, error) {
	return &core.AccountPolicy{UID: "pol-1", AccountHolder: holder, CurrencyUID: currencyUID, ClassificationUID: classificationUID, Status: core.AccountPolicyStatusActive}, nil
}

func (m *mockAccountPolicyStore) ListPolicies(ctx context.Context, holder int64) ([]core.AccountPolicy, error) {
	return []core.AccountPolicy{{UID: "pol-1", AccountHolder: holder, Status: core.AccountPolicyStatusActive}}, nil
}

type mockReconciler struct{}

func (m *mockReconciler) CheckAccountingEquation(ctx context.Context) (*core.ReconcileResult, error) {
	return &core.ReconcileResult{Balanced: true, Gap: decimal.Zero, CheckedAt: time.Now()}, nil
}

func (m *mockReconciler) ReconcileAccount(ctx context.Context, holder int64, currencyUID string) (*core.ReconcileResult, error) {
	return &core.ReconcileResult{Balanced: true, Gap: decimal.Zero, CheckedAt: time.Now()}, nil
}

type mockFullReconciler struct {
	report *core.ReconcileReport
	err    error
}

func (m *mockFullReconciler) RunFullReconciliation(ctx context.Context) (*core.ReconcileReport, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.report != nil {
		return m.report, nil
	}
	return &core.ReconcileReport{OverallPassed: true, RunAt: time.Now()}, nil
}

type mockSnapshotter struct{}

func (m *mockSnapshotter) CreateDailySnapshot(ctx context.Context, date time.Time) error { return nil }
func (m *mockSnapshotter) GetSnapshotBalance(ctx context.Context, holder int64, currencyUID string, date time.Time) ([]core.Balance, error) {
	return nil, nil
}

type mockAuditQuerier struct {
	listByAccountFn   func(ctx context.Context, filter core.AuditFilter) ([]core.Journal, string, error)
	listByTimeRangeFn func(ctx context.Context, filter core.AuditFilter) ([]core.Journal, string, error)
	traceBookingFn    func(ctx context.Context, bookingUID string) (*core.BookingTrace, error)
	listReversalsFn   func(ctx context.Context, journalUID string) ([]core.Journal, error)
}

func (m *mockAuditQuerier) ListJournalsByAccount(ctx context.Context, filter core.AuditFilter) ([]core.Journal, string, error) {
	if m.listByAccountFn != nil {
		return m.listByAccountFn(ctx, filter)
	}
	return []core.Journal{
		{UID: "jr-1", JournalTypeUID: "jt-1", IdempotencyKey: "audit-account", TotalDebit: decimal.NewFromInt(100), TotalCredit: decimal.NewFromInt(100), CreatedAt: time.Now()},
	}, "", nil
}

func (m *mockAuditQuerier) ListEntriesByJournal(ctx context.Context, journalUID string) ([]core.Entry, error) {
	return []core.Entry{
		{JournalUID: journalUID, AccountHolder: 100, CurrencyUID: "cur-1", ClassificationUID: "cls-1", EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100), CreatedAt: time.Now()},
	}, nil
}

func (m *mockAuditQuerier) ListJournalsByTimeRange(ctx context.Context, filter core.AuditFilter) ([]core.Journal, string, error) {
	if m.listByTimeRangeFn != nil {
		return m.listByTimeRangeFn(ctx, filter)
	}
	return []core.Journal{
		{UID: "jr-2", JournalTypeUID: "jt-1", IdempotencyKey: "audit-time-range", TotalDebit: decimal.NewFromInt(50), TotalCredit: decimal.NewFromInt(50), CreatedAt: time.Now()},
	}, "", nil
}

func (m *mockAuditQuerier) TraceBooking(ctx context.Context, bookingUID string) (*core.BookingTrace, error) {
	if m.traceBookingFn != nil {
		return m.traceBookingFn(ctx, bookingUID)
	}
	return &core.BookingTrace{
		Booking: core.Booking{
			UID: bookingUID, ClassificationUID: "cls-1", AccountHolder: 100, CurrencyUID: "cur-1",
			Amount: decimal.NewFromInt(500), Status: "confirmed", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		},
		Events: []core.Event{
			{UID: "evt-1", ClassificationCode: "deposit", BookingUID: bookingUID, AccountHolder: 100, CurrencyUID: "cur-1", FromStatus: "pending", ToStatus: "confirmed", Amount: decimal.NewFromInt(500), OccurredAt: time.Now()},
		},
		Journals: []core.Journal{
			{UID: "jr-10", JournalTypeUID: "jt-1", IdempotencyKey: "trace", TotalDebit: decimal.NewFromInt(500), TotalCredit: decimal.NewFromInt(500), CreatedAt: time.Now()},
		},
	}, nil
}

func (m *mockAuditQuerier) ListReversals(ctx context.Context, journalUID string) ([]core.Journal, error) {
	if m.listReversalsFn != nil {
		return m.listReversalsFn(ctx, journalUID)
	}
	return []core.Journal{
		{UID: journalUID, JournalTypeUID: "jt-1", IdempotencyKey: "root", TotalDebit: decimal.NewFromInt(100), TotalCredit: decimal.NewFromInt(100), CreatedAt: time.Now()},
	}, nil
}

type mockPlatformBalanceReader struct{}

func (m *mockPlatformBalanceReader) GetPlatformBalances(ctx context.Context, currencyUID string) (*core.PlatformBalance, error) {
	return &core.PlatformBalance{
		CurrencyUID: currencyUID,
		UserSide:    map[string]decimal.Decimal{"main_wallet": decimal.NewFromInt(1000)},
		SystemSide:  map[string]decimal.Decimal{"custodial": decimal.NewFromInt(1000)},
	}, nil
}

func (m *mockPlatformBalanceReader) GetTotalLiabilityByAsset(ctx context.Context, currencyUID string) (decimal.Decimal, error) {
	return decimal.NewFromInt(1000), nil
}

type mockSolvencyChecker struct {
	checkFn func(ctx context.Context, currencyUID string) (*core.SolvencyReport, error)
}

func (m *mockSolvencyChecker) SolvencyCheck(ctx context.Context, currencyUID string) (*core.SolvencyReport, error) {
	if m.checkFn != nil {
		return m.checkFn(ctx, currencyUID)
	}
	return &core.SolvencyReport{
		CurrencyUID: currencyUID,
		Liability:   decimal.NewFromInt(1000),
		Custodial:   decimal.NewFromInt(1200),
		Solvent:     true,
		Margin:      decimal.NewFromInt(200),
	}, nil
}

type mockBalanceTrendReader struct{}

func (m *mockBalanceTrendReader) GetBalanceTrends(ctx context.Context, filter core.BalanceTrendFilter) ([]core.BalanceTrendPoint, error) {
	return []core.BalanceTrendPoint{
		{Date: filter.From, Balance: decimal.NewFromInt(100), Inflow: decimal.NewFromInt(50), Outflow: decimal.Zero},
	}, nil
}

type mockPeriodCloser struct{}

func (m *mockPeriodCloser) ClosePeriod(ctx context.Context, input core.ClosePeriodInput) (*core.PeriodClose, error) {
	return &core.PeriodClose{UID: "pc-1", CloseBefore: input.CloseBefore, Note: input.Note, ActorID: input.ActorID, CreatedAt: time.Now()}, nil
}

func (m *mockPeriodCloser) ActiveCloseLine(ctx context.Context) (time.Time, error) {
	return time.Time{}, nil
}

func (m *mockPeriodCloser) ListPeriodCloses(ctx context.Context, limit int) ([]core.PeriodClose, error) {
	return []core.PeriodClose{{UID: "pc-1", CloseBefore: time.Now(), CreatedAt: time.Now()}}, nil
}

type mockTrialBalanceReader struct{}

func (m *mockTrialBalanceReader) TrialBalance(ctx context.Context, currencyUID string, asOf time.Time) (*core.TrialBalanceReport, error) {
	return &core.TrialBalanceReport{
		CurrencyUID: currencyUID,
		AsOf:        asOf,
		TotalDebit:  decimal.NewFromInt(100),
		TotalCredit: decimal.NewFromInt(100),
		Balanced:    true,
		Rows: []core.TrialBalanceRow{
			{ClassificationUID: "cls-1", ClassificationCode: "wallet", ClassificationName: "Wallet", NormalSide: core.NormalSideDebit, TotalDebit: decimal.NewFromInt(100), TotalCredit: decimal.Zero, Net: decimal.NewFromInt(100)},
		},
	}, nil
}

type mockQueryProvider struct{}

func (m *mockQueryProvider) GetJournal(ctx context.Context, uid string) (*core.Journal, []core.Entry, error) {
	j := &core.Journal{UID: uid, JournalTypeUID: "jt-1", IdempotencyKey: "test", TotalDebit: decimal.NewFromInt(100), TotalCredit: decimal.NewFromInt(100), CreatedAt: time.Now()}
	entries := []core.Entry{
		{JournalUID: uid, AccountHolder: 100, CurrencyUID: "cur-1", ClassificationUID: "cls-1", EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100), CreatedAt: time.Now()},
		{JournalUID: uid, AccountHolder: -100, CurrencyUID: "cur-1", ClassificationUID: "cls-1", EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100), CreatedAt: time.Now()},
	}
	return j, entries, nil
}

func (m *mockQueryProvider) ListJournals(ctx context.Context, cursor string, limit int32) ([]core.Journal, string, error) {
	return []core.Journal{
		{UID: "jr-1", JournalTypeUID: "jt-1", IdempotencyKey: "j1", TotalDebit: decimal.NewFromInt(100), TotalCredit: decimal.NewFromInt(100), CreatedAt: time.Now()},
	}, "", nil
}

func (m *mockQueryProvider) ListEntriesByAccount(ctx context.Context, holder int64, currencyUID, cursor string, limit int32) ([]core.Entry, string, error) {
	return []core.Entry{
		{JournalUID: "jr-1", AccountHolder: holder, CurrencyUID: currencyUID, ClassificationUID: "cls-1", EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100), CreatedAt: time.Now()},
	}, "", nil
}

func (m *mockQueryProvider) ListReservations(ctx context.Context, holder int64, status string, cursor string, limit int32) ([]core.Reservation, string, error) {
	return []core.Reservation{}, "", nil
}

func (m *mockQueryProvider) ListSnapshotsByDateRange(ctx context.Context, holder int64, currencyUID string, start, end time.Time) ([]core.BalanceSnapshot, error) {
	return []core.BalanceSnapshot{}, nil
}

func (m *mockQueryProvider) GetSystemRollups(ctx context.Context) ([]core.SystemRollup, error) {
	return []core.SystemRollup{
		{CurrencyUID: "cur-1", ClassificationUID: "cls-1", TotalBalance: decimal.NewFromInt(10000), UpdatedAt: time.Now()},
	}, nil
}

func (m *mockQueryProvider) GetHealthMetrics(ctx context.Context) (*core.HealthMetrics, error) {
	return &core.HealthMetrics{
		RollupQueueDepth:        3,
		CheckpointMaxAgeSeconds: 12,
		ActiveReservations:      5,
	}, nil
}

// --- Test helper ---

func newTestServer() *server.Server {
	return server.New(
		&mockJournalWriter{},
		&mockBalanceReader{},
		&mockReserver{},
		&mockBooker{},
		&mockBookingReader{},
		&mockEventReader{},
		&mockClassificationStore{},
		&mockJournalTypeStore{},
		&mockTemplateStore{},
		&mockCurrencyStore{},
		nil, // channels
		&mockReconciler{},
		&mockSnapshotter{},
		(*service.SystemRollupService)(nil), // not used directly
		&mockQueryProvider{},
		&mockAuditQuerier{},
		&mockPlatformBalanceReader{},
		&mockSolvencyChecker{},
		&mockBalanceTrendReader{},
		&mockFullReconciler{},
		&mockAccountPolicyStore{},
		&mockPeriodCloser{},
		&mockTrialBalanceReader{},
	)
}

// newTestServerWith creates a test server with custom overrides.
func newTestServerWith(opts ...func(*testServerOpts)) *server.Server {
	o := &testServerOpts{
		journals:         &mockJournalWriter{},
		balances:         &mockBalanceReader{},
		reserver:         &mockReserver{},
		booker:           &mockBooker{},
		bookingReader:    &mockBookingReader{},
		eventReader:      &mockEventReader{},
		classifications:  &mockClassificationStore{},
		journalTypes:     &mockJournalTypeStore{},
		templates:        &mockTemplateStore{},
		currencies:       &mockCurrencyStore{},
		reconciler:       &mockReconciler{},
		fullReconciler:   &mockFullReconciler{},
		snapshotter:      &mockSnapshotter{},
		queries:          &mockQueryProvider{},
		audit:            &mockAuditQuerier{},
		platformBalances: &mockPlatformBalanceReader{},
		solvency:         &mockSolvencyChecker{},
		balanceTrends:    &mockBalanceTrendReader{},
		accountPolicies:  &mockAccountPolicyStore{},
		periodCloser:     &mockPeriodCloser{},
		trialBalance:     &mockTrialBalanceReader{},
	}
	for _, fn := range opts {
		fn(o)
	}
	return server.New(
		o.journals, o.balances, o.reserver,
		o.booker, o.bookingReader, o.eventReader,
		o.classifications, o.journalTypes, o.templates, o.currencies,
		o.channels,
		o.reconciler, o.snapshotter, nil, o.queries,
		o.audit, o.platformBalances, o.solvency, o.balanceTrends,
		o.fullReconciler, o.accountPolicies,
		o.periodCloser, o.trialBalance,
	)
}

type testServerOpts struct {
	journals         core.JournalWriter
	balances         core.BalanceReader
	reserver         core.Reserver
	booker           core.Booker
	bookingReader    core.BookingReader
	eventReader      core.EventReader
	classifications  core.ClassificationStore
	journalTypes     core.JournalTypeStore
	templates        core.TemplateStore
	currencies       core.CurrencyStore
	channels         map[string]channel.Adapter
	reconciler       core.Reconciler
	fullReconciler   core.FullReconciler
	snapshotter      core.Snapshotter
	queries          core.QueryProvider
	audit            core.AuditQuerier
	platformBalances core.PlatformBalanceReader
	solvency         core.SolvencyChecker
	balanceTrends    core.BalanceTrendReader
	accountPolicies  core.AccountPolicyStore
	periodCloser     core.PeriodCloser
	trialBalance     core.TrialBalanceReader
}

func doRequest(srv http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

// parseEnvelope extracts the "data" field from the {code, message, data} envelope.
func parseEnvelope(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var env map[string]any
	require.NoError(t, json.Unmarshal(body, &env))
	data, ok := env["data"].(map[string]any)
	require.True(t, ok, "expected 'data' object in envelope, got: %v", env)
	return data
}

// parseEnvelopeList unwraps {code, message, data: {list: [...]}} — the
// uniform list shape (api-contract §6).
func parseEnvelopeList(t *testing.T, body []byte) []any {
	t.Helper()
	env := parseEnvelope(t, body)
	list, ok := env["list"].([]any)
	require.True(t, ok, "data.list missing or not an array: %v", env)
	return list
}

// --- Tests ---

func TestHealthEndpoint(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/system/health", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "ok", data["status"])
}

func TestPostJournal(t *testing.T) {
	srv := newTestServer()
	body := map[string]any{
		"journal_type_uid": "jt-1",
		"idempotency_key":  "test-123",
		"entries": []map[string]any{
			{"account_holder": 100, "currency_uid": "cur-1", "classification_uid": "cls-1", "entry_type": "debit", "amount": "100"},
			{"account_holder": -100, "currency_uid": "cur-1", "classification_uid": "cls-1", "entry_type": "credit", "amount": "100"},
		},
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/journals", body)
	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestPostJournal_PassesEventID(t *testing.T) {
	var captured core.JournalInput
	srv := newTestServerWith(func(o *testServerOpts) {
		o.journals = &mockJournalWriter{
			postFn: func(ctx context.Context, input core.JournalInput) (*core.Journal, error) {
				captured = input
				return &core.Journal{
					UID:            "uid-1",
					JournalTypeUID: input.JournalTypeUID,
					IdempotencyKey: input.IdempotencyKey,
					EventUID:       input.EventUID,
					TotalDebit:     decimal.NewFromInt(100),
					TotalCredit:    decimal.NewFromInt(100),
					CreatedAt:      time.Now(),
				}, nil
			},
		}
	})

	body := map[string]any{
		"journal_type_uid": "jt-1",
		"idempotency_key":  "test-event-link",
		"event_uid":        "evt-77",
		"entries": []map[string]any{
			{"account_holder": 100, "currency_uid": "cur-1", "classification_uid": "cls-1", "entry_type": "debit", "amount": "100"},
			{"account_holder": -100, "currency_uid": "cur-1", "classification_uid": "cls-1", "entry_type": "credit", "amount": "100"},
		},
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/journals", body)
	require.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, "evt-77", captured.EventUID)

	data := parseEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "evt-77", data["event_uid"])
}

func TestPostDepositTolerance(t *testing.T) {
	var calls []string
	srv := newTestServerWith(func(o *testServerOpts) {
		o.journals = &mockJournalWriter{
			templateFn: func(ctx context.Context, code string, params core.TemplateParams) (*core.Journal, error) {
				calls = append(calls, code)
				return &core.Journal{
					UID:            fmt.Sprintf("jr-%d", len(calls)),
					JournalTypeUID: "jt-1",
					IdempotencyKey: params.IdempotencyKey,
					Metadata:       params.Metadata,
					TotalDebit:     decimal.NewFromInt(100),
					TotalCredit:    decimal.NewFromInt(100),
					CreatedAt:      time.Now(),
				}, nil
			},
		}
	})

	body := map[string]any{
		"holder_id":       100,
		"currency_uid":    "cur-1",
		"idempotency_key": "dep-tol-1",
		"expected_amount": "100",
		"actual_amount":   "98",
		"tolerance":       "5",
		"source":          "deposit",
		"metadata":        map[string]string{"request_id": "req-1"},
	}

	w := doRequest(srv, http.MethodPost, "/api/v1/journals/deposit-tolerance", body)
	assert.Equal(t, http.StatusCreated, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "shortfall_auto_released", data["outcome"])
	assert.Equal(t, "2", data["delta"])
	assert.Equal(t, false, data["requires_manual_review"])

	journals, ok := data["journals"].([]any)
	require.True(t, ok)
	assert.Len(t, journals, 2)
	assert.Equal(t, []string{"deposit_confirm_pending", "deposit_release_pending"}, calls)
}

func TestPostTemplate_PassesEventID(t *testing.T) {
	var capturedCode string
	var captured core.TemplateParams
	srv := newTestServerWith(func(o *testServerOpts) {
		o.journals = &mockJournalWriter{
			templateFn: func(ctx context.Context, code string, params core.TemplateParams) (*core.Journal, error) {
				capturedCode = code
				captured = params
				return &core.Journal{
					UID:            "jr-2",
					JournalTypeUID: "jt-1",
					IdempotencyKey: params.IdempotencyKey,
					EventUID:       params.EventUID,
					TotalDebit:     decimal.NewFromInt(50),
					TotalCredit:    decimal.NewFromInt(50),
					CreatedAt:      time.Now(),
				}, nil
			},
		}
	})

	body := map[string]any{
		"template_code":   "deposit_confirm",
		"holder_id":       100,
		"currency_uid":    "cur-1",
		"idempotency_key": "tmpl-event-link",
		"event_uid":       "evt-88",
		"amounts": map[string]any{
			"amount": "50",
		},
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/journals/template", body)
	require.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, "deposit_confirm", capturedCode)
	assert.Equal(t, "evt-88", captured.EventUID)

	data := parseEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "evt-88", data["event_uid"])
}

func TestPostJournalUnbalanced(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.journals = &mockJournalWriter{
			postFn: func(ctx context.Context, input core.JournalInput) (*core.Journal, error) {
				return nil, fmt.Errorf("core: journal: unbalanced — debit=100 credit=50: %w", core.ErrUnbalancedJournal)
			},
		}
	})
	body := map[string]any{
		"journal_type_uid": "jt-1",
		"idempotency_key":  "test-unbalanced",
		"entries": []map[string]any{
			{"account_holder": 100, "currency_uid": "cur-1", "classification_uid": "cls-1", "entry_type": "debit", "amount": "100"},
			{"account_holder": -100, "currency_uid": "cur-1", "classification_uid": "cls-1", "entry_type": "credit", "amount": "50"},
		},
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/journals", body)
	// Unbalanced journal maps to bizcode 14003 → HTTP 422
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
}

func TestReverseJournal(t *testing.T) {
	srv := newTestServer()
	body := map[string]any{"reason": "error correction"}
	w := doRequest(srv, http.MethodPost, "/api/v1/journals/1/reverse", body)
	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestGetJournalWithEntries(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/journals/1", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	entries, ok := data["entries"].([]any)
	require.True(t, ok)
	assert.Len(t, entries, 2)
}

func TestListJournalsWithCursor(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/journals?limit=10", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	journals, ok := data["list"].([]any)
	require.True(t, ok)
	assert.Len(t, journals, 1)
}

func TestGetBalances(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/balances/100?currency_uid=cur-1", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	data := parseEnvelopeList(t, w.Body.Bytes())
	assert.Len(t, data, 2)
}

func TestGetBalanceBreakdown(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/balances/100/cur-1/breakdown", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "80", data["available"])
	assert.Equal(t, "100", data["pending"])
	assert.Equal(t, "20", data["locked"])
	assert.Equal(t, "200", data["total"])
}

func TestGetBalanceBreakdown_InvalidHolder(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/balances/abc/cur-1/breakdown", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestBatchBalances(t *testing.T) {
	srv := newTestServer()
	body := map[string]any{"holder_ids": []int64{100, 200}, "currency_uid": "cur-1"}
	w := doRequest(srv, http.MethodPost, "/api/v1/balances/batch", body)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestCreateReservation(t *testing.T) {
	srv := newTestServer()
	body := map[string]any{
		"account_holder":  100,
		"currency_uid":    "cur-1",
		"amount":          "50.00",
		"idempotency_key": "res-1",
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/reservations", body)
	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestSettleReservation(t *testing.T) {
	srv := newTestServer()
	body := map[string]any{"actual_amount": "48.50"}
	w := doRequest(srv, http.MethodPost, "/api/v1/reservations/1/settle", body)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestReleaseReservation(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodPost, "/api/v1/reservations/1/release", nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

// --- Booking lifecycle tests ---

func TestCreateBooking(t *testing.T) {
	srv := newTestServer()
	body := map[string]any{
		"classification_code": "deposit",
		"account_holder":      100,
		"currency_uid":        "cur-1",
		"amount":              "500.00",
		"channel_name":        "crypto",
		"idempotency_key":     "op-1",
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/bookings", body)
	assert.Equal(t, http.StatusCreated, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "bk-1", data["uid"])
	assert.Equal(t, "pending", data["status"])
}

func TestTransitionBooking(t *testing.T) {
	srv := newTestServer()
	body := map[string]any{
		"to_status":   "confirmed",
		"channel_ref": "tx-abc",
		"amount":      "500.00",
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/bookings/1/transition", body)
	assert.Equal(t, http.StatusOK, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "confirmed", data["to_status"])
}

func TestGetBooking(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/bookings/bk-1", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "bk-1", data["uid"])
}

func TestListBookings(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/bookings?holder=100&status=pending", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	ops, ok := data["list"].([]any)
	require.True(t, ok)
	assert.Len(t, ops, 1)
}

// --- Event tests ---

func TestGetEvent(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/events/evt-1", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "evt-1", data["uid"])
	assert.Equal(t, "confirmed", data["to_status"])
}

func TestListEvents(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/events?booking_uid=bk-1", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	events, ok := data["list"].([]any)
	require.True(t, ok)
	assert.Len(t, events, 1)
}

// --- Metadata tests ---

func TestClassificationCRUD(t *testing.T) {
	srv := newTestServer()

	// Create
	body := map[string]any{"code": "REVENUE", "name": "Revenue", "normal_side": "credit"}
	w := doRequest(srv, http.MethodPost, "/api/v1/classifications", body)
	assert.Equal(t, http.StatusCreated, w.Code)

	// List
	w = doRequest(srv, http.MethodGet, "/api/v1/classifications?active_only=true", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	// Deactivate
	w = doRequest(srv, http.MethodPost, "/api/v1/classifications/1/deactivate", nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestCreateClassification_WithLifecycle(t *testing.T) {
	srv := newTestServer()

	body := map[string]any{
		"code":        "deposit",
		"name":        "Deposit",
		"normal_side": "credit",
		"lifecycle": map[string]any{
			"initial":  "pending",
			"terminal": []string{"confirmed", "expired"},
			"transitions": map[string]any{
				"pending": []string{"confirmed", "expired"},
			},
		},
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/classifications", body)
	assert.Equal(t, http.StatusCreated, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	lifecycle, ok := data["lifecycle"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "pending", lifecycle["initial"])
}

func TestJournalTypeCRUD(t *testing.T) {
	srv := newTestServer()

	body := map[string]any{"code": "FEE", "name": "Fee"}
	w := doRequest(srv, http.MethodPost, "/api/v1/journal-types", body)
	assert.Equal(t, http.StatusCreated, w.Code)

	w = doRequest(srv, http.MethodGet, "/api/v1/journal-types", nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestTemplateCRUD(t *testing.T) {
	srv := newTestServer()

	body := map[string]any{
		"code":             "deposit",
		"name":             "Deposit",
		"journal_type_uid": "jt-1",
		"lines": []map[string]any{
			{"classification_uid": "cls-1", "entry_type": "debit", "holder_role": "user", "amount_key": "amount", "sort_order": 1},
			{"classification_uid": "cls-1", "entry_type": "credit", "holder_role": "system", "amount_key": "amount", "sort_order": 2},
		},
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/templates", body)
	assert.Equal(t, http.StatusCreated, w.Code)

	w = doRequest(srv, http.MethodGet, "/api/v1/templates?active_only=true", nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestTemplateCreate_RejectsEmptyLines(t *testing.T) {
	srv := newTestServer()

	body := map[string]any{
		"code":             "broken",
		"name":             "Broken",
		"journal_type_uid": "jt-1",
		"lines":            []map[string]any{},
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/templates", body)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestTemplatePreview(t *testing.T) {
	srv := newTestServer()

	body := map[string]any{
		"holder_id":    100,
		"currency_uid": "cur-1",
		"amounts":      map[string]string{"amount": "500"},
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/templates/deposit/preview", body)
	assert.Equal(t, http.StatusOK, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	entries, ok := data["entries"].([]any)
	require.True(t, ok)
	assert.Len(t, entries, 2)
}

func TestCurrencyCRUD(t *testing.T) {
	srv := newTestServer()

	body := map[string]any{"code": "USDC", "name": "USD Coin", "exponent": 6}
	w := doRequest(srv, http.MethodPost, "/api/v1/currencies", body)
	assert.Equal(t, http.StatusCreated, w.Code)

	w = doRequest(srv, http.MethodGet, "/api/v1/currencies", nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestCreateCurrency_MissingExponentRejected(t *testing.T) {
	srv := newTestServer()

	// exponent=0 is legal (JPY), so an omitted field must be a 400 — not
	// silently decoded to 0 and accepted as a zero-decimal currency.
	body := map[string]any{"code": "USDC", "name": "USD Coin"}
	w := doRequest(srv, http.MethodPost, "/api/v1/currencies", body)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreateCurrency_ExplicitZeroExponentAccepted(t *testing.T) {
	srv := newTestServer()

	body := map[string]any{"code": "JPY", "name": "Japanese Yen", "exponent": 0}
	w := doRequest(srv, http.MethodPost, "/api/v1/currencies", body)
	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestReconcileGlobal(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodPost, "/api/v1/reconcile", nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestReconcileAccount(t *testing.T) {
	srv := newTestServer()
	body := map[string]any{"holder": 100, "currency_uid": "cur-1"}
	w := doRequest(srv, http.MethodPost, "/api/v1/reconcile/account", body)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestReconcileFull(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodPost, "/api/v1/reconcile/full", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Data core.ReconcileReport `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Data.OverallPassed)
}

func TestReconcileFull_PropagatesError(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.fullReconciler = &mockFullReconciler{err: assert.AnError}
	})
	w := doRequest(srv, http.MethodPost, "/api/v1/reconcile/full", nil)
	assert.NotEqual(t, http.StatusOK, w.Code)
}

func TestSystemBalances(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/system/balances", nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestListSnapshots(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/snapshots?holder=100&currency_uid=cur-1&start=2026-01-01&end=2026-12-31", nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestListEntriesByAccount(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/entries?holder=100&currency_uid=cur-1", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	entries, ok := data["list"].([]any)
	require.True(t, ok)
	assert.Len(t, entries, 1)
}

func TestInvalidBody(t *testing.T) {
	srv := newTestServer()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/journals", bytes.NewBufferString("{invalid"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestMissingRequiredParams(t *testing.T) {
	srv := newTestServer()

	// Missing holder on balances
	w := doRequest(srv, http.MethodGet, "/api/v1/balances/abc?currency_uid=cur-1", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// Missing currency_id on balances
	w = doRequest(srv, http.MethodGet, "/api/v1/balances/100", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// Empty batch
	w = doRequest(srv, http.MethodPost, "/api/v1/balances/batch", map[string]any{"holder_ids": []int64{}, "currency_uid": "cur-1"})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- Error path tests ---

func TestPostJournal_NotFound(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.journals = &mockJournalWriter{
			reverseFn: func(ctx context.Context, journalUID string, reason string) (*core.Journal, error) {
				return nil, fmt.Errorf("postgres: reverse journal: journal %s: %w", journalUID, core.ErrNotFound)
			},
		}
	})
	body := map[string]any{"reason": "error correction"}
	w := doRequest(srv, http.MethodPost, "/api/v1/journals/999/reverse", body)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestPostJournal_InvalidInput(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.journals = &mockJournalWriter{
			postFn: func(ctx context.Context, input core.JournalInput) (*core.Journal, error) {
				return nil, fmt.Errorf("validation: %w", core.ErrInvalidInput)
			},
		}
	})
	body := map[string]any{
		"journal_type_uid": "jt-1",
		"idempotency_key":  "test-invalid-input",
		"entries": []map[string]any{
			{"account_holder": 100, "currency_uid": "cur-1", "classification_uid": "cls-1", "entry_type": "debit", "amount": "100"},
			{"account_holder": -100, "currency_uid": "cur-1", "classification_uid": "cls-1", "entry_type": "credit", "amount": "100"},
		},
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/journals", body)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestPostJournal_DuplicateJournal(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.journals = &mockJournalWriter{
			postFn: func(ctx context.Context, input core.JournalInput) (*core.Journal, error) {
				return nil, fmt.Errorf("idempotency: %w", core.ErrDuplicateJournal)
			},
		}
	})
	body := map[string]any{
		"journal_type_uid": "jt-1",
		"idempotency_key":  "test-duplicate",
		"entries": []map[string]any{
			{"account_holder": 100, "currency_uid": "cur-1", "classification_uid": "cls-1", "entry_type": "debit", "amount": "100"},
			{"account_holder": -100, "currency_uid": "cur-1", "classification_uid": "cls-1", "entry_type": "credit", "amount": "100"},
		},
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/journals", body)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
}

func TestPostJournal_Conflict(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.journals = &mockJournalWriter{
			postFn: func(ctx context.Context, input core.JournalInput) (*core.Journal, error) {
				return nil, fmt.Errorf("idempotency payload mismatch: %w", core.ErrConflict)
			},
		}
	})
	body := map[string]any{
		"journal_type_uid": "jt-1",
		"idempotency_key":  "test-conflict",
		"entries": []map[string]any{
			{"account_holder": 100, "currency_uid": "cur-1", "classification_uid": "cls-1", "entry_type": "debit", "amount": "100"},
			{"account_holder": -100, "currency_uid": "cur-1", "classification_uid": "cls-1", "entry_type": "credit", "amount": "100"},
		},
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/journals", body)
	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestPostJournal_InternalError(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.journals = &mockJournalWriter{
			postFn: func(ctx context.Context, input core.JournalInput) (*core.Journal, error) {
				return nil, fmt.Errorf("database connection failed")
			},
		}
	})
	body := map[string]any{
		"journal_type_uid": "jt-1",
		"idempotency_key":  "test-internal",
		"entries": []map[string]any{
			{"account_holder": 100, "currency_uid": "cur-1", "classification_uid": "cls-1", "entry_type": "debit", "amount": "100"},
			{"account_holder": -100, "currency_uid": "cur-1", "classification_uid": "cls-1", "entry_type": "credit", "amount": "100"},
		},
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/journals", body)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestReverseJournal_NotFound(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.journals = &mockJournalWriter{
			reverseFn: func(ctx context.Context, journalUID string, reason string) (*core.Journal, error) {
				return nil, fmt.Errorf("postgres: reverse journal: %w", core.ErrNotFound)
			},
		}
	})
	w := doRequest(srv, http.MethodPost, "/api/v1/journals/42/reverse", map[string]any{"reason": "not found test"})
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestReverseJournal_MissingReason(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodPost, "/api/v1/journals/1/reverse", map[string]any{"reason": ""})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSettleReservation_NotFound(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.reserver = &mockReserver{
			settleFn: func(ctx context.Context, reservationUID string, actualAmount decimal.Decimal) error {
				return fmt.Errorf("postgres: settle reservation: %w", core.ErrNotFound)
			},
		}
	})
	w := doRequest(srv, http.MethodPost, "/api/v1/reservations/99/settle", map[string]any{"actual_amount": "50.00"})
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestSettleReservation_InvalidTransition(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.reserver = &mockReserver{
			settleFn: func(ctx context.Context, reservationUID string, actualAmount decimal.Decimal) error {
				return fmt.Errorf("service: settle: %w", core.ErrInvalidTransition)
			},
		}
	})
	w := doRequest(srv, http.MethodPost, "/api/v1/reservations/1/settle", map[string]any{"actual_amount": "50.00"})
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
}

func TestCreateReservation_InvalidInput(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.reserver = &mockReserver{
			reserveFn: func(ctx context.Context, input core.ReserveInput) (*core.Reservation, error) {
				return nil, fmt.Errorf("service: reserve: %w", core.ErrInvalidInput)
			},
		}
	})
	body := map[string]any{
		"account_holder":  100,
		"currency_uid":    "cur-1",
		"amount":          "50.00",
		"idempotency_key": "res-invalid",
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/reservations", body)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- Booking error path tests ---

func TestCreateBooking_NotFound(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.booker = &mockBooker{
			createFn: func(ctx context.Context, input core.CreateBookingInput) (*core.Booking, error) {
				return nil, fmt.Errorf("service: create booking: classification not found: %w", core.ErrNotFound)
			},
		}
	})
	body := map[string]any{
		"classification_code": "unknown",
		"account_holder":      100,
		"currency_uid":        "cur-1",
		"amount":              "500.00",
		"idempotency_key":     "op-notfound",
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/bookings", body)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestTransition_InvalidTransition(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.booker = &mockBooker{
			transitionFn: func(ctx context.Context, input core.TransitionInput) (*core.Event, error) {
				return nil, fmt.Errorf("service: transition: %w", core.ErrInvalidTransition)
			},
		}
	})
	body := map[string]any{
		"to_status": "confirmed",
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/bookings/1/transition", body)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
}

func TestGetBooking_NotFound(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.bookingReader = &mockBookingReader{
			getFn: func(ctx context.Context, uid string) (*core.Booking, error) {
				return nil, fmt.Errorf("postgres: get booking: %w", core.ErrNotFound)
			},
		}
	})
	w := doRequest(srv, http.MethodGet, "/api/v1/bookings/999", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGetEvent_NotFound(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.eventReader = &mockEventReader{
			getFn: func(ctx context.Context, uid string) (*core.Event, error) {
				return nil, fmt.Errorf("postgres: get event: %w", core.ErrNotFound)
			},
		}
	})
	w := doRequest(srv, http.MethodGet, "/api/v1/events/999", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestCreateBooking_InsufficientBalance(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.booker = &mockBooker{
			createFn: func(ctx context.Context, input core.CreateBookingInput) (*core.Booking, error) {
				return nil, fmt.Errorf("service: create booking: %w", core.ErrInsufficientBalance)
			},
		}
	})
	body := map[string]any{
		"classification_code": "withdrawal",
		"account_holder":      100,
		"currency_uid":        "cur-1",
		"amount":              "99999.00",
		"idempotency_key":     "op-insufficient",
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/bookings", body)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
}

// serverErrorEnvelope mirrors httpx.ErrorBody for asserting on the
// "retryable" field from black-box HTTP responses.
type serverErrorEnvelope struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// --- Audit ---

func TestAuditListJournals_ByAccount(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/audit/journals?holder=100&currency_uid=cur-1", nil)
	require.Equal(t, http.StatusOK, w.Code)
	page := parseEnvelope(t, w.Body.Bytes())
	arr, ok := page["list"].([]any)
	require.True(t, ok)
	assert.NotEmpty(t, arr)
}

func TestAuditListJournals_ByTimeRange(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/audit/journals?from=2026-01-01T00:00:00Z&to=2026-01-31T00:00:00Z", nil)
	require.Equal(t, http.StatusOK, w.Code)
}

func TestAuditListJournals_NoFilter(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/audit/journals", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAuditListJournals_HolderWithoutCurrency(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/audit/journals?holder=100", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAuditListJournals_InvalidFrom(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/audit/journals?from=not-a-date", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAuditTraceBooking(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/audit/bookings/42/trace", nil)
	require.Equal(t, http.StatusOK, w.Code)
	data := parseEnvelope(t, w.Body.Bytes())
	assert.Contains(t, data, "booking")
	assert.Contains(t, data, "events")
	assert.Contains(t, data, "journals")
}

func TestAuditTraceBooking_NotFound(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.audit = &mockAuditQuerier{
			traceBookingFn: func(ctx context.Context, bookingUID string) (*core.BookingTrace, error) {
				return nil, fmt.Errorf("postgres: trace booking: %w", core.ErrNotFound)
			},
		}
	})
	w := doRequest(srv, http.MethodGet, "/api/v1/audit/bookings/999/trace", nil)
	require.Equal(t, http.StatusNotFound, w.Code)

	var env serverErrorEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.False(t, env.Retryable, "not-found is a permanent outcome, not retryable")
}

func TestAuditTraceBooking_InternalError_Retryable(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.audit = &mockAuditQuerier{
			traceBookingFn: func(ctx context.Context, bookingUID string) (*core.BookingTrace, error) {
				return nil, fmt.Errorf("postgres: trace booking: connection reset")
			},
		}
	})
	w := doRequest(srv, http.MethodGet, "/api/v1/audit/bookings/999/trace", nil)
	require.Equal(t, http.StatusInternalServerError, w.Code)

	var env serverErrorEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.True(t, env.Retryable, "unclassified internal errors default to retryable")
}

func TestAuditTraceBooking_OpaqueUIDPassesThrough(t *testing.T) {
	// uids are opaque strings — the handler no longer rejects non-numeric
	// path params; resolution happens in the store (unknown uid → 404 there).
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/audit/bookings/any-opaque-uid/trace", nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAuditListReversals(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/audit/journals/10/reversals", nil)
	require.Equal(t, http.StatusOK, w.Code)
	arr := parseEnvelopeList(t, w.Body.Bytes())
	assert.NotEmpty(t, arr)
}

// --- Platform ---

func TestPlatformBalances(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/platform/balances?currency_uid=cur-1", nil)
	require.Equal(t, http.StatusOK, w.Code)
	data := parseEnvelope(t, w.Body.Bytes())
	assert.Contains(t, data, "user_side")
	assert.Contains(t, data, "system_side")
}

func TestPlatformBalances_MissingCurrency(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/platform/balances", nil)
	require.Equal(t, http.StatusBadRequest, w.Code)

	var env serverErrorEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.False(t, env.Retryable, "invalid input is not retryable")
}

func TestPlatformSolvency(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/platform/solvency?currency_uid=cur-1", nil)
	require.Equal(t, http.StatusOK, w.Code)
	data := parseEnvelope(t, w.Body.Bytes())
	assert.Equal(t, true, data["solvent"])
}

func TestPlatformSolvency_Insolvent(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.solvency = &mockSolvencyChecker{
			checkFn: func(ctx context.Context, currencyUID string) (*core.SolvencyReport, error) {
				return &core.SolvencyReport{
					CurrencyUID: currencyUID,
					Liability:   decimal.NewFromInt(1000),
					Custodial:   decimal.NewFromInt(800),
					Solvent:     false,
					Margin:      decimal.NewFromInt(-200),
				}, nil
			},
		}
	})
	w := doRequest(srv, http.MethodGet, "/api/v1/platform/solvency?currency_uid=cur-1", nil)
	require.Equal(t, http.StatusOK, w.Code)
	data := parseEnvelope(t, w.Body.Bytes())
	assert.Equal(t, false, data["solvent"])
	assert.Equal(t, "-200", data["margin"])
}

func TestPlatformSolvency_MissingCurrency(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/platform/solvency", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- Balance trends ---

func TestBalanceTrends(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/balances/trends?holder=100&currency_uid=cur-1&from=2026-01-01T00:00:00Z&to=2026-01-31T00:00:00Z", nil)
	require.Equal(t, http.StatusOK, w.Code)
	arr := parseEnvelopeList(t, w.Body.Bytes())
	assert.NotEmpty(t, arr)
}

func TestBalanceTrends_MissingFrom(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/balances/trends?holder=100&currency_uid=cur-1&to=2026-01-31T00:00:00Z", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestBalanceTrends_MissingHolder(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/balances/trends?currency_uid=cur-1&from=2026-01-01T00:00:00Z&to=2026-01-31T00:00:00Z", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestBalanceTrends_InvalidTo(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/balances/trends?holder=100&currency_uid=cur-1&from=2026-01-01T00:00:00Z&to=not-a-date", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- Idempotency-Key header alias (api-contract §9) ---

func TestIdempotencyHeaderAlias_InjectsIntoBody(t *testing.T) {
	captured := ""
	srv := newTestServerWith(func(o *testServerOpts) {
		o.journals = &mockJournalWriter{postFn: func(ctx context.Context, input core.JournalInput) (*core.Journal, error) {
			captured = input.IdempotencyKey
			return &core.Journal{UID: "uid-1", IdempotencyKey: input.IdempotencyKey}, nil
		}}
	})
	body := map[string]any{
		"journal_type_uid": "jt-1",
		"entries": []map[string]any{
			{"account_holder": 1, "currency_uid": "cur-1", "classification_uid": "cls-1", "entry_type": "debit", "amount": "5"},
			{"account_holder": -1, "currency_uid": "cur-1", "classification_uid": "cls-1", "entry_type": "credit", "amount": "5"},
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/journals", jsonBody(t, body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "hdr-key-1")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Equal(t, "hdr-key-1", captured)
}

func TestIdempotencyHeaderAlias_MismatchRejected(t *testing.T) {
	srv := newTestServer()
	body := map[string]any{"idempotency_key": "body-key", "journal_type_uid": "jt-1"}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/journals", jsonBody(t, body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "different-key")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func jsonBody(t *testing.T, v any) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, json.NewEncoder(&buf).Encode(v))
	return &buf
}

// Pins the read-surface auth requirement: with API keys configured, EVERY
// endpoint — reads included — demands a bearer key (holder is a guessable
// int64; an open GET surface exposes every holder's balances and history).
// Only the liveness/readiness probes stay open for Kubernetes.
func TestAuth_ReadsRequireKeyWhenConfigured(t *testing.T) {
	cfg := &server.Config{Env: "dev", CORSAllowOrigin: "*", MaxBodyBytes: 256 * 1024,
		APIKeys: []server.APIKey{{Name: "test", Scope: server.ScopeAdmin, Secret: []byte("test-key-1")}}}
	srv := server.NewWithConfig(cfg,
		&mockJournalWriter{},
		&mockBalanceReader{},
		&mockReserver{},
		&mockBooker{},
		&mockBookingReader{},
		&mockEventReader{},
		&mockClassificationStore{},
		&mockJournalTypeStore{},
		&mockTemplateStore{},
		&mockCurrencyStore{},
		nil,
		&mockReconciler{},
		&mockSnapshotter{},
		(*service.SystemRollupService)(nil),
		&mockQueryProvider{},
		&mockAuditQuerier{},
		&mockPlatformBalanceReader{},
		&mockSolvencyChecker{},
		&mockBalanceTrendReader{},
		&mockFullReconciler{},
		&mockAccountPolicyStore{},
		&mockPeriodCloser{},
		&mockTrialBalanceReader{},
	)

	// Read without key → rejected.
	w := doRequest(srv, http.MethodGet, "/api/v1/balances/100?currency_uid=cur-1", nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	// Read with key → accepted.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/balances/100?currency_uid=cur-1", nil)
	req.Header.Set("Authorization", "Bearer test-key-1")
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// Probes stay open without a key.
	w = doRequest(srv, http.MethodGet, "/api/v1/system/health", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	w = doRequest(srv, http.MethodGet, "/api/v1/system/ready", nil)
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// Webhook callbacks are exempt from bearer auth: external channels cannot
// hold our API keys — the channel adapter's signature verification (HMAC) is
// that surface's authentication, matching the OpenAPI `security: []`
// declaration. (An unknown channel still 404s, but must not 401.)
func TestAuth_WebhookPathExemptFromBearerAuth(t *testing.T) {
	cfg := &server.Config{Env: "dev", CORSAllowOrigin: "*", MaxBodyBytes: 256 * 1024,
		APIKeys: []server.APIKey{{Name: "test", Scope: server.ScopeAdmin, Secret: []byte("test-key-1")}}}
	srv := server.NewWithConfig(cfg,
		&mockJournalWriter{},
		&mockBalanceReader{},
		&mockReserver{},
		&mockBooker{},
		&mockBookingReader{},
		&mockEventReader{},
		&mockClassificationStore{},
		&mockJournalTypeStore{},
		&mockTemplateStore{},
		&mockCurrencyStore{},
		nil,
		&mockReconciler{},
		&mockSnapshotter{},
		(*service.SystemRollupService)(nil),
		&mockQueryProvider{},
		&mockAuditQuerier{},
		&mockPlatformBalanceReader{},
		&mockSolvencyChecker{},
		&mockBalanceTrendReader{},
		&mockFullReconciler{},
		&mockAccountPolicyStore{},
		&mockPeriodCloser{},
		&mockTrialBalanceReader{},
	)

	w := doRequest(srv, http.MethodPost, "/api/v1/webhooks/evm", map[string]any{"payload": "x"})
	assert.NotEqual(t, http.StatusUnauthorized, w.Code, "webhook surface must not demand a bearer key; got %d", w.Code)
}

// newScopedTestServer builds a server with the given API keys configured so
// scope enforcement is active.
func newScopedTestServer(keys ...server.APIKey) *server.Server {
	cfg := &server.Config{Env: "dev", CORSAllowOrigin: "*", MaxBodyBytes: 256 * 1024, APIKeys: keys}
	return server.NewWithConfig(cfg,
		&mockJournalWriter{},
		&mockBalanceReader{},
		&mockReserver{},
		&mockBooker{},
		&mockBookingReader{},
		&mockEventReader{},
		&mockClassificationStore{},
		&mockJournalTypeStore{},
		&mockTemplateStore{},
		&mockCurrencyStore{},
		nil,
		&mockReconciler{},
		&mockSnapshotter{},
		(*service.SystemRollupService)(nil),
		&mockQueryProvider{},
		&mockAuditQuerier{},
		&mockPlatformBalanceReader{},
		&mockSolvencyChecker{},
		&mockBalanceTrendReader{},
		&mockFullReconciler{},
		&mockAccountPolicyStore{},
		&mockPeriodCloser{},
		&mockTrialBalanceReader{},
	)
}

func doAuthedRequest(t *testing.T, srv *server.Server, method, path, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, path, jsonBody(t, body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

// Pins the scope ladder: read < write < admin. A read key can query but not
// post; a write key can post journals but not mutate configuration; an
// admin key can do everything.
func TestAuth_ScopeEnforcement(t *testing.T) {
	srv := newScopedTestServer(
		server.APIKey{Name: "reporter", Scope: server.ScopeRead, Secret: []byte("read-key")},
		server.APIKey{Name: "app", Scope: server.ScopeWrite, Secret: []byte("write-key")},
		server.APIKey{Name: "ops", Scope: server.ScopeAdmin, Secret: []byte("admin-key")},
	)

	journalBody := map[string]any{"idempotency_key": "k1", "journal_type_uid": "jt-1"}
	classificationBody := map[string]any{"code": "c1", "name": "C1"}

	// read key: GETs pass, business writes and admin writes are forbidden.
	w := doAuthedRequest(t, srv, http.MethodGet, "/api/v1/balances/100?currency_uid=cur-1", "read-key", nil)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = doAuthedRequest(t, srv, http.MethodPost, "/api/v1/balances/batch", "read-key",
		map[string]any{"holders": []int64{100}, "currency_uid": "cur-1"})
	assert.NotEqual(t, http.StatusForbidden, w.Code, "batch balance lookup is a semantic read")
	w = doAuthedRequest(t, srv, http.MethodPost, "/api/v1/journals", "read-key", journalBody)
	assert.Equal(t, http.StatusForbidden, w.Code)
	w = doAuthedRequest(t, srv, http.MethodPost, "/api/v1/classifications", "read-key", classificationBody)
	assert.Equal(t, http.StatusForbidden, w.Code)

	// write key: business writes pass, config mutations are forbidden.
	w = doAuthedRequest(t, srv, http.MethodPost, "/api/v1/journals", "write-key", journalBody)
	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	w = doAuthedRequest(t, srv, http.MethodGet, "/api/v1/journals", "write-key", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	w = doAuthedRequest(t, srv, http.MethodPost, "/api/v1/classifications", "write-key", classificationBody)
	assert.Equal(t, http.StatusForbidden, w.Code)
	w = doAuthedRequest(t, srv, http.MethodPost, "/api/v1/periods/close", "write-key", map[string]any{})
	assert.Equal(t, http.StatusForbidden, w.Code)

	// admin key: everything passes scope (handler-level validation may still 4xx).
	w = doAuthedRequest(t, srv, http.MethodPost, "/api/v1/classifications", "admin-key", classificationBody)
	assert.NotEqual(t, http.StatusForbidden, w.Code)
	w = doAuthedRequest(t, srv, http.MethodPost, "/api/v1/journals", "admin-key",
		map[string]any{"idempotency_key": "k2", "journal_type_uid": "jt-1"})
	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
}
