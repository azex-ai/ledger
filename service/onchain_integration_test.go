package service_test

import (
	"context"
	"fmt"
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
	latest   map[int64]int64 // key: chainID -- see setLatestBlock
}

func newFakeChainReader() *fakeChainReader {
	return &fakeChainReader{included: make(map[string]bool), latest: make(map[int64]int64)}
}

func (f *fakeChainReader) LatestBlock(ctx context.Context, chainID int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.latest[chainID], nil // 0 (map zero value) unless setLatestBlock was called
}

// setLatestBlock drives the recheck loop's confirmations math
// (latest - booking's stored BlockNumber + 1) against a specific chain head
// -- see TestOnchain_RecheckPendingDeposits_HonorsRealBlockNumber, which
// needs a real, controllable head to pin the C1 regression (recheck must
// derive confirmations from the booking's actual stored BlockNumber, not
// silently treat every deposit as already past its confirmation threshold).
func (f *fakeChainReader) setLatestBlock(chainID, block int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latest[chainID] = block
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

// fakeDepositConfirmer implements service.DepositConfirmer for the M3
// reconciliation gate tests (design doc §9.3). Each (chainID, txHash,
// txLogSeq) must be explicitly seeded via set -- an unseeded key returns
// included=false, matching "this second source has no idea about this
// transfer," which is exactly the disagreement reviewGate must catch.
type fakeDepositConfirmer struct {
	mu        sync.Mutex
	responses map[string]fakeConfirmResponse
	calls     int
}

type fakeConfirmResponse struct {
	amount   decimal.Decimal
	included bool
}

func newFakeDepositConfirmer() *fakeDepositConfirmer {
	return &fakeDepositConfirmer{responses: make(map[string]fakeConfirmResponse)}
}

func confirmKey(chainID int64, txHash string, txLogSeq int32) string {
	return fmt.Sprintf("%d:%s:%d", chainID, txHash, txLogSeq)
}

func (f *fakeDepositConfirmer) set(chainID int64, txHash string, txLogSeq int32, amount decimal.Decimal, included bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses[confirmKey(chainID, txHash, txLogSeq)] = fakeConfirmResponse{amount: amount, included: included}
}

func (f *fakeDepositConfirmer) ConfirmDeposit(ctx context.Context, chainID int64, txHash string, txLogSeq int32) (decimal.Decimal, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	r, ok := f.responses[confirmKey(chainID, txHash, txLogSeq)]
	if !ok {
		return decimal.Zero, false, nil
	}
	return r.amount, r.included, nil
}

// --- test harness ---

type onchainHarness struct {
	svc        *service.Onchain
	classes    *postgres.ClassificationStore
	bookings   *postgres.BookingStore
	currencies *postgres.CurrencyStore

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
		currencies:  currencyStore,
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

// chainSetWithCeilings mirrors chainSetWithToken but also wires the M3
// compensating-control ceilings (design doc §9.2/§9.3) onto the credit
// token's config -- zero disables the corresponding gate, same as
// chainSetWithToken's implicit zero-value defaults.
func chainSetWithCeilings(chainID int64, token, currencyCode string, confirmations int32, autoCreditCeiling, reconcileCeiling decimal.Decimal) core.ChainSet {
	return core.ChainSet{
		chainID: {
			ChainID:       chainID,
			Confirmations: confirmations,
			Factory:       itFactory,
			InitHash:      itInitHash,
			CreditTokens: map[string]core.TokenConfig{
				token: {
					TokenAddress:      token,
					CurrencyCode:      currencyCode,
					AutoCreditCeiling: autoCreditCeiling,
					ReconcileCeiling:  reconcileCeiling,
				},
			},
			SweepTokens: map[string]core.TokenConfig{token: {TokenAddress: token, CurrencyCode: currencyCode}},
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

// TestOnchain_RecheckPendingDeposits_HonorsRealBlockNumber is C1's
// regression: recheckPendingDeposits must compute confirmations from the
// booking's actually-stored BlockNumber against the chain's real current
// head, not silently treat every pending/confirming deposit as already past
// its confirmation threshold. Earlier tests all drive IngestDeposit/recheck
// with a fakeChainReader whose LatestBlock stub returned a constant 0,
// which never exercises this math at all -- that is exactly how both C1
// (BlockNumber never populated by either producer) and the recheck loop's
// blind spot went unnoticed. This test drives the real recheck loop
// (RunPendingRecheckOnce, not IngestDeposit's inline advanceConfirmation
// call) against a controllable chain head.
func TestOnchain_RecheckPendingDeposits_HonorsRealBlockNumber(t *testing.T) {
	const (
		chainID       = int64(1)
		token         = "0xusdttoken"
		confirmations = int32(6)
	)
	chains := chainSetWithToken(chainID, token, "USDT-recheck", confirmations)
	h := setupOnchain(t, chains, []string{"USDT-recheck"})
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 7501)
	require.NoError(t, err)

	const txHash = "0xreallatest"
	h.reader.setIncluded(chainID, txHash, true) // tx is genuinely still on-chain throughout

	// First sighting: 0 confirmations at observation time, mined at block
	// 100 -- IngestDeposit's own advanceConfirmation call only ever sees this
	// single (stale) confirmations count, so it correctly stays "confirming"
	// (0 < 6). The bug this test targets is entirely in what happens AFTER,
	// on the recheck loop's own re-derivation.
	booking, err := h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: txHash, TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("100"),
		Confirmations: 0, BlockNumber: 100,
	})
	require.NoError(t, err)
	require.Equal(t, core.Status("confirming"), booking.Status)

	// Chain head is only 2 blocks past the deposit's block -- 102-100+1 = 3
	// confirmations, still below the 6-confirmation threshold. Pre-fix, with
	// BlockNumber never persisted (always 0), this would have computed
	// 102-0+1 = 103 >= 6 and wrongly confirmed the deposit on this very
	// first recheck tick.
	h.reader.setLatestBlock(chainID, 102)
	h.svc.RunPendingRecheckOnce(ctx)

	stillPending, err := h.bookings.GetBooking(ctx, booking.UID)
	require.NoError(t, err)
	assert.Equal(t, core.Status("confirming"), stillPending.Status, "must not confirm before the real confirmation threshold is met")
	assert.Empty(t, stillPending.JournalUID)

	// Chain head advances enough to cross the threshold -- 106-100+1 = 7 >= 6.
	h.reader.setLatestBlock(chainID, 106)
	h.svc.RunPendingRecheckOnce(ctx)

	confirmed, err := h.bookings.GetBooking(ctx, booking.UID)
	require.NoError(t, err)
	assert.Equal(t, core.Status("confirmed"), confirmed.Status)
	assert.NotEmpty(t, confirmed.JournalUID)
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

// TestOnchain_Sweep_FailedRevivesToSentWithNewNonce pins the M5 fix: a sweep
// booking that exhausts its gas-bump retries and terminates in "failed" must
// not stay stuck there forever. MaxSweepBumps(0) forces the very first stuck
// check to transition straight to "failed" (no bump attempts needed to
// reproduce the terminal state), then a subsequent sweep tick must revive
// the SAME booking (SweepLifecycle's failed->pending retry edge) with a
// freshly-requested nonce and drive it back to "sent" -- not silently no-op
// on the terminal booking, and not ErrConflict on a colliding idempotency
// key.
func TestOnchain_Sweep_FailedRevivesToSentWithNewNonce(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithToken(chainID, token, "USDT-revive", 2)
	h := setupOnchain(t, chains, []string{"USDT-revive"},
		service.WithMaxSweepBumps(0),
		service.WithSweepStuckAfter(0),
	)
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 7601)
	require.NoError(t, err)
	h.scanner.balances[da.Address] = decimal.NewFromInt(50)

	policy := core.SweepPolicy{
		ChainID:      chainID,
		Token:        token,
		MinThreshold: decimal.NewFromInt(10),
		GasCeiling:   decimal.NewFromInt(100),
		BatchLimit:   10,
		Interval:     time.Minute,
	}

	// Tick 1: pending -> sent, nonce 0.
	require.NoError(t, h.svc.RunSweepOnce(ctx, policy))
	sweepUID := h.classificationUID(t, "sweep")
	bookings, _, err := h.bookings.ListBookings(ctx, core.BookingFilter{ClassificationUID: sweepUID, Status: "sent", Limit: 10})
	require.NoError(t, err)
	require.Len(t, bookings, 1)
	firstBookingUID := bookings[0].UID
	require.Equal(t, "0", bookings[0].Metadata["nonce"])

	// Tick 2: not included, maxSweepBumps=0 -> straight to terminal "failed".
	h.reader.setIncluded(chainID, bookings[0].ChannelRef, false)
	require.NoError(t, h.svc.RunSweepOnce(ctx, policy))
	failedBooking, err := h.bookings.GetBooking(ctx, firstBookingUID)
	require.NoError(t, err)
	require.Equal(t, core.Status("failed"), failedBooking.Status)

	// Tick 3: must revive the SAME booking (not ErrConflict, not a silent
	// no-op) using a freshly-requested nonce, and re-broadcast.
	require.NoError(t, h.svc.RunSweepOnce(ctx, policy))
	revived, err := h.bookings.GetBooking(ctx, firstBookingUID)
	require.NoError(t, err)
	assert.Equal(t, core.Status("sent"), revived.Status, "failed sweep must be revived to sent, not stuck or silently ignored")
	assert.Equal(t, firstBookingUID, revived.UID, "revival reuses the same booking (audit trail), not a new one")
	assert.Equal(t, "1", revived.Metadata["nonce"], "revival must use a freshly-requested nonce, not the stale/stuck one")

	require.Len(t, h.sweeper.batchSweeps, 2, "the revived attempt broadcasts a second batchSweep call")
	assert.Equal(t, uint64(0), h.sweeper.batchSweeps[0].nonce)
	assert.Equal(t, uint64(1), h.sweeper.batchSweeps[1].nonce, "revival's broadcast must carry the new nonce, not the stale one")

	// Tick 4: the revived broadcast gets included -> confirms cleanly.
	h.reader.setIncluded(chainID, revived.ChannelRef, true)
	require.NoError(t, h.svc.RunSweepOnce(ctx, policy))
	confirmed, err := h.bookings.GetBooking(ctx, firstBookingUID)
	require.NoError(t, err)
	assert.Equal(t, core.Status("confirmed"), confirmed.Status)
	assert.Empty(t, confirmed.JournalUID, "sweep bookings must never post a journal, even after a revival")
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

// --- M3 compensating controls: threshold gate + reconciliation -> review
// (docs/plans/2026-07-11-crypto-deposit-sweep-design.md §9, I-21) ---

// TestOnchain_IngestDeposit_OverCeiling_RoutesToReview is the safety
// regression this whole control exists for (design doc §9.2): a single-source
// sighting whose amount exceeds AutoCreditCeiling must be parked in review,
// not auto-credited, even though it clears the confirmation threshold on its
// own -- this is exactly the unbounded-mint path M3 closes.
func TestOnchain_IngestDeposit_OverCeiling_RoutesToReview(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithCeilings(chainID, token, "USDT-overceil", 2, decimal.NewFromInt(100), decimal.Zero)
	h := setupOnchain(t, chains, []string{"USDT-overceil"})
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 8001)
	require.NoError(t, err)

	booking, err := h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: "0xoverceil", TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("150"),
		Confirmations: 5, BlockNumber: 100,
	})
	require.NoError(t, err)
	require.NotNil(t, booking)
	assert.Equal(t, core.Status("review"), booking.Status)
	assert.Empty(t, booking.JournalUID, "review must not post a journal (I-21)")
	assert.Equal(t, "over_ceiling", booking.Metadata["review_reason"])

	// Re-observing the same sighting while parked in review must be a no-op
	// (advanceConfirmation's "review" early-return), not re-evaluate the gate
	// or re-transition.
	replay, err := h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: "0xoverceil", TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("150"),
		Confirmations: 6, BlockNumber: 100,
	})
	require.NoError(t, err)
	assert.Equal(t, core.Status("review"), replay.Status)
	assert.Empty(t, replay.JournalUID)
}

// TestOnchain_IngestDeposit_UnderCeiling_StillConfirms pins the opt-in
// contract: a deposit below AutoCreditCeiling takes the pre-M3 path
// unchanged (auto-credited with a journal as soon as it clears the
// confirmation threshold).
func TestOnchain_IngestDeposit_UnderCeiling_StillConfirms(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithCeilings(chainID, token, "USDT-underceil", 2, decimal.NewFromInt(100), decimal.Zero)
	h := setupOnchain(t, chains, []string{"USDT-underceil"})
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 8002)
	require.NoError(t, err)

	booking, err := h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: "0xunderceil", TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("50"),
		Confirmations: 5, BlockNumber: 100,
	})
	require.NoError(t, err)
	require.NotNil(t, booking)
	assert.Equal(t, core.Status("confirmed"), booking.Status)
	assert.NotEmpty(t, booking.JournalUID)
}

// TestOnchain_IngestDeposit_ReconcileMismatch_RoutesToReview drives the
// reconciliation gate in isolation (AutoCreditCeiling disabled): a second,
// independent source disagreeing with the primary sighting's amount routes
// to review even though the primary sighting alone would have confirmed.
func TestOnchain_IngestDeposit_ReconcileMismatch_RoutesToReview(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
		txHash  = "0xreconcilemismatch"
	)
	confirmer := newFakeDepositConfirmer()
	confirmer.set(chainID, txHash, 0, decimal.RequireFromString("999"), true) // disagrees with the primary sighting's amount

	chains := chainSetWithCeilings(chainID, token, "USDT-mismatch", 2, decimal.Zero, decimal.NewFromInt(10))
	h := setupOnchain(t, chains, []string{"USDT-mismatch"}, service.WithDepositConfirmer(confirmer))
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 8003)
	require.NoError(t, err)

	booking, err := h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: txHash, TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("100"),
		Confirmations: 5, BlockNumber: 100,
	})
	require.NoError(t, err)
	require.NotNil(t, booking)
	assert.Equal(t, core.Status("review"), booking.Status)
	assert.Empty(t, booking.JournalUID)
	assert.Equal(t, "reconcile_mismatch", booking.Metadata["review_reason"])
	assert.Equal(t, 1, confirmer.calls)
}

// TestOnchain_IngestDeposit_ReconcileMatch_Confirms: the second source
// agreeing exactly (same amount, included=true) lets the deposit proceed to
// confirmed as normal.
func TestOnchain_IngestDeposit_ReconcileMatch_Confirms(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
		txHash  = "0xreconcilematch"
	)
	confirmer := newFakeDepositConfirmer()
	confirmer.set(chainID, txHash, 0, decimal.RequireFromString("100"), true)

	chains := chainSetWithCeilings(chainID, token, "USDT-match", 2, decimal.Zero, decimal.NewFromInt(10))
	h := setupOnchain(t, chains, []string{"USDT-match"}, service.WithDepositConfirmer(confirmer))
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 8004)
	require.NoError(t, err)

	booking, err := h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: txHash, TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("100"),
		Confirmations: 5, BlockNumber: 100,
	})
	require.NoError(t, err)
	require.NotNil(t, booking)
	assert.Equal(t, core.Status("confirmed"), booking.Status)
	assert.NotEmpty(t, booking.JournalUID)
	assert.Equal(t, 1, confirmer.calls)
}

// TestOnchain_IngestDeposit_ReconcileBelowCeiling_SkipsRPCCall: amounts at or
// below ReconcileCeiling never call the second source at all (design doc
// §9.3: "小额不值得双查 RPC 成本").
func TestOnchain_IngestDeposit_ReconcileBelowCeiling_SkipsRPCCall(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	confirmer := newFakeDepositConfirmer() // no responses seeded -- any call would report included=false

	chains := chainSetWithCeilings(chainID, token, "USDT-belowreconcile", 2, decimal.Zero, decimal.NewFromInt(1000))
	h := setupOnchain(t, chains, []string{"USDT-belowreconcile"}, service.WithDepositConfirmer(confirmer))
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 8005)
	require.NoError(t, err)

	booking, err := h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: "0xbelowreconcile", TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("50"),
		Confirmations: 5, BlockNumber: 100,
	})
	require.NoError(t, err)
	require.NotNil(t, booking)
	assert.Equal(t, core.Status("confirmed"), booking.Status)
	assert.NotEmpty(t, booking.JournalUID)
	assert.Zero(t, confirmer.calls, "amount at/below ReconcileCeiling must never call the second source")
}

// erroringDepositConfirmer implements service.DepositConfirmer by always
// returning err -- the mi4 regression fixture (a real second-source outage:
// RPC timeout, provider 5xx, etc.), as opposed to fakeDepositConfirmer's
// "no idea about this transfer" (included=false, no error) which models a
// second source that is reachable but disagrees.
type erroringDepositConfirmer struct{ err error }

func (c *erroringDepositConfirmer) ConfirmDeposit(ctx context.Context, chainID int64, txHash string, txLogSeq int32) (decimal.Decimal, bool, error) {
	return decimal.Zero, false, c.err
}

// TestOnchain_IngestDeposit_ReconcileError_FailsClosedStaysConfirming pins
// mi4 (docs/bugs/2026-07-11-m3-security-review.md): when the reconciliation
// gate's second source itself errors (as opposed to disagreeing), the
// deposit path must fail CLOSED -- IngestDeposit returns an error, the
// booking stays in "confirming" (neither auto-confirmed -- no unbounded
// mint on a source outage -- nor pushed into "review", which is reserved for
// a genuine disagreement a human needs to adjudicate), and no journal is
// ever posted. This is the safety-critical branch reviewGate/advanceConfirmation
// take when DepositConfirmer.ConfirmDeposit itself errors (onchain.go's
// reviewGate: "reconcile: %w" wrap), previously exercised only by
// inspection, never asserted.
func TestOnchain_IngestDeposit_ReconcileError_FailsClosedStaysConfirming(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
		txHash  = "0xreconcileerr"
	)
	confirmer := &erroringDepositConfirmer{err: fmt.Errorf("second source unreachable: timeout")}

	chains := chainSetWithCeilings(chainID, token, "USDT-reconcileerr", 2, decimal.Zero, decimal.NewFromInt(10))
	h := setupOnchain(t, chains, []string{"USDT-reconcileerr"}, service.WithDepositConfirmer(confirmer))
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 8010)
	require.NoError(t, err)

	_, err = h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: txHash, TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("100"),
		Confirmations: 5, BlockNumber: 100,
	})
	require.Error(t, err, "a reconciliation-source error must fail closed, not be swallowed")

	depositUID := h.classificationUID(t, presets.DepositClassificationCode)
	bookings, _, err := h.bookings.ListBookings(ctx, core.BookingFilter{
		ClassificationUID: depositUID,
		Status:            "confirming",
		Limit:             100,
	})
	require.NoError(t, err)
	var found *core.Booking
	for i := range bookings {
		if bookings[i].ChannelRef == txHash+"#0" {
			found = &bookings[i]
		}
	}
	require.NotNil(t, found, "booking must exist and be parked in confirming, not lost")
	assert.Equal(t, core.Status("confirming"), found.Status, "must stay confirming -- neither auto-confirmed nor routed to review on a source ERROR (as opposed to a genuine disagreement)")
	assert.Empty(t, found.JournalUID, "no journal may be posted while fail-closed (I-21)")
}

// --- M3.1 secure-by-default: Run() refuses to start with an unconfigured
// AutoCreditCeiling (design doc §9.2 addendum, MJ1) ---

// TestOnchain_Run_RejectsUnconfiguredAutoCreditCeiling pins MJ1: a
// CreditTokens entry that never set AutoCreditCeiling (chainSetWithToken's
// implicit zero value) must fail Run() outright, rather than silently
// running with the pre-M3 unbounded-mint trust model.
func TestOnchain_Run_RejectsUnconfiguredAutoCreditCeiling(t *testing.T) {
	chains := chainSetWithToken(1, "0xusdttoken", "USDT-run-reject", 2) // AutoCreditCeiling left at zero
	h := setupOnchain(t, chains, []string{"USDT-run-reject"})

	err := h.svc.Run(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)
	assert.Contains(t, err.Error(), "AutoCreditCeiling")
}

// TestOnchain_Run_AllowsExplicitUnboundedSentinel pins MJ1's escape hatch: a
// consumer that deliberately sets AutoCreditCeiling to
// core.UnboundedAutoCredit (an explicit risk acceptance) is NOT blocked by
// the same startup check.
func TestOnchain_Run_AllowsExplicitUnboundedSentinel(t *testing.T) {
	chains := chainSetWithCeilings(1, "0xusdttoken", "USDT-run-allow", 2, core.UnboundedAutoCredit, decimal.Zero)
	h := setupOnchain(t, chains, []string{"USDT-run-allow"})

	// Pre-cancel: Run()'s background loops (watch/recheck/reorg-recheck) hit
	// their ctx.Done() case immediately, so Run() returns as soon as startup
	// validation passes -- this test only cares that validation didn't
	// reject the sentinel, not about the loops actually ticking.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := h.svc.Run(ctx)
	require.NoError(t, err)
}

// TestOnchain_ApproveReview_PostsJournalWithEventLink pins the approve half
// of I-21: a reviewed booking has zero ledger effect until ApproveReview is
// called, at which point it posts the SAME deposit_confirm journal path
// (EventUID cross-link) the normal confirming->confirmed transition uses.
// Idempotent re-approval is a no-op, not a second journal.
func TestOnchain_ApproveReview_PostsJournalWithEventLink(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithCeilings(chainID, token, "USDT-approve", 2, decimal.NewFromInt(100), decimal.Zero)
	h := setupOnchain(t, chains, []string{"USDT-approve"})
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 8006)
	require.NoError(t, err)

	reviewed, err := h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: "0xapprove", TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("150"),
		Confirmations: 5, BlockNumber: 100,
	})
	require.NoError(t, err)
	require.Equal(t, core.Status("review"), reviewed.Status)
	require.Empty(t, reviewed.JournalUID)

	approved, err := h.svc.ApproveReview(ctx, reviewed.UID, "ops-alice")
	require.NoError(t, err)
	assert.Equal(t, core.Status("confirmed"), approved.Status)
	assert.NotEmpty(t, approved.JournalUID)
	// MJ2: the approving actor must land on the booking's audit trail.
	assert.Equal(t, "ops-alice", approved.Metadata["approved_by"])

	journalUID := approved.JournalUID

	// Idempotent re-approval: no error, same booking, no second journal.
	again, err := h.svc.ApproveReview(ctx, reviewed.UID, "ops-alice")
	require.NoError(t, err)
	assert.Equal(t, core.Status("confirmed"), again.Status)
	assert.Equal(t, journalUID, again.JournalUID)
}

// TestOnchain_RejectReview_NoJournal pins the reject half of I-21: a rejected
// booking transitions to failed and NEVER posts a journal -- the deposit is
// never credited. Idempotent re-rejection is a no-op.
func TestOnchain_RejectReview_NoJournal(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithCeilings(chainID, token, "USDT-reject", 2, decimal.NewFromInt(100), decimal.Zero)
	h := setupOnchain(t, chains, []string{"USDT-reject"})
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 8007)
	require.NoError(t, err)

	reviewed, err := h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: "0xreject", TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("150"),
		Confirmations: 5, BlockNumber: 100,
	})
	require.NoError(t, err)
	require.Equal(t, core.Status("review"), reviewed.Status)

	rejected, err := h.svc.RejectReview(ctx, reviewed.UID, "ops-bob", "suspected fraud")
	require.NoError(t, err)
	assert.Equal(t, core.Status("failed"), rejected.Status)
	assert.Empty(t, rejected.JournalUID, "reject must never post a journal (I-21)")
	assert.Equal(t, "suspected fraud", rejected.Metadata["reject_reason"])
	// MJ2: the rejecting actor must land on the booking's audit trail.
	assert.Equal(t, "ops-bob", rejected.Metadata["rejected_by"])

	// Idempotent re-rejection: no error, same terminal booking, still no journal.
	again, err := h.svc.RejectReview(ctx, reviewed.UID, "ops-bob", "suspected fraud")
	require.NoError(t, err)
	assert.Equal(t, core.Status("failed"), again.Status)
	assert.Empty(t, again.JournalUID)
}

// TestOnchain_ApproveReview_RejectReview_ConflictWhenNotReview: calling
// either operation on a booking that never entered review is a conflict, not
// a silent no-op or a forced transition.
func TestOnchain_ApproveReview_RejectReview_ConflictWhenNotReview(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithToken(chainID, token, "USDT-notreview", 10) // high confirmation threshold -- stays "confirming"
	h := setupOnchain(t, chains, []string{"USDT-notreview"})
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 8008)
	require.NoError(t, err)

	confirming, err := h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: "0xnotreview", TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("50"),
		Confirmations: 0, BlockNumber: 100,
	})
	require.NoError(t, err)
	require.Equal(t, core.Status("confirming"), confirming.Status)

	_, err = h.svc.ApproveReview(ctx, confirming.UID, "ops-key")
	assert.ErrorIs(t, err, core.ErrConflict)

	_, err = h.svc.RejectReview(ctx, confirming.UID, "ops-key", "n/a")
	assert.ErrorIs(t, err, core.ErrConflict)
}

// TestOnchain_ApproveReview_RejectReview_RefuseNonDepositClassification pins
// mi1 (defense-in-depth, docs/bugs/2026-07-11-m3-security-review.md): even
// though today only the deposit classification's lifecycle ever reaches
// "review", ApproveReview/RejectReview must not trust a booking's status
// alone. A booking under a DIFFERENT classification that happens to reach a
// state also named "review" (simulating a hypothetical future preset, not
// today's installed bundle) must be refused, not silently driven through
// depositConfirmTemplate.
func TestOnchain_ApproveReview_RejectReview_RefuseNonDepositClassification(t *testing.T) {
	chains := chainSetWithCeilings(1, "0xusdttoken", "USDT-mi1", 2, decimal.NewFromInt(100), decimal.Zero)
	h := setupOnchain(t, chains, []string{"USDT-mi1"})
	ctx := context.Background()

	otherLifecycle := &core.Lifecycle{
		Initial:  "pending",
		Terminal: []core.Status{"done"},
		Transitions: map[core.Status][]core.Status{
			"pending": {"review"},
			"review":  {"done"},
		},
	}
	otherClass, err := h.classes.CreateClassification(ctx, core.ClassificationInput{
		Code:       "other-thing",
		Name:       "Other Thing",
		NormalSide: core.NormalSideCredit,
		Lifecycle:  otherLifecycle,
	})
	require.NoError(t, err)

	currencies, err := h.currencies.ListCurrencies(ctx, false)
	require.NoError(t, err)
	var currencyUID string
	for _, c := range currencies {
		if c.Code == "USDT-mi1" {
			currencyUID = c.UID
		}
	}
	require.NotEmpty(t, currencyUID)

	otherBooking, err := h.bookings.CreateBooking(ctx, core.CreateBookingInput{
		ClassificationCode: otherClass.Code,
		AccountHolder:      9999,
		CurrencyUID:        currencyUID,
		Amount:             decimal.NewFromInt(1),
		IdempotencyKey:     "other-thing-1",
	})
	require.NoError(t, err)
	_, err = h.bookings.Transition(ctx, core.TransitionInput{
		BookingUID: otherBooking.UID,
		ToStatus:   "review",
		Source:     "test",
	})
	require.NoError(t, err)

	_, err = h.svc.ApproveReview(ctx, otherBooking.UID, "attacker")
	assert.ErrorIs(t, err, core.ErrConflict)

	_, err = h.svc.RejectReview(ctx, otherBooking.UID, "attacker", "n/a")
	assert.ErrorIs(t, err, core.ErrConflict)
}

// TestOnchain_ListReviews pins the HTTP review queue's backing query (design
// doc §9.4): only deposit bookings currently parked in review are returned --
// a booking that is confirming (never routed to review) or already resolved
// (approved back to confirmed) must not appear.
func TestOnchain_ListReviews(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithCeilings(chainID, token, "USDT-listreviews", 2, decimal.NewFromInt(100), decimal.Zero)
	h := setupOnchain(t, chains, []string{"USDT-listreviews"})
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 8009)
	require.NoError(t, err)

	// Stays in review.
	reviewed, err := h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: "0xlistreview1", TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("150"),
		Confirmations: 5, BlockNumber: 100,
	})
	require.NoError(t, err)
	require.Equal(t, core.Status("review"), reviewed.Status)

	// Resolved out of review -- must not appear in the queue anymore.
	resolved, err := h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: "0xlistreview2", TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("200"),
		Confirmations: 5, BlockNumber: 100,
	})
	require.NoError(t, err)
	require.Equal(t, core.Status("review"), resolved.Status)
	_, err = h.svc.ApproveReview(ctx, resolved.UID, "ops-key")
	require.NoError(t, err)

	// Never routed to review at all -- must not appear.
	_, err = h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: "0xlistreview3", TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("50"),
		Confirmations: 5, BlockNumber: 100,
	})
	require.NoError(t, err)

	bookings, next, err := h.svc.ListReviews(ctx, "", 50)
	require.NoError(t, err)
	assert.Empty(t, next)
	require.Len(t, bookings, 1)
	assert.Equal(t, reviewed.UID, bookings[0].UID)
	assert.Equal(t, core.Status("review"), bookings[0].Status)
	assert.Empty(t, bookings[0].JournalUID)
}
