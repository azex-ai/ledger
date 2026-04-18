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

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/server"
	"github.com/azex-ai/ledger/service"
)

// --- Mock implementations ---

type mockJournalWriter struct {
	postFn      func(ctx context.Context, input core.JournalInput) (*core.Journal, error)
	templateFn  func(ctx context.Context, code string, params core.TemplateParams) (*core.Journal, error)
	reverseFn   func(ctx context.Context, journalID int64, reason string) (*core.Journal, error)
}

func (m *mockJournalWriter) PostJournal(ctx context.Context, input core.JournalInput) (*core.Journal, error) {
	if m.postFn != nil {
		return m.postFn(ctx, input)
	}
	return &core.Journal{ID: 1, JournalTypeID: input.JournalTypeID, IdempotencyKey: input.IdempotencyKey, TotalDebit: decimal.NewFromInt(100), TotalCredit: decimal.NewFromInt(100), CreatedAt: time.Now()}, nil
}

func (m *mockJournalWriter) ExecuteTemplate(ctx context.Context, code string, params core.TemplateParams) (*core.Journal, error) {
	if m.templateFn != nil {
		return m.templateFn(ctx, code, params)
	}
	return &core.Journal{ID: 2, JournalTypeID: 1, IdempotencyKey: params.IdempotencyKey, TotalDebit: decimal.NewFromInt(50), TotalCredit: decimal.NewFromInt(50), CreatedAt: time.Now()}, nil
}

func (m *mockJournalWriter) ReverseJournal(ctx context.Context, journalID int64, reason string) (*core.Journal, error) {
	if m.reverseFn != nil {
		return m.reverseFn(ctx, journalID, reason)
	}
	return &core.Journal{ID: 3, JournalTypeID: 1, IdempotencyKey: fmt.Sprintf("reversal:%d:%s", journalID, reason), ReversalOf: &journalID, TotalDebit: decimal.NewFromInt(100), TotalCredit: decimal.NewFromInt(100), CreatedAt: time.Now()}, nil
}

type mockBalanceReader struct{}

func (m *mockBalanceReader) GetBalance(ctx context.Context, holder int64, currencyID, classificationID int64) (decimal.Decimal, error) {
	return decimal.NewFromInt(1000), nil
}

func (m *mockBalanceReader) GetBalances(ctx context.Context, holder int64, currencyID int64) ([]core.Balance, error) {
	return []core.Balance{
		{AccountHolder: holder, CurrencyID: currencyID, ClassificationID: 1, Balance: decimal.NewFromInt(500)},
		{AccountHolder: holder, CurrencyID: currencyID, ClassificationID: 2, Balance: decimal.NewFromInt(300)},
	}, nil
}

func (m *mockBalanceReader) BatchGetBalances(ctx context.Context, holderIDs []int64, currencyID int64) (map[int64][]core.Balance, error) {
	result := make(map[int64][]core.Balance)
	for _, id := range holderIDs {
		result[id] = []core.Balance{
			{AccountHolder: id, CurrencyID: currencyID, ClassificationID: 1, Balance: decimal.NewFromInt(100)},
		}
	}
	return result, nil
}

type mockReserver struct{}

func (m *mockReserver) Reserve(ctx context.Context, input core.ReserveInput) (*core.Reservation, error) {
	return &core.Reservation{ID: 1, AccountHolder: input.AccountHolder, CurrencyID: input.CurrencyID, ReservedAmount: input.Amount, Status: core.ReservationStatusActive, IdempotencyKey: input.IdempotencyKey, ExpiresAt: time.Now().Add(15 * time.Minute), CreatedAt: time.Now(), UpdatedAt: time.Now()}, nil
}

func (m *mockReserver) Settle(ctx context.Context, reservationID int64, actualAmount decimal.Decimal) error {
	return nil
}

func (m *mockReserver) Release(ctx context.Context, reservationID int64) error {
	return nil
}

type mockDepositor struct{}

func (m *mockDepositor) InitDeposit(ctx context.Context, input core.DepositInput) (*core.Deposit, error) {
	return &core.Deposit{ID: 1, AccountHolder: input.AccountHolder, CurrencyID: input.CurrencyID, ExpectedAmount: input.ExpectedAmount, Status: core.DepositStatusPending, ChannelName: input.ChannelName, IdempotencyKey: input.IdempotencyKey, CreatedAt: time.Now(), UpdatedAt: time.Now()}, nil
}

func (m *mockDepositor) ConfirmingDeposit(ctx context.Context, depositID int64, channelRef string) error {
	return nil
}

func (m *mockDepositor) ConfirmDeposit(ctx context.Context, input core.ConfirmDepositInput) error {
	return nil
}

func (m *mockDepositor) FailDeposit(ctx context.Context, depositID int64, reason string) error {
	return nil
}

func (m *mockDepositor) ExpireDeposit(ctx context.Context, depositID int64) error {
	return nil
}

type mockWithdrawer struct{}

func (m *mockWithdrawer) InitWithdraw(ctx context.Context, input core.WithdrawInput) (*core.Withdrawal, error) {
	return &core.Withdrawal{ID: 1, AccountHolder: input.AccountHolder, CurrencyID: input.CurrencyID, Amount: input.Amount, Status: core.WithdrawStatusLocked, ChannelName: input.ChannelName, IdempotencyKey: input.IdempotencyKey, ReviewRequired: input.ReviewRequired, CreatedAt: time.Now(), UpdatedAt: time.Now()}, nil
}

func (m *mockWithdrawer) ReserveWithdraw(ctx context.Context, withdrawalID int64) error    { return nil }
func (m *mockWithdrawer) ReviewWithdraw(ctx context.Context, withdrawalID int64, approved bool) error {
	return nil
}
func (m *mockWithdrawer) ProcessWithdraw(ctx context.Context, withdrawalID int64, channelRef string) error {
	return nil
}
func (m *mockWithdrawer) ConfirmWithdraw(ctx context.Context, withdrawalID int64) error  { return nil }
func (m *mockWithdrawer) FailWithdraw(ctx context.Context, withdrawalID int64, reason string) error {
	return nil
}
func (m *mockWithdrawer) RetryWithdraw(ctx context.Context, withdrawalID int64) error { return nil }

type mockClassificationStore struct{}

func (m *mockClassificationStore) CreateClassification(ctx context.Context, input core.ClassificationInput) (*core.Classification, error) {
	return &core.Classification{ID: 1, Code: input.Code, Name: input.Name, NormalSide: input.NormalSide, IsSystem: input.IsSystem, IsActive: true, CreatedAt: time.Now()}, nil
}

func (m *mockClassificationStore) DeactivateClassification(ctx context.Context, id int64) error {
	return nil
}

func (m *mockClassificationStore) ListClassifications(ctx context.Context, activeOnly bool) ([]core.Classification, error) {
	return []core.Classification{
		{ID: 1, Code: "ASSET", Name: "Asset", NormalSide: core.NormalSideDebit, IsActive: true},
		{ID: 2, Code: "LIABILITY", Name: "Liability", NormalSide: core.NormalSideCredit, IsActive: true},
	}, nil
}

type mockJournalTypeStore struct{}

func (m *mockJournalTypeStore) CreateJournalType(ctx context.Context, input core.JournalTypeInput) (*core.JournalType, error) {
	return &core.JournalType{ID: 1, Code: input.Code, Name: input.Name, IsActive: true, CreatedAt: time.Now()}, nil
}

func (m *mockJournalTypeStore) DeactivateJournalType(ctx context.Context, id int64) error {
	return nil
}

func (m *mockJournalTypeStore) ListJournalTypes(ctx context.Context, activeOnly bool) ([]core.JournalType, error) {
	return []core.JournalType{{ID: 1, Code: "DEPOSIT", Name: "Deposit", IsActive: true}}, nil
}

type mockTemplateStore struct{}

func (m *mockTemplateStore) CreateTemplate(ctx context.Context, input core.TemplateInput) (*core.EntryTemplate, error) {
	return &core.EntryTemplate{ID: 1, Code: input.Code, Name: input.Name, JournalTypeID: input.JournalTypeID, IsActive: true, CreatedAt: time.Now()}, nil
}

func (m *mockTemplateStore) DeactivateTemplate(ctx context.Context, id int64) error { return nil }

func (m *mockTemplateStore) GetTemplate(ctx context.Context, code string) (*core.EntryTemplate, error) {
	return &core.EntryTemplate{
		ID: 1, Code: code, Name: "Test", JournalTypeID: 1, IsActive: true,
		Lines: []core.EntryTemplateLine{
			{ID: 1, ClassificationID: 1, EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount"},
			{ID: 2, ClassificationID: 1, EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount"},
		},
	}, nil
}

func (m *mockTemplateStore) ListTemplates(ctx context.Context, activeOnly bool) ([]core.EntryTemplate, error) {
	return []core.EntryTemplate{{ID: 1, Code: "deposit", Name: "Deposit", JournalTypeID: 1, IsActive: true}}, nil
}

type mockCurrencyStore struct{}

func (m *mockCurrencyStore) CreateCurrency(ctx context.Context, input core.CurrencyInput) (*core.Currency, error) {
	return &core.Currency{ID: 1, Code: input.Code, Name: input.Name}, nil
}

func (m *mockCurrencyStore) ListCurrencies(ctx context.Context) ([]core.Currency, error) {
	return []core.Currency{{ID: 1, Code: "USDT", Name: "Tether"}}, nil
}

func (m *mockCurrencyStore) GetCurrency(ctx context.Context, id int64) (*core.Currency, error) {
	return &core.Currency{ID: id, Code: "USDT", Name: "Tether"}, nil
}

type mockReconciler struct{}

func (m *mockReconciler) CheckAccountingEquation(ctx context.Context) (*core.ReconcileResult, error) {
	return &core.ReconcileResult{Balanced: true, Gap: decimal.Zero, CheckedAt: time.Now()}, nil
}

func (m *mockReconciler) ReconcileAccount(ctx context.Context, holder int64, currencyID int64) (*core.ReconcileResult, error) {
	return &core.ReconcileResult{Balanced: true, Gap: decimal.Zero, CheckedAt: time.Now()}, nil
}

type mockSnapshotter struct{}

func (m *mockSnapshotter) CreateDailySnapshot(ctx context.Context, date time.Time) error { return nil }
func (m *mockSnapshotter) GetSnapshotBalance(ctx context.Context, holder int64, currencyID int64, date time.Time) ([]core.Balance, error) {
	return nil, nil
}

type mockQueryProvider struct{}

func (m *mockQueryProvider) GetJournal(ctx context.Context, id int64) (*core.Journal, []core.Entry, error) {
	j := &core.Journal{ID: id, JournalTypeID: 1, IdempotencyKey: "test", TotalDebit: decimal.NewFromInt(100), TotalCredit: decimal.NewFromInt(100), CreatedAt: time.Now()}
	entries := []core.Entry{
		{ID: 1, JournalID: id, AccountHolder: 100, CurrencyID: 1, ClassificationID: 1, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100), CreatedAt: time.Now()},
		{ID: 2, JournalID: id, AccountHolder: -100, CurrencyID: 1, ClassificationID: 1, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100), CreatedAt: time.Now()},
	}
	return j, entries, nil
}

func (m *mockQueryProvider) ListJournals(ctx context.Context, cursorID int64, limit int32) ([]core.Journal, error) {
	return []core.Journal{
		{ID: 1, JournalTypeID: 1, IdempotencyKey: "j1", TotalDebit: decimal.NewFromInt(100), TotalCredit: decimal.NewFromInt(100), CreatedAt: time.Now()},
	}, nil
}

func (m *mockQueryProvider) ListEntriesByAccount(ctx context.Context, holder, currencyID, cursorID int64, limit int32) ([]core.Entry, error) {
	return []core.Entry{
		{ID: 1, JournalID: 1, AccountHolder: holder, CurrencyID: currencyID, ClassificationID: 1, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100), CreatedAt: time.Now()},
	}, nil
}

func (m *mockQueryProvider) ListReservations(ctx context.Context, holder int64, status string, limit int32) ([]core.Reservation, error) {
	return []core.Reservation{}, nil
}

func (m *mockQueryProvider) ListDeposits(ctx context.Context, holder int64, limit int32) ([]core.Deposit, error) {
	return []core.Deposit{}, nil
}

func (m *mockQueryProvider) ListWithdrawals(ctx context.Context, holder int64, limit int32) ([]core.Withdrawal, error) {
	return []core.Withdrawal{}, nil
}

func (m *mockQueryProvider) ListSnapshotsByDateRange(ctx context.Context, holder, currencyID int64, start, end time.Time) ([]core.BalanceSnapshot, error) {
	return []core.BalanceSnapshot{}, nil
}

func (m *mockQueryProvider) GetSystemRollups(ctx context.Context) ([]server.SystemRollupBalance, error) {
	return []server.SystemRollupBalance{
		{CurrencyID: 1, ClassificationID: 1, TotalBalance: decimal.NewFromInt(10000), UpdatedAt: time.Now()},
	}, nil
}

// --- Test helper ---

func newTestServer() *server.Server {
	return server.New(
		&mockJournalWriter{},
		&mockBalanceReader{},
		&mockReserver{},
		&mockDepositor{},
		&mockWithdrawer{},
		&mockClassificationStore{},
		&mockJournalTypeStore{},
		&mockTemplateStore{},
		&mockCurrencyStore{},
		&mockReconciler{},
		&mockSnapshotter{},
		(*service.SystemRollupService)(nil), // not used directly
		&mockQueryProvider{},
	)
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

// --- Tests ---

func TestHealthEndpoint(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/system/health", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "ok", resp["status"])
}

func TestPostJournal(t *testing.T) {
	srv := newTestServer()
	body := map[string]any{
		"journal_type_id":  1,
		"idempotency_key":  "test-123",
		"entries": []map[string]any{
			{"account_holder": 100, "currency_id": 1, "classification_id": 1, "entry_type": "debit", "amount": "100"},
			{"account_holder": -100, "currency_id": 1, "classification_id": 1, "entry_type": "credit", "amount": "100"},
		},
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/journals", body)
	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestPostJournalUnbalanced(t *testing.T) {
	srv := server.New(
		&mockJournalWriter{
			postFn: func(ctx context.Context, input core.JournalInput) (*core.Journal, error) {
				return nil, fmt.Errorf("core: journal: unbalanced — debit=100 credit=50")
			},
		},
		&mockBalanceReader{}, &mockReserver{}, &mockDepositor{}, &mockWithdrawer{},
		&mockClassificationStore{}, &mockJournalTypeStore{}, &mockTemplateStore{}, &mockCurrencyStore{},
		&mockReconciler{}, &mockSnapshotter{}, nil, &mockQueryProvider{},
	)
	body := map[string]any{
		"journal_type_id":  1,
		"idempotency_key":  "test-unbalanced",
		"entries": []map[string]any{
			{"account_holder": 100, "currency_id": 1, "classification_id": 1, "entry_type": "debit", "amount": "100"},
			{"account_holder": -100, "currency_id": 1, "classification_id": 1, "entry_type": "credit", "amount": "50"},
		},
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/journals", body)
	assert.Equal(t, http.StatusBadRequest, w.Code)
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

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	entries, ok := resp["entries"].([]any)
	require.True(t, ok)
	assert.Len(t, entries, 2)
}

func TestListJournalsWithCursor(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/journals?limit=10", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	data, ok := resp["data"].([]any)
	require.True(t, ok)
	assert.Len(t, data, 1)
}

func TestGetBalances(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/balances/100?currency_id=1", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	data, ok := resp["data"].([]any)
	require.True(t, ok)
	assert.Len(t, data, 2)
}

func TestBatchBalances(t *testing.T) {
	srv := newTestServer()
	body := map[string]any{"holder_ids": []int64{100, 200}, "currency_id": 1}
	w := doRequest(srv, http.MethodPost, "/api/v1/balances/batch", body)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestCreateReservation(t *testing.T) {
	srv := newTestServer()
	body := map[string]any{
		"account_holder":  100,
		"currency_id":     1,
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

func TestDepositLifecycle(t *testing.T) {
	srv := newTestServer()

	// Init
	body := map[string]any{
		"account_holder":  100,
		"currency_id":     1,
		"expected_amount": "500.00",
		"channel_name":    "crypto",
		"idempotency_key": "dep-1",
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/deposits", body)
	assert.Equal(t, http.StatusCreated, w.Code)

	// Confirming
	w = doRequest(srv, http.MethodPost, "/api/v1/deposits/1/confirming", map[string]any{"channel_ref": "tx-abc"})
	assert.Equal(t, http.StatusOK, w.Code)

	// Confirm
	w = doRequest(srv, http.MethodPost, "/api/v1/deposits/1/confirm", map[string]any{"actual_amount": "500.00", "channel_ref": "tx-abc"})
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestWithdrawalLifecycle(t *testing.T) {
	srv := newTestServer()

	// Init
	body := map[string]any{
		"account_holder":  100,
		"currency_id":     1,
		"amount":          "200.00",
		"channel_name":    "crypto",
		"idempotency_key": "wd-1",
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/withdrawals", body)
	assert.Equal(t, http.StatusCreated, w.Code)

	// Reserve
	w = doRequest(srv, http.MethodPost, "/api/v1/withdrawals/1/reserve", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	// Process
	w = doRequest(srv, http.MethodPost, "/api/v1/withdrawals/1/process", map[string]any{"channel_ref": "tx-xyz"})
	assert.Equal(t, http.StatusOK, w.Code)

	// Confirm
	w = doRequest(srv, http.MethodPost, "/api/v1/withdrawals/1/confirm", nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestWithdrawalWithReview(t *testing.T) {
	srv := newTestServer()

	body := map[string]any{
		"account_holder":  100,
		"currency_id":     1,
		"amount":          "1000.00",
		"channel_name":    "crypto",
		"idempotency_key": "wd-review",
		"review_required": true,
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/withdrawals", body)
	assert.Equal(t, http.StatusCreated, w.Code)

	// Review approve
	w = doRequest(srv, http.MethodPost, "/api/v1/withdrawals/1/review", map[string]any{"approved": true})
	assert.Equal(t, http.StatusOK, w.Code)
}

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
		"code":            "deposit",
		"name":            "Deposit",
		"journal_type_id": 1,
		"lines": []map[string]any{
			{"classification_id": 1, "entry_type": "debit", "holder_role": "user", "amount_key": "amount", "sort_order": 1},
			{"classification_id": 1, "entry_type": "credit", "holder_role": "system", "amount_key": "amount", "sort_order": 2},
		},
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/templates", body)
	assert.Equal(t, http.StatusCreated, w.Code)

	w = doRequest(srv, http.MethodGet, "/api/v1/templates?active_only=true", nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestTemplatePreview(t *testing.T) {
	srv := newTestServer()

	body := map[string]any{
		"holder_id":   100,
		"currency_id": 1,
		"amounts":     map[string]string{"amount": "500"},
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/templates/deposit/preview", body)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	entries, ok := resp["entries"].([]any)
	require.True(t, ok)
	assert.Len(t, entries, 2)
}

func TestCurrencyCRUD(t *testing.T) {
	srv := newTestServer()

	body := map[string]any{"code": "USDC", "name": "USD Coin"}
	w := doRequest(srv, http.MethodPost, "/api/v1/currencies", body)
	assert.Equal(t, http.StatusCreated, w.Code)

	w = doRequest(srv, http.MethodGet, "/api/v1/currencies", nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestReconcileGlobal(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodPost, "/api/v1/reconcile", nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestReconcileAccount(t *testing.T) {
	srv := newTestServer()
	body := map[string]any{"holder": 100, "currency_id": 1}
	w := doRequest(srv, http.MethodPost, "/api/v1/reconcile/account", body)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestSystemBalances(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/system/balances", nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestListSnapshots(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/snapshots?holder=100&currency_id=1&start=2026-01-01&end=2026-12-31", nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestListEntriesByAccount(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/entries?holder=100&currency_id=1", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	data, ok := resp["data"].([]any)
	require.True(t, ok)
	assert.Len(t, data, 1)
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
	w := doRequest(srv, http.MethodGet, "/api/v1/balances/abc?currency_id=1", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// Missing currency_id on balances
	w = doRequest(srv, http.MethodGet, "/api/v1/balances/100", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// Empty batch
	w = doRequest(srv, http.MethodPost, "/api/v1/balances/batch", map[string]any{"holder_ids": []int64{}, "currency_id": 1})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
