package ledger_test

import (
	"context"
	"sync"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
)

// storeMetricsRecorder pins I-M1: every postgres store write path
// (LedgerStore, ReserverStore, BookingStore) must reach the core.Metrics
// this Service was constructed with. Embeds core.NoopMetrics by value so
// every unrelated method is a safe no-op.
type storeMetricsRecorder struct {
	core.NoopMetrics
	mu                   sync.Mutex
	journalPosted        []string
	journalFailed        []string
	journalFailedReasons []string
	idempotencyCollision []string
	templateFailed       []string
	reserveCreated       int
	reserveSettled       int
	reserveReleased      int
}

func (m *storeMetricsRecorder) JournalPosted(code string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.journalPosted = append(m.journalPosted, code)
}

func (m *storeMetricsRecorder) JournalFailed(code, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.journalFailed = append(m.journalFailed, code)
	m.journalFailedReasons = append(m.journalFailedReasons, reason)
}

func (m *storeMetricsRecorder) IdempotencyCollision(code string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.idempotencyCollision = append(m.idempotencyCollision, code)
}

func (m *storeMetricsRecorder) TemplateFailed(code, _ string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.templateFailed = append(m.templateFailed, code)
}

func (m *storeMetricsRecorder) ReserveCreated() { m.mu.Lock(); defer m.mu.Unlock(); m.reserveCreated++ }
func (m *storeMetricsRecorder) ReserveSettled() { m.mu.Lock(); defer m.mu.Unlock(); m.reserveSettled++ }
func (m *storeMetricsRecorder) ReserveReleased() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reserveReleased++
}

// TestLedgerNew_WiresMetricsIntoLedgerStore pins I-M1 for the journal path:
// PostJournal's success and failure exits, and ensureJournalMatchesInput's
// idempotency-collision detection, all reach the core.Metrics passed to
// ledger.New -- before this fix, postgres/ had no core.Metrics dependency
// at all, so every one of these was silently unobserved regardless of what
// the consumer wired in.
func TestLedgerNew_WiresMetricsIntoLedgerStore(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	metrics := &storeMetricsRecorder{}
	svc, err := ledger.New(pool, ledger.WithMetrics(metrics))
	require.NoError(t, err)

	suffix := postgrestest.UniqueKey("im1")
	cur, err := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{Code: "IM1_" + suffix, Name: "I-M1 Unit", Exponent: 18})
	require.NoError(t, err)
	wallet, err := svc.Classifications().CreateClassification(ctx, core.ClassificationInput{
		Code: "im1_wallet_" + suffix, Name: "I-M1 Wallet", NormalSide: core.NormalSideDebit, BalanceRole: core.BalanceRoleAvailable,
	})
	require.NoError(t, err)
	vault, err := svc.Classifications().CreateClassification(ctx, core.ClassificationInput{
		Code: "im1_vault_" + suffix, Name: "I-M1 Vault", NormalSide: core.NormalSideCredit, IsSystem: true,
	})
	require.NoError(t, err)
	jt, err := svc.JournalTypes().CreateJournalType(ctx, core.JournalTypeInput{Code: "im1_fund_" + suffix, Name: "I-M1 Fund"})
	require.NoError(t, err)

	const holder = int64(9601)
	key := postgrestest.UniqueKey("im1-post")
	input := core.JournalInput{
		JournalTypeUID: jt.UID,
		IdempotencyKey: key,
		Source:         "im1-test",
		ActorID:        holder,
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: cur.UID, ClassificationUID: wallet.UID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(10)},
			{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: cur.UID, ClassificationUID: vault.UID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(10)},
		},
	}

	// --- success: JournalPosted ---
	_, err = svc.JournalWriter().PostJournal(ctx, input)
	require.NoError(t, err)
	metrics.mu.Lock()
	assert.Contains(t, metrics.journalPosted, jt.Code, "JournalPosted must fire with the journal type CODE (not uid) on success")
	metrics.mu.Unlock()

	// --- idempotency collision: same key, different payload -> IdempotencyCollision + JournalFailed(conflict-shaped) ---
	divergent := input
	divergent.Entries = []core.EntryInput{
		{AccountHolder: holder, CurrencyUID: cur.UID, ClassificationUID: wallet.UID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(999)},
		{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: cur.UID, ClassificationUID: vault.UID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(999)},
	}
	_, err = svc.JournalWriter().PostJournal(ctx, divergent)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrConflict)
	metrics.mu.Lock()
	assert.NotEmpty(t, metrics.idempotencyCollision, "IdempotencyCollision must fire when the same key resolves to a divergent payload")
	metrics.mu.Unlock()

	// --- failure: unbalanced journal -> JournalFailed ---
	badInput := input
	badInput.IdempotencyKey = postgrestest.UniqueKey("im1-bad")
	badInput.Entries = []core.EntryInput{
		{AccountHolder: holder, CurrencyUID: cur.UID, ClassificationUID: wallet.UID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(10)},
		{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: cur.UID, ClassificationUID: vault.UID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(5)},
	}
	_, err = svc.JournalWriter().PostJournal(ctx, badInput)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrUnbalancedJournal, "core.JournalInput.Validate rejects a per-currency imbalance before it ever reaches postJournalWithQueries")
	metrics.mu.Lock()
	// journalTypeCode is "" here by design: Validate() runs (and rejects
	// this input) in PostJournal's OUTER function, before the journal type
	// is ever resolved -- see PostJournal's own JournalFailed call site.
	assert.Contains(t, metrics.journalFailedReasons, "unbalanced", "JournalFailed must fire (reason=unbalanced) even for a Validate()-level rejection, not only a DB-level one")
	metrics.mu.Unlock()
}

// TestLedgerNew_WiresMetricsIntoReserverStore pins I-M1 for Reserve/Settle/
// Release's success exits.
func TestLedgerNew_WiresMetricsIntoReserverStore(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	metrics := &storeMetricsRecorder{}
	svc, err := ledger.New(pool, ledger.WithMetrics(metrics))
	require.NoError(t, err)

	suffix := postgrestest.UniqueKey("im1r")
	cur, err := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{Code: "IM1R_" + suffix, Name: "I-M1 Reserve Unit", Exponent: 18})
	require.NoError(t, err)
	wallet, err := svc.Classifications().CreateClassification(ctx, core.ClassificationInput{
		Code: "im1r_wallet_" + suffix, Name: "I-M1 Reserve Wallet", NormalSide: core.NormalSideDebit, BalanceRole: core.BalanceRoleAvailable,
	})
	require.NoError(t, err)
	vault, err := svc.Classifications().CreateClassification(ctx, core.ClassificationInput{
		Code: "im1r_vault_" + suffix, Name: "I-M1 Reserve Vault", NormalSide: core.NormalSideCredit, IsSystem: true,
	})
	require.NoError(t, err)
	jt, err := svc.JournalTypes().CreateJournalType(ctx, core.JournalTypeInput{Code: "im1r_fund_" + suffix, Name: "I-M1 Reserve Fund"})
	require.NoError(t, err)
	const holder = int64(9602)

	// Fund the holder before reserving against it -- Reserve checks
	// available balance, and this fixture starts from zero.
	_, err = svc.JournalWriter().PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jt.UID,
		IdempotencyKey: postgrestest.UniqueKey("im1r-fund"),
		Source:         "im1r-test",
		ActorID:        holder,
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: cur.UID, ClassificationUID: wallet.UID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: cur.UID, ClassificationUID: vault.UID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	})
	require.NoError(t, err)

	res, err := svc.Reserver().Reserve(ctx, core.ReserveInput{
		AccountHolder:  holder,
		CurrencyUID:    cur.UID,
		Amount:         decimal.NewFromInt(5),
		IdempotencyKey: postgrestest.UniqueKey("im1-reserve"),
	})
	require.NoError(t, err)
	metrics.mu.Lock()
	assert.Equal(t, 1, metrics.reserveCreated)
	metrics.mu.Unlock()

	require.NoError(t, svc.Reserver().Settle(ctx, core.SettleInput{
		ReservationUID: res.UID,
		Amount:         decimal.NewFromInt(5),
		IdempotencyKey: postgrestest.UniqueKey("im1-settle"),
	}))
	metrics.mu.Lock()
	assert.Equal(t, 1, metrics.reserveSettled)
	metrics.mu.Unlock()

	res2, err := svc.Reserver().Reserve(ctx, core.ReserveInput{
		AccountHolder:  holder,
		CurrencyUID:    cur.UID,
		Amount:         decimal.NewFromInt(3),
		IdempotencyKey: postgrestest.UniqueKey("im1-reserve2"),
	})
	require.NoError(t, err)
	require.NoError(t, svc.Reserver().Release(ctx, core.ReleaseInput{
		ReservationUID: res2.UID,
		IdempotencyKey: postgrestest.UniqueKey("im1-release"),
	}))
	metrics.mu.Lock()
	assert.Equal(t, 1, metrics.reserveReleased)
	metrics.mu.Unlock()
}

// TestLedgerNew_WiresMetricsIntoTemplateFailed pins TemplateFailed: a
// template render failure (unknown code) must be attributed by the CALLER's
// template code, distinct from JournalFailed's journal-type-code label.
func TestLedgerNew_WiresMetricsIntoTemplateFailed(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	metrics := &storeMetricsRecorder{}
	svc, err := ledger.New(pool, ledger.WithMetrics(metrics))
	require.NoError(t, err)

	_, err = svc.JournalWriter().ExecuteTemplate(ctx, "no_such_template", core.TemplateParams{})
	require.Error(t, err)
	metrics.mu.Lock()
	assert.Contains(t, metrics.templateFailed, "no_such_template")
	metrics.mu.Unlock()
}
