package service_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/presets"
	"github.com/azex-ai/ledger/service"
)

// Real CREATE2 fingerprint (see core/create2_test.go's ground-truth vectors)
// -- any valid 20/32-byte hex pair works here, only used for deterministic
// address derivation, not verified against a real deployment in these tests.
const (
	itFactory  = "0x6CE5E7A510C693E1E4FC032d8De0c394C9C1A323"
	itInitHash = "0x2ef28d391fa40901fc8c61168ece13f5247e49e87925cd7f617262b9231b9ece"
)

// testTxComposer implements service.TxComposer by hand-rolling the same
// begin/bind/commit sequence (*ledger.Service).RunInTx performs -- service/'s
// own tests cannot reach into the root `ledger` package's Service type
// without contriving a dependency (ledger.go imports service; see
// onchain.go's TxComposer doc comment for why the port exists at all), so
// this mirrors it directly against the two postgres stores under test.
type testTxComposer struct {
	pool         *pgxpool.Pool
	bookingStore *postgres.BookingStore
	ledgerStore  *postgres.LedgerStore
}

func (c *testTxComposer) RunInTx(ctx context.Context, fn func(ctx context.Context, booker core.Booker, journals core.JournalWriter) error) error {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if err := fn(ctx, c.bookingStore.WithDB(tx), c.ledgerStore.WithDB(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

// --- fakes for the chain-facing ports (per task instructions: hand-written
// fakes for ChainReader/ChainScanner/Sweeper, real Postgres for everything
// else -- no mocked DB) ---

type fakeChainReader struct {
	mu       sync.Mutex
	included map[string]bool // key: "chainID:txHash"
}

func newFakeChainReader() *fakeChainReader {
	return &fakeChainReader{included: make(map[string]bool)}
}

func (f *fakeChainReader) LatestBlock(ctx context.Context, chainID int64) (int64, error) {
	return 0, nil // not exercised: these tests drive IngestDeposit/sweep directly, not the watcher loop
}

func (f *fakeChainReader) FetchDeposits(ctx context.Context, chainID, fromBlock, toBlock int64, addresses []string) ([]core.DepositSighting, error) {
	return nil, nil
}

func (f *fakeChainReader) TxIncluded(ctx context.Context, chainID int64, txHash string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.included[chainTxKey(chainID, txHash)], nil
}

func (f *fakeChainReader) setIncluded(chainID int64, txHash string, included bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.included[chainTxKey(chainID, txHash)] = included
}

func chainTxKey(chainID int64, txHash string) string {
	return decimal.NewFromInt(chainID).String() + ":" + txHash
}

type fakeChainScanner struct {
	balances map[string]decimal.Decimal
}

func (f *fakeChainScanner) ScanBalances(ctx context.Context, chainID int64, token string, addresses []string) (map[string]decimal.Decimal, error) {
	out := make(map[string]decimal.Decimal)
	for _, a := range addresses {
		if b, ok := f.balances[a]; ok {
			out[a] = b
		}
	}
	return out, nil
}

type fakeSweeper struct {
	mu            sync.Mutex
	nonceSeq      uint64
	nextNonceCall int
	gasPrice      decimal.Decimal
	batchSweeps   []fakeBatchSweepCall
	txHashSeq     int
}

type fakeBatchSweepCall struct {
	chainID int64
	token   string
	targets []core.SweepTarget
	nonce   uint64
}

func (f *fakeSweeper) NextNonce(ctx context.Context, chainID int64) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextNonceCall++
	n := f.nonceSeq
	f.nonceSeq++
	return n, nil
}

func (f *fakeSweeper) BatchSweep(ctx context.Context, chainID int64, token string, targets []core.SweepTarget, nonce uint64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txHashSeq++
	f.batchSweeps = append(f.batchSweeps, fakeBatchSweepCall{chainID, token, targets, nonce})
	return "0xsweep" + decimal.NewFromInt(int64(f.txHashSeq)).String(), nil
}

func (f *fakeSweeper) GasPrice(ctx context.Context, chainID int64) (decimal.Decimal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gasPrice, nil
}

// --- test harness ---

type onchainHarness struct {
	svc      *service.Onchain
	classes  *postgres.ClassificationStore
	bookings *postgres.BookingStore

	reader      *fakeChainReader
	scanner     *fakeChainScanner
	sweeper     *fakeSweeper
	deadLetters *postgres.IngestDeadLetterStore
}

// setupOnchain wires a service.Onchain against a fresh testcontainers
// Postgres instance, seeding one currency per code in currencyCodes.
func setupOnchain(t *testing.T, chains core.ChainSet, currencyCodes []string, opts ...service.OnchainOption) *onchainHarness {
	t.Helper()
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	classStore := postgres.NewClassificationStore(pool)
	tmplStore := postgres.NewTemplateStore(pool)
	require.NoError(t, presets.InstallCryptoDepositBundle(ctx, classStore, classStore, tmplStore))

	currencyStore := postgres.NewCurrencyStore(pool)
	for _, code := range currencyCodes {
		_, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{Code: code, Name: code, Exponent: 18})
		require.NoError(t, err)
	}

	bookingStore := postgres.NewBookingStore(pool)
	ledgerStore := postgres.NewLedgerStore(pool)

	reader := newFakeChainReader()
	scanner := &fakeChainScanner{balances: make(map[string]decimal.Decimal)}
	sweeper := &fakeSweeper{gasPrice: decimal.NewFromInt(1)}
	deadLetters := postgres.NewIngestDeadLetterStore(pool)

	deps := service.OnchainDeps{
		Registry:        postgres.NewDepositAddressStore(pool),
		Cursors:         postgres.NewChainCursorStore(pool),
		Booker:          bookingStore,
		BookingReader:   bookingStore,
		Journals:        ledgerStore,
		TxComposer:      &testTxComposer{pool: pool, bookingStore: bookingStore, ledgerStore: ledgerStore},
		Reader:          reader,
		Scanner:         scanner,
		Sweeper:         sweeper,
		DeadLetters:     deadLetters,
		Currencies:      currencyStore,
		Classifications: classStore,
	}
	onchain := service.NewOnchain(deps, chains, opts...)

	return &onchainHarness{
		svc:         onchain,
		classes:     classStore,
		bookings:    bookingStore,
		reader:      reader,
		scanner:     scanner,
		sweeper:     sweeper,
		deadLetters: deadLetters,
	}
}

func (h *onchainHarness) classificationUID(t *testing.T, code string) string {
	t.Helper()
	c, err := h.classes.GetByCode(context.Background(), code)
	require.NoError(t, err)
	return c.UID
}

func chainSetWithToken(chainID int64, token, currencyCode string, confirmations int32) core.ChainSet {
	return core.ChainSet{
		chainID: {
			ChainID:       chainID,
			Confirmations: confirmations,
			Factory:       itFactory,
			InitHash:      itInitHash,
			CreditTokens:  map[string]core.TokenConfig{token: {TokenAddress: token, CurrencyCode: currencyCode}},
			SweepTokens:   map[string]core.TokenConfig{token: {TokenAddress: token, CurrencyCode: currencyCode}},
		},
	}
}

// --- EnsureDepositAddress ---

func TestOnchain_EnsureDepositAddress_Idempotent(t *testing.T) {
	chains := chainSetWithToken(1, "0xusdttoken", "USDT-ensure", 2)
	h := setupOnchain(t, chains, []string{"USDT-ensure"})
	ctx := context.Background()

	da1, err := h.svc.EnsureDepositAddress(ctx, 7001)
	require.NoError(t, err)

	expected, err := core.DeriveDepositAddress(itFactory, itInitHash, 7001)
	require.NoError(t, err)
	assert.Equal(t, expected, da1.Address)

	da2, err := h.svc.EnsureDepositAddress(ctx, 7001)
	require.NoError(t, err)
	assert.Equal(t, da1.UID, da2.UID)
	assert.Equal(t, da1.Address, da2.Address)
}

// --- IngestDeposit: full lifecycle, dual-path idempotency, dead-letter ---

func TestOnchain_IngestDeposit_FullLifecycle(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithToken(chainID, token, "USDT-lifecycle", 2)
	h := setupOnchain(t, chains, []string{"USDT-lifecycle"})
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 7101)
	require.NoError(t, err)

	sighting := core.DepositSighting{
		ChainID:       chainID,
		TxHash:        "0xtx1",
		TxLogSeq:      0,
		Token:         token,
		From:          "0xsender",
		To:            da.Address,
		Amount:        decimal.RequireFromString("100"),
		Confirmations: 0,
		BlockNumber:   100,
	}

	booking, err := h.svc.IngestDeposit(ctx, sighting)
	require.NoError(t, err)
	require.NotNil(t, booking)
	assert.Equal(t, core.Status("confirming"), booking.Status)
	assert.Equal(t, da.AccountHolder, booking.AccountHolder)

	// Re-observing the SAME sighting (e.g. watcher re-scanning an overlapping
	// block range) must be a pure idempotent no-op -- no error, no duplicate
	// booking, status unchanged.
	replay, err := h.svc.IngestDeposit(ctx, sighting)
	require.NoError(t, err)
	assert.Equal(t, booking.UID, replay.UID)
	assert.Equal(t, core.Status("confirming"), replay.Status)

	// A second, distinct Transfer log within the SAME tx (different
	// txlog_seq) must NOT collide with the first -- proves the idempotency
	// key is keyed on txlog_seq, not just (chain_id, tx_hash).
	sighting2 := sighting
	sighting2.TxLogSeq = 1
	sighting2.Amount = decimal.RequireFromString("50")
	booking2, err := h.svc.IngestDeposit(ctx, sighting2)
	require.NoError(t, err)
	assert.NotEqual(t, booking.UID, booking2.UID)

	// Now the same underlying transfer (chain_id, tx_hash, txlog_seq
	// unchanged) is re-observed with enough confirmations to confirm --
	// stable identity fields (amount/token/block_number) are unchanged,
	// proving watcher-vs-webhook re-derivation of the same sighting never
	// spuriously ErrConflicts.
	sighting.Confirmations = 5
	confirmed, err := h.svc.IngestDeposit(ctx, sighting)
	require.NoError(t, err)
	assert.Equal(t, booking.UID, confirmed.UID)
	assert.Equal(t, core.Status("confirmed"), confirmed.Status)
	assert.NotEmpty(t, confirmed.JournalUID)
}

func TestOnchain_IngestDeposit_ConflictingPayloadIsDeadLettered(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithToken(chainID, token, "USDT-deadletter", 2)
	h := setupOnchain(t, chains, []string{"USDT-deadletter"})
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 7201)
	require.NoError(t, err)

	sightingA := core.DepositSighting{
		ChainID: chainID, TxHash: "0xtxconflict", TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("100"), BlockNumber: 200,
	}
	_, err = h.svc.IngestDeposit(ctx, sightingA)
	require.NoError(t, err)

	// Simulate a normalization bug: the SAME identity (chain/tx/txlog_seq)
	// observed with a DIFFERENT amount -- CreateBooking's idempotency check
	// must reject this as ErrConflict, and it must be dead-lettered rather
	// than silently retried or swallowed (design doc §6).
	sightingB := sightingA
	sightingB.Amount = decimal.RequireFromString("999")

	_, err = h.svc.IngestDeposit(ctx, sightingB)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrConflict)

	letters, err := h.deadLetters.ListDeadLetters(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, 1, countByTxHash(letters, "0xtxconflict"))

	// Repeating the exact same conflicting call must still error every time
	// (never silently swallowed) but must not spam the dead letter table
	// (RecordDeadLetter is idempotent on idempotency_key).
	_, err = h.svc.IngestDeposit(ctx, sightingB)
	require.Error(t, err)
	letters, err = h.deadLetters.ListDeadLetters(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, 1, countByTxHash(letters, "0xtxconflict"))
}

func countByTxHash(letters []core.IngestDeadLetter, txHash string) int {
	n := 0
	for _, l := range letters {
		if l.TxHash == txHash {
			n++
		}
	}
	return n
}

func TestOnchain_IngestDeposit_IgnoresNonWhitelistedTokenAndUnregisteredAddress(t *testing.T) {
	const chainID = int64(1)
	chains := chainSetWithToken(chainID, "0xusdttoken", "USDT-ignore", 2)
	h := setupOnchain(t, chains, []string{"USDT-ignore"})
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 7301)
	require.NoError(t, err)

	// Non-whitelisted token: ignored, no error, no booking.
	booking, err := h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: "0xtxother", TxLogSeq: 0, Token: "0xrandomtoken",
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("1"), BlockNumber: 1,
	})
	require.NoError(t, err)
	assert.Nil(t, booking)

	// Unregistered `to` address: ignored, no error, no booking.
	unregistered, err := core.DeriveDepositAddress(itFactory, itInitHash, 7302)
	require.NoError(t, err)
	booking, err = h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: "0xtxother2", TxLogSeq: 0, Token: "0xusdttoken",
		From: "0xsender", To: unregistered, Amount: decimal.RequireFromString("1"), BlockNumber: 1,
	})
	require.NoError(t, err)
	assert.Nil(t, booking)
}

// --- Sweep ---

func TestOnchain_Sweep_NonceReuseAndNoJournal(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithToken(chainID, token, "USDT-sweep", 2)
	h := setupOnchain(t, chains, []string{"USDT-sweep"})
	ctx := context.Background()

	da1, err := h.svc.EnsureDepositAddress(ctx, 7401)
	require.NoError(t, err)
	da2, err := h.svc.EnsureDepositAddress(ctx, 7402)
	require.NoError(t, err)

	h.scanner.balances[da1.Address] = decimal.NewFromInt(50) // above threshold
	h.scanner.balances[da2.Address] = decimal.NewFromInt(5)  // below threshold, excluded

	policy := core.SweepPolicy{
		ChainID:      chainID,
		Token:        token,
		MinThreshold: decimal.NewFromInt(10),
		GasCeiling:   decimal.NewFromInt(100),
		BatchLimit:   10,
		Interval:     time.Minute,
	}

	require.NoError(t, h.svc.RunSweepOnce(ctx, policy))

	sweepUID := h.classificationUID(t, "sweep")
	bookings, _, err := h.bookings.ListBookings(ctx, core.BookingFilter{ClassificationUID: sweepUID, Status: "sent", Limit: 10})
	require.NoError(t, err)
	require.Len(t, bookings, 1)
	b := bookings[0]
	assert.True(t, decimal.NewFromInt(50).Equal(b.Amount))
	assert.Empty(t, b.JournalUID, "sweep bookings must never post a journal")
	assert.Equal(t, "0", b.Metadata["nonce"])
	assert.Equal(t, da1.Address, b.Metadata["addresses"])

	// BatchSweep must have been called with the resolved SweepTarget (address
	// + account holder), not a bare address -- CREATE2 derivation is one-way,
	// so the adapter cannot recover da1's holder from its address alone; the
	// registry lookup that supplies it must actually have happened.
	require.Len(t, h.sweeper.batchSweeps, 1)
	require.Len(t, h.sweeper.batchSweeps[0].targets, 1)
	assert.Equal(t, core.SweepTarget{Address: da1.Address, AccountHolder: 7401}, h.sweeper.batchSweeps[0].targets[0])

	// Second tick before inclusion: same in-flight booking is reused, no new
	// nonce and no new broadcast.
	h.reader.setIncluded(chainID, b.ChannelRef, false)
	require.NoError(t, h.svc.RunSweepOnce(ctx, policy))
	assert.Equal(t, 1, h.sweeper.nextNonceCall, "nonce must be requested exactly once and reused thereafter")

	// Now the broadcast tx is included -- next tick confirms it.
	h.reader.setIncluded(chainID, b.ChannelRef, true)
	require.NoError(t, h.svc.RunSweepOnce(ctx, policy))

	confirmed, err := h.bookings.GetBooking(ctx, b.UID)
	require.NoError(t, err)
	assert.Equal(t, core.Status("confirmed"), confirmed.Status)
	assert.Empty(t, confirmed.JournalUID, "sweep bookings must never post a journal, even once confirmed")

	assert.Len(t, h.sweeper.batchSweeps, 1, "BatchSweep must be called exactly once across the whole flow (no gas-bump needed, tx included on first check)")
}

func TestOnchain_Sweep_UnattributedToken(t *testing.T) {
	const chainID = int64(1)
	chains := core.ChainSet{
		chainID: {
			ChainID:       chainID,
			Confirmations: 2,
			Factory:       itFactory,
			InitHash:      itInitHash,
			CreditTokens:  map[string]core.TokenConfig{"0xusdttoken": {TokenAddress: "0xusdttoken", CurrencyCode: "USDT-unattr"}},
			SweepTokens: map[string]core.TokenConfig{
				core.SweepNativeToken: {TokenAddress: core.SweepNativeToken, CurrencyCode: "NATIVE-unattr"},
			},
		},
	}
	h := setupOnchain(t, chains, []string{"USDT-unattr", "NATIVE-unattr"})
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 7501)
	require.NoError(t, err)
	h.scanner.balances[da.Address] = decimal.NewFromInt(1)

	policy := core.SweepPolicy{
		ChainID:      chainID,
		Token:        core.SweepNativeToken,
		MinThreshold: decimal.Zero,
		GasCeiling:   decimal.NewFromInt(100),
		BatchLimit:   10,
		Interval:     time.Minute,
	}
	require.NoError(t, h.svc.RunSweepOnce(ctx, policy))

	sweepUID := h.classificationUID(t, "sweep")
	bookings, _, err := h.bookings.ListBookings(ctx, core.BookingFilter{ClassificationUID: sweepUID, Status: "sent", Limit: 10})
	require.NoError(t, err)
	require.Len(t, bookings, 1)
	assert.Equal(t, "native", bookings[0].Metadata["token"])
	assert.Empty(t, bookings[0].JournalUID)
}
