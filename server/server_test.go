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
	postFn     func(ctx context.Context, input core.JournalInput) (*core.Journal, error)
	templateFn func(ctx context.Context, code string, params core.TemplateParams) (*core.Journal, error)
	reverseFn  func(ctx context.Context, journalID int64, reason string) (*core.Journal, error)
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
	return &core.Journal{ID: 3, JournalTypeID: 1, IdempotencyKey: fmt.Sprintf("reversal:%d:%s", journalID, reason), ReversalOf: journalID, TotalDebit: decimal.NewFromInt(100), TotalCredit: decimal.NewFromInt(100), CreatedAt: time.Now()}, nil
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

type mockReserver struct {
	reserveFn func(ctx context.Context, input core.ReserveInput) (*core.Reservation, error)
	settleFn  func(ctx context.Context, reservationID int64, actualAmount decimal.Decimal) error
	releaseFn func(ctx context.Context, reservationID int64) error
}

func (m *mockReserver) Reserve(ctx context.Context, input core.ReserveInput) (*core.Reservation, error) {
	if m.reserveFn != nil {
		return m.reserveFn(ctx, input)
	}
	return &core.Reservation{ID: 1, AccountHolder: input.AccountHolder, CurrencyID: input.CurrencyID, ReservedAmount: input.Amount, Status: core.ReservationStatusActive, IdempotencyKey: input.IdempotencyKey, ExpiresAt: time.Now().Add(15 * time.Minute), CreatedAt: time.Now(), UpdatedAt: time.Now()}, nil
}

func (m *mockReserver) Settle(ctx context.Context, reservationID int64, actualAmount decimal.Decimal) error {
	if m.settleFn != nil {
		return m.settleFn(ctx, reservationID, actualAmount)
	}
	return nil
}

func (m *mockReserver) Release(ctx context.Context, reservationID int64) error {
	if m.releaseFn != nil {
		return m.releaseFn(ctx, reservationID)
	}
	return nil
}

type mockOperator struct {
	createFn     func(ctx context.Context, input core.CreateOperationInput) (*core.Operation, error)
	transitionFn func(ctx context.Context, input core.TransitionInput) (*core.Event, error)
}

func (m *mockOperator) CreateOperation(ctx context.Context, input core.CreateOperationInput) (*core.Operation, error) {
	if m.createFn != nil {
		return m.createFn(ctx, input)
	}
	return &core.Operation{
		ID: 1, ClassificationID: 1, AccountHolder: input.AccountHolder,
		CurrencyID: input.CurrencyID, Amount: input.Amount, Status: "pending",
		ChannelName: input.ChannelName, IdempotencyKey: input.IdempotencyKey,
		Metadata: input.Metadata, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}, nil
}

func (m *mockOperator) Transition(ctx context.Context, input core.TransitionInput) (*core.Event, error) {
	if m.transitionFn != nil {
		return m.transitionFn(ctx, input)
	}
	return &core.Event{
		ID: 1, ClassificationCode: "deposit", OperationID: input.OperationID,
		AccountHolder: 100, CurrencyID: 1,
		FromStatus: "pending", ToStatus: input.ToStatus,
		Amount: input.Amount, OccurredAt: time.Now(),
	}, nil
}

type mockOperationReader struct {
	getFn  func(ctx context.Context, id int64) (*core.Operation, error)
	listFn func(ctx context.Context, filter core.OperationFilter) ([]core.Operation, error)
}

func (m *mockOperationReader) GetOperation(ctx context.Context, id int64) (*core.Operation, error) {
	if m.getFn != nil {
		return m.getFn(ctx, id)
	}
	return &core.Operation{
		ID: id, ClassificationID: 1, AccountHolder: 100,
		CurrencyID: 1, Amount: decimal.NewFromInt(500), Status: "pending",
		ChannelName: "crypto", IdempotencyKey: "op-1",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}, nil
}

func (m *mockOperationReader) ListOperations(ctx context.Context, filter core.OperationFilter) ([]core.Operation, error) {
	if m.listFn != nil {
		return m.listFn(ctx, filter)
	}
	return []core.Operation{
		{ID: 1, ClassificationID: 1, AccountHolder: 100, CurrencyID: 1, Amount: decimal.NewFromInt(500), Status: "pending", CreatedAt: time.Now(), UpdatedAt: time.Now()},
	}, nil
}

type mockEventReader struct {
	getFn  func(ctx context.Context, id int64) (*core.Event, error)
	listFn func(ctx context.Context, filter core.EventFilter) ([]core.Event, error)
}

func (m *mockEventReader) GetEvent(ctx context.Context, id int64) (*core.Event, error) {
	if m.getFn != nil {
		return m.getFn(ctx, id)
	}
	return &core.Event{
		ID: id, ClassificationCode: "deposit", OperationID: 1,
		AccountHolder: 100, CurrencyID: 1,
		FromStatus: "pending", ToStatus: "confirmed",
		Amount: decimal.NewFromInt(500), OccurredAt: time.Now(),
	}, nil
}

func (m *mockEventReader) ListEvents(ctx context.Context, filter core.EventFilter) ([]core.Event, error) {
	if m.listFn != nil {
		return m.listFn(ctx, filter)
	}
	return []core.Event{
		{ID: 1, ClassificationCode: "deposit", OperationID: 1, AccountHolder: 100, CurrencyID: 1, FromStatus: "pending", ToStatus: "confirmed", Amount: decimal.NewFromInt(500), OccurredAt: time.Now()},
	}, nil
}

type mockClassificationStore struct{}

func (m *mockClassificationStore) CreateClassification(ctx context.Context, input core.ClassificationInput) (*core.Classification, error) {
	return &core.Classification{ID: 1, Code: input.Code, Name: input.Name, NormalSide: input.NormalSide, IsSystem: input.IsSystem, IsActive: true, CreatedAt: time.Now()}, nil
}

func (m *mockClassificationStore) GetByCode(ctx context.Context, code string) (*core.Classification, error) {
	return &core.Classification{ID: 1, Code: code, Name: code, NormalSide: core.NormalSideDebit, IsActive: true, CreatedAt: time.Now()}, nil
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

func (m *mockQueryProvider) ListDeposits(ctx context.Context, holder int64, status string, limit int32) ([]core.Deposit, error) {
	return []core.Deposit{}, nil
}

func (m *mockQueryProvider) ListWithdrawals(ctx context.Context, holder int64, status string, limit int32) ([]core.Withdrawal, error) {
	return []core.Withdrawal{}, nil
}

func (m *mockQueryProvider) ListSnapshotsByDateRange(ctx context.Context, holder, currencyID int64, start, end time.Time) ([]core.BalanceSnapshot, error) {
	return []core.BalanceSnapshot{}, nil
}

func (m *mockQueryProvider) GetSystemRollups(ctx context.Context) ([]core.SystemRollup, error) {
	return []core.SystemRollup{
		{CurrencyID: 1, ClassificationID: 1, TotalBalance: decimal.NewFromInt(10000), UpdatedAt: time.Now()},
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
		&mockOperator{},
		&mockOperationReader{},
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
	)
}

// newTestServerWith creates a test server with custom overrides.
func newTestServerWith(opts ...func(*testServerOpts)) *server.Server {
	o := &testServerOpts{
		journals:        &mockJournalWriter{},
		balances:        &mockBalanceReader{},
		reserver:        &mockReserver{},
		operator:        &mockOperator{},
		operationReader: &mockOperationReader{},
		eventReader:     &mockEventReader{},
		classifications: &mockClassificationStore{},
		journalTypes:    &mockJournalTypeStore{},
		templates:       &mockTemplateStore{},
		currencies:      &mockCurrencyStore{},
		reconciler:      &mockReconciler{},
		snapshotter:     &mockSnapshotter{},
		queries:         &mockQueryProvider{},
	}
	for _, fn := range opts {
		fn(o)
	}
	return server.New(
		o.journals, o.balances, o.reserver,
		o.operator, o.operationReader, o.eventReader,
		o.classifications, o.journalTypes, o.templates, o.currencies,
		o.channels,
		o.reconciler, o.snapshotter, nil, o.queries,
	)
}

type testServerOpts struct {
	journals        core.JournalWriter
	balances        core.BalanceReader
	reserver        core.Reserver
	operator        core.Operator
	operationReader core.OperationReader
	eventReader     core.EventReader
	classifications core.ClassificationStore
	journalTypes    core.JournalTypeStore
	templates       core.TemplateStore
	currencies      core.CurrencyStore
	channels        map[string]channel.Adapter
	reconciler      core.Reconciler
	snapshotter     core.Snapshotter
	queries         core.QueryProvider
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

// parseEnvelopeArray extracts the "data" field as an array from the envelope.
func parseEnvelopeArray(t *testing.T, body []byte) []any {
	t.Helper()
	var env map[string]any
	require.NoError(t, json.Unmarshal(body, &env))
	data, ok := env["data"].([]any)
	require.True(t, ok, "expected 'data' array in envelope, got: %v", env)
	return data
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
	srv := newTestServerWith(func(o *testServerOpts) {
		o.journals = &mockJournalWriter{
			postFn: func(ctx context.Context, input core.JournalInput) (*core.Journal, error) {
				return nil, fmt.Errorf("core: journal: unbalanced — debit=100 credit=50: %w", core.ErrUnbalancedJournal)
			},
		}
	})
	body := map[string]any{
		"journal_type_id":  1,
		"idempotency_key":  "test-unbalanced",
		"entries": []map[string]any{
			{"account_holder": 100, "currency_id": 1, "classification_id": 1, "entry_type": "debit", "amount": "100"},
			{"account_holder": -100, "currency_id": 1, "classification_id": 1, "entry_type": "credit", "amount": "50"},
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
	journals, ok := data["data"].([]any)
	require.True(t, ok)
	assert.Len(t, journals, 1)
}

func TestGetBalances(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/balances/100?currency_id=1", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	data := parseEnvelopeArray(t, w.Body.Bytes())
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

// --- Operation lifecycle tests ---

func TestCreateOperation(t *testing.T) {
	srv := newTestServer()
	body := map[string]any{
		"classification_code": "deposit",
		"account_holder":      100,
		"currency_id":         1,
		"amount":              "500.00",
		"channel_name":        "crypto",
		"idempotency_key":     "op-1",
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/operations", body)
	assert.Equal(t, http.StatusCreated, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	assert.Equal(t, float64(1), data["id"])
	assert.Equal(t, "pending", data["status"])
}

func TestTransitionOperation(t *testing.T) {
	srv := newTestServer()
	body := map[string]any{
		"to_status":   "confirmed",
		"channel_ref": "tx-abc",
		"amount":      "500.00",
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/operations/1/transition", body)
	assert.Equal(t, http.StatusOK, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	assert.Equal(t, "confirmed", data["to_status"])
}

func TestGetOperation(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/operations/1", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	assert.Equal(t, float64(1), data["id"])
}

func TestListOperations(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/operations?holder=100&status=pending", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	ops, ok := data["data"].([]any)
	require.True(t, ok)
	assert.Len(t, ops, 1)
}

// --- Event tests ---

func TestGetEvent(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/events/1", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	assert.Equal(t, float64(1), data["id"])
	assert.Equal(t, "confirmed", data["to_status"])
}

func TestListEvents(t *testing.T) {
	srv := newTestServer()
	w := doRequest(srv, http.MethodGet, "/api/v1/events?operation_id=1", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	data := parseEnvelope(t, w.Body.Bytes())
	events, ok := data["data"].([]any)
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

	data := parseEnvelope(t, w.Body.Bytes())
	entries, ok := data["entries"].([]any)
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

	data := parseEnvelope(t, w.Body.Bytes())
	entries, ok := data["data"].([]any)
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
	w := doRequest(srv, http.MethodGet, "/api/v1/balances/abc?currency_id=1", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// Missing currency_id on balances
	w = doRequest(srv, http.MethodGet, "/api/v1/balances/100", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// Empty batch
	w = doRequest(srv, http.MethodPost, "/api/v1/balances/batch", map[string]any{"holder_ids": []int64{}, "currency_id": 1})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- Error path tests ---

func TestPostJournal_NotFound(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.journals = &mockJournalWriter{
			reverseFn: func(ctx context.Context, journalID int64, reason string) (*core.Journal, error) {
				return nil, fmt.Errorf("postgres: reverse journal: journal %d: %w", journalID, core.ErrNotFound)
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
		"journal_type_id": 1,
		"idempotency_key": "test-invalid-input",
		"entries": []map[string]any{
			{"account_holder": 100, "currency_id": 1, "classification_id": 1, "entry_type": "debit", "amount": "100"},
			{"account_holder": -100, "currency_id": 1, "classification_id": 1, "entry_type": "credit", "amount": "100"},
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
		"journal_type_id": 1,
		"idempotency_key": "test-duplicate",
		"entries": []map[string]any{
			{"account_holder": 100, "currency_id": 1, "classification_id": 1, "entry_type": "debit", "amount": "100"},
			{"account_holder": -100, "currency_id": 1, "classification_id": 1, "entry_type": "credit", "amount": "100"},
		},
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/journals", body)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
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
		"journal_type_id": 1,
		"idempotency_key": "test-internal",
		"entries": []map[string]any{
			{"account_holder": 100, "currency_id": 1, "classification_id": 1, "entry_type": "debit", "amount": "100"},
			{"account_holder": -100, "currency_id": 1, "classification_id": 1, "entry_type": "credit", "amount": "100"},
		},
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/journals", body)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestReverseJournal_NotFound(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.journals = &mockJournalWriter{
			reverseFn: func(ctx context.Context, journalID int64, reason string) (*core.Journal, error) {
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
			settleFn: func(ctx context.Context, reservationID int64, actualAmount decimal.Decimal) error {
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
			settleFn: func(ctx context.Context, reservationID int64, actualAmount decimal.Decimal) error {
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
		"currency_id":     1,
		"amount":          "50.00",
		"idempotency_key": "res-invalid",
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/reservations", body)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- Operation error path tests ---

func TestCreateOperation_NotFound(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.operator = &mockOperator{
			createFn: func(ctx context.Context, input core.CreateOperationInput) (*core.Operation, error) {
				return nil, fmt.Errorf("service: create operation: classification not found: %w", core.ErrNotFound)
			},
		}
	})
	body := map[string]any{
		"classification_code": "unknown",
		"account_holder":      100,
		"currency_id":         1,
		"amount":              "500.00",
		"idempotency_key":     "op-notfound",
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/operations", body)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestTransition_InvalidTransition(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.operator = &mockOperator{
			transitionFn: func(ctx context.Context, input core.TransitionInput) (*core.Event, error) {
				return nil, fmt.Errorf("service: transition: %w", core.ErrInvalidTransition)
			},
		}
	})
	body := map[string]any{
		"to_status": "confirmed",
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/operations/1/transition", body)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
}

func TestGetOperation_NotFound(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.operationReader = &mockOperationReader{
			getFn: func(ctx context.Context, id int64) (*core.Operation, error) {
				return nil, fmt.Errorf("postgres: get operation: %w", core.ErrNotFound)
			},
		}
	})
	w := doRequest(srv, http.MethodGet, "/api/v1/operations/999", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGetEvent_NotFound(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.eventReader = &mockEventReader{
			getFn: func(ctx context.Context, id int64) (*core.Event, error) {
				return nil, fmt.Errorf("postgres: get event: %w", core.ErrNotFound)
			},
		}
	})
	w := doRequest(srv, http.MethodGet, "/api/v1/events/999", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestCreateOperation_InsufficientBalance(t *testing.T) {
	srv := newTestServerWith(func(o *testServerOpts) {
		o.operator = &mockOperator{
			createFn: func(ctx context.Context, input core.CreateOperationInput) (*core.Operation, error) {
				return nil, fmt.Errorf("service: create operation: %w", core.ErrInsufficientBalance)
			},
		}
	})
	body := map[string]any{
		"classification_code": "withdrawal",
		"account_holder":      100,
		"currency_id":         1,
		"amount":              "99999.00",
		"idempotency_key":     "op-insufficient",
	}
	w := doRequest(srv, http.MethodPost, "/api/v1/operations", body)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
}
