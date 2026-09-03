package service_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

// recordingServiceLogger captures what the onchain orchestration reports, so
// pins can assert on the trace a fail-closed path is required to leave
// (working-agreements §3) instead of taking the code's word for it.
type recordingServiceLogger struct {
	mu    sync.Mutex
	lines []string
}

func newRecordingServiceLogger() *recordingServiceLogger { return &recordingServiceLogger{} }

func (l *recordingServiceLogger) record(level, msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+msg+" "+fmt.Sprint(args...))
}

func (l *recordingServiceLogger) Info(msg string, args ...any)  { l.record("INFO", msg, args...) }
func (l *recordingServiceLogger) Warn(msg string, args ...any)  { l.record("WARN", msg, args...) }
func (l *recordingServiceLogger) Error(msg string, args ...any) { l.record("ERROR", msg, args...) }

func (l *recordingServiceLogger) contains(substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

func (h *onchainHarness) logger(t *testing.T) *recordingServiceLogger {
	t.Helper()
	l, ok := h.log.(*recordingServiceLogger)
	require.True(t, ok, "harness logger must be the recording one")
	return l
}

func (h *onchainHarness) cursorBlock(t *testing.T, chainID int64) int64 {
	t.Helper()
	cursor, err := h.cursors.GetCursor(context.Background(), chainID)
	if errors.Is(err, core.ErrNotFound) {
		return -1
	}
	require.NoError(t, err)
	return cursor.LastScannedBlock
}

func (h *onchainHarness) deadLetterCount(t *testing.T) int {
	t.Helper()
	rows, _, err := h.deadLetters.ListDeadLetters(context.Background(), "", 100)
	require.NoError(t, err)
	return len(rows)
}

// --- G-C1 / I-52: the cursor must not outrun ingestion --------------------

// TestOnchain_Watch_HoldsCursorWhenIngestFails pins G-C1
// (docs/audits/2026-09-02-deep-audit/onchain-money-path.md Critical #1, also
// reported independently by the concurrency and operability territories).
//
// scanChainOnce used to log an ingest failure and advance the cursor anyway.
// Nothing ever looks at a block below the cursor again -- the forward scan
// starts at cursor+1, the recheck loops only revisit bookings that already
// exist, and registration rescans only cover newly registered addresses -- so
// one failed CreateBooking meant a real, on-chain deposit that the ledger
// would never see again. No dead letter, no metric; ChainCursorLag even
// stayed healthy, because the cursor kept moving.
//
// The failure injected here is a transient one (a plain error, which
// core.IsRetryable treats as retryable), so the correct behavior is to hold
// the cursor and retry the whole window. After watcherStallAlertAfter
// consecutive failures the still-blocking sightings are also dead-lettered,
// so an operator can see WHICH transfer the chain is wedged on.
func TestOnchain_Watch_HoldsCursorWhenIngestFails(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithCeilings(chainID, token, "USDT-hold", 1,
		decimal.NewFromInt(100_000), decimal.Zero)
	h := setupOnchain(t, chains, []string{"USDT-hold"}, service.WithWatcherStallAlertAfter(2))
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 8101)
	require.NoError(t, err)

	h.reader.setLatestBlock(chainID, 500)
	h.reader.setSightings(chainID, core.DepositSighting{
		ChainID: chainID, TxHash: "0xheldtx", TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("42"),
		Confirmations: 3, BlockNumber: 400,
	})

	// Injected transient failure: the pending->confirming transition fails,
	// so IngestDeposit returns an error after having created the booking.
	h.booker.failNextTransition("confirming", errors.New("connection reset by peer"))

	err = h.svc.RunWatchOnce(ctx, chainID)
	require.Error(t, err, "a failed ingest must fail the tick, not be logged and forgotten")
	assert.Equal(t, int64(-1), h.cursorBlock(t, chainID),
		"the cursor must not exist yet: the window containing the failed sighting was never fully ingested")

	// Second consecutive failure reaches the escalation threshold: the
	// blocking sighting is dead-lettered so it is findable, and the cursor
	// STILL does not move.
	h.booker.failNextTransition("confirming", errors.New("connection reset by peer"))
	require.Error(t, h.svc.RunWatchOnce(ctx, chainID))
	assert.Equal(t, int64(-1), h.cursorBlock(t, chainID))
	assert.Equal(t, 1, h.deadLetterCount(t), "the wedged sighting must be recorded for on-call")
	assert.True(t, h.logger(t).contains("watcher: wedged"), "the wedge must be alertable, not just implied")

	// Once ingestion succeeds, the same window is re-scanned (the sighting
	// is still there) and the cursor finally advances.
	require.NoError(t, h.svc.RunWatchOnce(ctx, chainID))
	assert.Equal(t, int64(500), h.cursorBlock(t, chainID))

	bookings, _, err := h.bookings.ListBookings(ctx, core.BookingFilter{
		ClassificationUID: h.classificationUID(t, "deposit"), Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, bookings, 1, "the deposit is booked exactly once despite the re-scan (IngestDeposit is idempotent)")
	assert.Equal(t, core.Status("confirmed"), bookings[0].Status)
}

// TestOnchain_Watch_DeadLettersPermanentRejectionAndAdvances is the other
// half of the G-C1 decision, and the reason it is not simply "never advance
// on any failure": a sighting whose rejection is DETERMINISTIC (here, a token
// whose ledger currency was never registered) fails identically on every
// retry, so holding the cursor for it converts one unbookable deposit into
// "this chain ingests nothing, ever again". Such a sighting is written to
// ingest_dead_letters -- durable, keyed by its idempotency key, findable --
// and only then skipped.
func TestOnchain_Watch_DeadLettersPermanentRejectionAndAdvances(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	// The chain credits "USDT-missing", which is deliberately NOT created in
	// the ledger: currencyResolver.resolve returns core.ErrNotFound.
	chains := chainSetWithCeilings(chainID, token, "USDT-missing", 1,
		decimal.NewFromInt(100_000), decimal.Zero)
	h := setupOnchain(t, chains, []string{"USDT-present"})
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 8201)
	require.NoError(t, err)

	h.reader.setLatestBlock(chainID, 700)
	h.reader.setSightings(chainID, core.DepositSighting{
		ChainID: chainID, TxHash: "0xdeterministic", TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("7"),
		Confirmations: 0, BlockNumber: 690,
	})

	require.NoError(t, h.svc.RunWatchOnce(ctx, chainID),
		"a permanently unbookable sighting must not wedge the chain once it is durably recorded")
	assert.Equal(t, int64(700), h.cursorBlock(t, chainID))

	rows, _, err := h.deadLetters.ListDeadLetters(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1, "skipping is only allowed because the sighting is recorded")
	assert.Equal(t, "deposit-1-0xdeterministic-0", rows[0].IdempotencyKey)
	assert.Contains(t, rows[0].Reason, "not registered")
}

// --- G-M2 / I-53: the scan must stay behind the reorg-mutable tip --------

// TestOnchain_Watch_NeverScansPastConfirmationDepth pins G-M2: scanning to
// the head marked blocks scanned that a reorg can still replace, and the
// forward scan never looks back -- so a transfer that only exists in the
// replacement block was permanently invisible. Confirmations is the
// consumer's own statement of how deep a reorg they expect, so it is also
// the scanner's rollback depth.
func TestOnchain_Watch_NeverScansPastConfirmationDepth(t *testing.T) {
	const (
		chainID       = int64(1)
		token         = "0xusdttoken"
		confirmations = int32(12)
	)
	chains := chainSetWithToken(chainID, token, "USDT-depth", confirmations)
	h := setupOnchain(t, chains, []string{"USDT-depth"})
	ctx := context.Background()

	_, err := h.svc.EnsureDepositAddress(ctx, 8301)
	require.NoError(t, err)

	h.reader.setLatestBlock(chainID, 1000)
	require.NoError(t, h.svc.RunWatchOnce(ctx, chainID))

	windows := h.reader.fetchWindows()
	require.Len(t, windows, 1)
	assert.LessOrEqual(t, windows[0].toBlock, int64(989),
		"the scan must stop at latest-Confirmations+1 = 989, not at the head")
	assert.LessOrEqual(t, h.cursorBlock(t, chainID), int64(989),
		"and the cursor must not claim a block the scan did not cover")
}

// --- G-M6: an unconfigured token must not disable the ceilings ------------

// TestOnchain_Recheck_UnconfiguredTokenRoutesToReview pins G-M6: the token
// lookup used to be a single-value map read, so a token dropped from
// CreditTokens yielded a zero-valued TokenConfig whose non-positive ceilings
// turned BOTH the auto-credit ceiling and the reconciliation gate off --
// auto-crediting any amount. That is reachable with no attacker at all: a
// delisted token's already-confirming bookings are still driven to confirmed
// by the recheck loop, and startup validation can only see tokens that are
// still configured.
func TestOnchain_Recheck_UnconfiguredTokenRoutesToReview(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	configured := chainSetWithCeilings(chainID, token, "USDT-delist", 6,
		decimal.NewFromInt(300), decimal.Zero)
	h := setupOnchain(t, configured, []string{"USDT-delist"})
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 8401)
	require.NoError(t, err)

	booking, err := h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: "0xdelisted", TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("10000"),
		Confirmations: 0, BlockNumber: 100,
	})
	require.NoError(t, err)
	require.Equal(t, core.Status("confirming"), booking.Status)

	// The deposit is real and the chain says so -- this pin is about the
	// CONFIG vanishing, not about the evidence. Since money-out C-2 the
	// recheck loop re-reads the log before crediting, so the fixture has to
	// provide one or the booking would be parked for the other reason.
	h.reader.setIncluded(chainID, "0xdelisted", true)
	h.reader.setSightings(chainID, core.DepositSighting{
		ChainID: chainID, TxHash: "0xdelisted", TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("10000"),
		Confirmations: 0, BlockNumber: 100,
	})

	// The token is delisted: a new Onchain over the same database, with the
	// token gone from CreditTokens (which is what a config rollback or a
	// contract migration looks like), drives the existing booking.
	delisted := core.ChainSet{chainID: {
		ChainID:       chainID,
		Confirmations: 6,
		Factory:       itFactory,
		InitHash:      itInitHash,
		CreditTokens:  map[string]core.TokenConfig{},
		SweepTokens:   configured[chainID].SweepTokens,
	}}
	h2 := h.rewire(t, delisted)
	h.reader.setLatestBlock(chainID, 200) // far past the 6-confirmation threshold
	require.NoError(t, h2.RunPendingRecheckOnce(ctx))

	after, err := h.bookings.GetBooking(ctx, booking.UID)
	require.NoError(t, err)
	assert.Equal(t, core.Status("review"), after.Status,
		"a deposit whose token config vanished must be parked for review, never auto-credited")
	assert.Empty(t, after.JournalUID, "and no journal may be posted for it")
	assert.Equal(t, "token_unconfigured", after.Metadata["review_reason"])
}

// --- G-M1: one RPC answer must not irreversibly reject a deposit ---------

// TestOnchain_Recheck_ShallowReorgNeedsConsecutiveMisses pins G-M1: a single
// TxIncluded=false used to transition a below-threshold deposit straight to
// terminal "failed", and the booking's idempotency key then absorbed every
// future sighting of the same transfer, so a real deposit could never be
// credited again. TxIncluded answers false for any node that has not caught
// up, and this branch only runs in the window where nodes disagree most --
// the same class of evidence the DEEP reorg path deliberately refuses to act
// on automatically.
func TestOnchain_Recheck_ShallowReorgNeedsConsecutiveMisses(t *testing.T) {
	const (
		chainID       = int64(1)
		token         = "0xusdttoken"
		confirmations = int32(6)
		txHash        = "0xvanishing"
	)
	chains := chainSetWithToken(chainID, token, "USDT-shallow", confirmations)
	h := setupOnchain(t, chains, []string{"USDT-shallow"}, service.WithShallowReorgMisses(3))
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 8501)
	require.NoError(t, err)

	booking, err := h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: txHash, TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("100"),
		Confirmations: 0, BlockNumber: 100,
	})
	require.NoError(t, err)
	require.Equal(t, core.Status("confirming"), booking.Status)

	// Still below the confirmation threshold (102-100+1 = 3 < 6), and the
	// node claims not to know the tx.
	h.reader.setLatestBlock(chainID, 102)
	h.reader.setIncluded(chainID, txHash, false)

	for i := 1; i <= 2; i++ {
		require.NoError(t, h.svc.RunPendingRecheckOnce(ctx))
		current, err := h.bookings.GetBooking(ctx, booking.UID)
		require.NoError(t, err)
		require.Equal(t, core.Status("confirming"), current.Status,
			"miss %d of 3 must not decide anything irreversible", i)
	}

	// One corroborating observation resets the streak: the count is
	// CONSECUTIVE misses, not misses ever.
	h.reader.setIncluded(chainID, txHash, true)
	require.NoError(t, h.svc.RunPendingRecheckOnce(ctx))
	h.reader.setIncluded(chainID, txHash, false)
	for i := 1; i <= 2; i++ {
		require.NoError(t, h.svc.RunPendingRecheckOnce(ctx))
		current, err := h.bookings.GetBooking(ctx, booking.UID)
		require.NoError(t, err)
		require.Equal(t, core.Status("confirming"), current.Status,
			"the streak restarted, so miss %d must still not fail the booking", i)
	}

	// Third consecutive miss: now the booking fails -- and the automatic,
	// irreversible refusal leaves an anomaly row a human has to close out.
	require.NoError(t, h.svc.RunPendingRecheckOnce(ctx))
	failed, err := h.bookings.GetBooking(ctx, booking.UID)
	require.NoError(t, err)
	require.Equal(t, core.Status("failed"), failed.Status)

	open, err := h.reorgs.ListOpenReorgs(ctx, 10)
	require.NoError(t, err)
	require.Len(t, open, 1, "failing a deposit on RPC evidence alone must leave a durable trace")
	assert.Equal(t, core.ReorgKindShallowReorgFailed, open[0].Kind)
	assert.Equal(t, booking.UID, open[0].BookingUID)
	assert.True(t, open[0].IsOpen())

	// And if the transaction turns out to be on chain after all, that is
	// reported loudly and repeatedly -- nothing else can credit the holder.
	h.reader.setIncluded(chainID, txHash, true)
	require.NoError(t, h.svc.RunReorgRecheckOnce(ctx))
	assert.True(t, h.logger(t).contains("failed_tx_returned"),
		"a wrongly-failed deposit whose tx returned must be escalated, not silently left failed")
}

// --- G-M8: a detected reorg must outlive its alert window ----------------

// TestOnchain_ReorgRecheck_AnomalyOutlivesTheRecheckWindow pins G-M8: under
// the default manual policy, a deep reorg produced a Warn log and a counter
// increment, and recheckOneConfirmedDeposit stopped looking once the booking
// was reorgRecheckWindow blocks behind the tip -- about 17 minutes on a
// 2-second chain. RUNBOOK §12 asks on-call to verify against a second source
// before reversing; by the time they did, the signal identifying WHICH
// booking was gone.
func TestOnchain_ReorgRecheck_AnomalyOutlivesTheRecheckWindow(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
		txHash  = "0xdeepreorg"
	)
	chains := chainSetWithToken(chainID, token, "USDT-deep", 1)
	h := setupOnchain(t, chains, []string{"USDT-deep"}, service.WithReorgRecheckWindow(50))
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 8601)
	require.NoError(t, err)

	h.reader.setLatestBlock(chainID, 1000)
	booking, err := h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: txHash, TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("100"),
		Confirmations: 5, BlockNumber: 990,
	})
	require.NoError(t, err)
	require.Equal(t, core.Status("confirmed"), booking.Status)
	require.NotEmpty(t, booking.JournalUID, "the holder's balance has moved -- that is what makes a deep reorg serious")

	// The transaction leaves the canonical chain while the booking is still
	// inside the recheck window.
	h.reader.setIncluded(chainID, txHash, false)
	require.NoError(t, h.svc.RunReorgRecheckOnce(ctx))

	open, err := h.reorgs.ListOpenReorgs(ctx, 10)
	require.NoError(t, err)
	require.Len(t, open, 1)
	require.Equal(t, core.ReorgKindDeepReorg, open[0].Kind)
	require.Equal(t, booking.UID, open[0].BookingUID)
	assert.Equal(t, booking.JournalUID, open[0].JournalUID,
		"the row must name the journal an operator has to reverse")
	firstSeen := open[0].LastSeenAt

	// The tip moves far beyond the window: the confirmed-deposit scan now
	// skips this booking entirely (that is its cost bound), but the open
	// anomaly is still re-observed and still reported.
	h.reader.setLatestBlock(chainID, 5000)
	time.Sleep(5 * time.Millisecond) // so last_seen_at can be observed to move
	require.NoError(t, h.svc.RunReorgRecheckOnce(ctx))

	stillOpen, err := h.reorgs.ListOpenReorgs(ctx, 10)
	require.NoError(t, err)
	require.Len(t, stillOpen, 1, "an anomaly must not disappear because the booking got old")
	assert.True(t, stillOpen[0].LastSeenAt.After(firstSeen),
		"re-observation must move last_seen_at, so 'still true' is distinguishable from 'nobody looked'")

	// Only an operator takes it off the queue.
	require.NoError(t, h.reorgs.ResolveReorg(ctx, core.ReorgKindDeepReorg, booking.UID, "reversed manually per RUNBOOK §12"))
	afterResolve, err := h.reorgs.ListOpenReorgs(ctx, 10)
	require.NoError(t, err)
	assert.Empty(t, afterResolve)

	// A recheck after resolution must not silently reopen it.
	require.NoError(t, h.svc.RunReorgRecheckOnce(ctx))
	afterRecheck, err := h.reorgs.ListOpenReorgs(ctx, 10)
	require.NoError(t, err)
	assert.Empty(t, afterRecheck, "reopening a closed-out anomaly is an operator decision, not a tick's")
}

// TestOnchain_Run_RefusesChainReaderWithoutReorgRecorder is the other half of
// G-M8: the fix is only worth anything if it is wired. A ChainReader with no
// ReorgRecorder means the reorg detector runs and its verdict evaporates, so
// Run refuses to start rather than pretend to protect the money path.
func TestOnchain_Run_RefusesChainReaderWithoutReorgRecorder(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	deps := onchainDepsWithoutReorgRecorder(t, pool)

	chains := chainSetWithCeilings(1, "0xusdttoken", "USDT-norecorder", 1,
		core.UnboundedAutoCredit, decimal.Zero)
	onchain := service.NewOnchain(deps, chains)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled: if the gate ever regresses, Run returns instead of blocking in its loops (F-m4)

	err := onchain.Run(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)
	assert.Contains(t, err.Error(), "ReorgRecorder")
}

// --- G-M5: a lost tx hash must not be papered over with a rebroadcast ----

// TestOnchain_Sweep_DoesNotRebroadcastAfterALostTxHash pins G-M5: BatchSweep
// broadcasts, then Transition(sent) persists the hash. If the persistence
// fails, the transaction is in the mempool and its hash is nowhere -- and the
// next tick used to find the booking still in "pending" and broadcast at the
// same nonce again, with priorTxHash empty. If the first transaction landed,
// every later attempt gets "nonce too low" and this chain's collection stops
// forever; if it is still pending, the replacement goes out underpriced.
//
// The signer EOA is single-deployment by design, so a pending nonce above the
// booking's can only mean our own broadcast consumed it: the tick fails
// closed and says so.
func TestOnchain_Sweep_DoesNotRebroadcastAfterALostTxHash(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithToken(chainID, token, "USDT-orphan", 2)
	h := setupOnchain(t, chains, []string{"USDT-orphan"})
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 8701)
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

	// Tick 1: the broadcast succeeds, persisting its hash does not.
	h.booker.failNextTransition("sent", errors.New("pool exhausted"))
	require.Error(t, h.svc.RunSweepOnce(ctx, policy))
	require.Len(t, h.sweeper.batchSweeps, 1)

	sweepUID := h.classificationUID(t, "sweep")
	pending, _, err := h.bookings.ListBookings(ctx, core.BookingFilter{ClassificationUID: sweepUID, Status: "pending", Limit: 10})
	require.NoError(t, err)
	require.Len(t, pending, 1, "the booking is stranded in pending with no channel_ref -- the state this pin is about")
	require.Empty(t, pending[0].ChannelRef)

	// Tick 2: must NOT rebroadcast at the spent nonce.
	err = h.svc.RunSweepOnce(ctx, policy)
	require.Error(t, err, "the tick must fail closed rather than replay a nonce the chain has already consumed")
	assert.ErrorIs(t, err, core.ErrConflict)
	assert.Len(t, h.sweeper.batchSweeps, 1, "exactly one broadcast total: the orphaned one")
	assert.True(t, h.logger(t).contains("orphaned_broadcast"),
		"the stall must name itself so RUNBOOK §15 recovery can start")
	// M-3: this is the one condition in the file that blocks a (chain,
	// token)'s collection INDEFINITELY -- every later tick finds the same
	// booking and returns the same conflict -- and it had no counter, only a
	// log line into a logger that is NopLogger() by default. It is now on
	// RUNBOOK §14's page-on-any-nonzero table, next to a §15 subsection that
	// actually describes the recovery the error message promises.
	assert.Equal(t, []int64{chainID}, h.metrics.orphanedBroadcastCalls(),
		"the only permanent block on an outbound channel must be countable, not just logged")
}

// --- G-M4 / G-m6: the gas-bump timer must actually reset ------------------

// TestOnchain_Sweep_GasBumpWaitsBetweenBumps pins G-M4: the stuck check
// compared time.Since(bookings.updated_at), and a gas-bump deliberately
// performs no transition, so updated_at never moved. Once the threshold was
// crossed it stayed crossed and EVERY subsequent tick bumped -- consuming the
// whole retry budget at the sweep interval instead of at sweepStuckAfter, and
// bidding 1.125^n up whatever gas spike caused the stall. It also mixed
// clocks: updated_at is Postgres', time.Since is this process' (G-m6).
func TestOnchain_Sweep_GasBumpWaitsBetweenBumps(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
		stuck   = 60 * time.Millisecond
	)
	chains := chainSetWithToken(chainID, token, "USDT-bumpwait", 2)
	h := setupOnchain(t, chains, []string{"USDT-bumpwait"},
		service.WithSweepStuckAfter(stuck),
		service.WithMaxSweepBumps(5),
	)
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 8801)
	require.NoError(t, err)
	h.scanner.balances[da.Address] = decimal.NewFromInt(50)

	policy := core.SweepPolicy{
		ChainID:      chainID,
		Token:        token,
		MinThreshold: decimal.NewFromInt(10),
		GasCeiling:   decimal.NewFromInt(100),
		BatchLimit:   10,
		Interval:     time.Millisecond,
	}

	require.NoError(t, h.svc.RunSweepOnce(ctx, policy)) // initial broadcast
	require.Len(t, h.sweeper.batchSweeps, 1)

	sweepUID := h.classificationUID(t, "sweep")
	sent, _, err := h.bookings.ListBookings(ctx, core.BookingFilter{ClassificationUID: sweepUID, Status: "sent", Limit: 10})
	require.NoError(t, err)
	require.Len(t, sent, 1)
	h.reader.setIncluded(chainID, sent[0].ChannelRef, false)

	// Four rapid ticks well inside the stuck window: no bump at all.
	for i := 0; i < 4; i++ {
		require.NoError(t, h.svc.RunSweepOnce(ctx, policy))
	}
	require.Len(t, h.sweeper.batchSweeps, 1, "no bump may happen before sweepStuckAfter elapses")

	// Past the window: exactly one bump...
	time.Sleep(stuck + 20*time.Millisecond)
	require.NoError(t, h.svc.RunSweepOnce(ctx, policy))
	require.Len(t, h.sweeper.batchSweeps, 2)
	h.reader.setIncluded(chainID, h.sweeper.batchSweeps[1].priorTxHash, false)

	// ...and the next four rapid ticks must NOT bump again: the timer
	// restarted at the bump. This is the assertion the old implementation
	// fails (it bumped on every tick, four more times here).
	for i := 0; i < 4; i++ {
		require.NoError(t, h.svc.RunSweepOnce(ctx, policy))
	}
	assert.Len(t, h.sweeper.batchSweeps, 2, "each gas-bump must be separated by sweepStuckAfter, not by the sweep interval")

	// After another full window, the second bump is allowed.
	time.Sleep(stuck + 20*time.Millisecond)
	require.NoError(t, h.svc.RunSweepOnce(ctx, policy))
	assert.Len(t, h.sweeper.batchSweeps, 3)
}

// --- G-M10: the priorTxHash half of the previous round's fix -------------

// TestOnchain_Sweep_GasBumpCarriesPriorTxHash pins G-M10: the 2026-08-26
// round fixed "a restart mid-retry rebroadcasts underpriced" by threading the
// prior transaction's hash into BatchSweep, and the service half of that fix
// had no pin at all -- fakeBatchSweepCall recorded priorTxHash and nothing
// ever asserted it, while both existing revival tests used MaxSweepBumps(0)
// so the gas-bump branch was never executed. Reverting that call site to ""
// left the suite green.
func TestOnchain_Sweep_GasBumpCarriesPriorTxHash(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithToken(chainID, token, "USDT-priorhash", 2)
	h := setupOnchain(t, chains, []string{"USDT-priorhash"},
		service.WithSweepStuckAfter(0), // every tick is "stuck", so bumps are back-to-back
		service.WithMaxSweepBumps(3),
	)
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 8901)
	require.NoError(t, err)
	h.scanner.balances[da.Address] = decimal.NewFromInt(50)

	policy := core.SweepPolicy{
		ChainID:      chainID,
		Token:        token,
		MinThreshold: decimal.NewFromInt(10),
		GasCeiling:   decimal.NewFromInt(100),
		BatchLimit:   10,
		Interval:     time.Millisecond,
	}

	require.NoError(t, h.svc.RunSweepOnce(ctx, policy))
	require.Len(t, h.sweeper.batchSweeps, 1)
	firstHash := h.sweeper.lastTxHash(t, 0)
	assert.Empty(t, h.sweeper.batchSweeps[0].priorTxHash, "the first dispatch replaces nothing")

	sweepUID := h.classificationUID(t, "sweep")
	sent, _, err := h.bookings.ListBookings(ctx, core.BookingFilter{ClassificationUID: sweepUID, Status: "sent", Limit: 10})
	require.NoError(t, err)
	require.Len(t, sent, 1)
	h.reader.setIncluded(chainID, firstHash, false)

	// Bump 1 must carry the first broadcast's hash, so chains/evm's
	// priorFeeFloor can read the fee actually paid off the chain.
	require.NoError(t, h.svc.RunSweepOnce(ctx, policy))
	require.Len(t, h.sweeper.batchSweeps, 2)
	require.Equal(t, firstHash, h.sweeper.batchSweeps[1].priorTxHash,
		"a gas-bump with an empty priorTxHash is the exact regression the 2026-08-26 fix closed")

	secondHash := h.sweeper.lastTxHash(t, 1)
	h.reader.setIncluded(chainID, secondHash, false)

	// Bump 2 must carry bump 1's hash -- i.e. the in-memory tracking takes
	// precedence over the booking's (deliberately unchanged) ChannelRef.
	require.NoError(t, h.svc.RunSweepOnce(ctx, policy))
	require.Len(t, h.sweeper.batchSweeps, 3)
	assert.Equal(t, secondHash, h.sweeper.batchSweeps[2].priorTxHash)
	assert.NotEqual(t, sent[0].ChannelRef, h.sweeper.batchSweeps[2].priorTxHash,
		"the persisted ChannelRef is only the fallback; the live hash wins while this process knows it")
}

// --- B-m7: the watcher is single-flight across replicas ------------------

// TestOnchain_Watch_SkipsWhenAnotherReplicaHoldsTheLock pins B-m7's
// orchestration half (concurrency.md Minor): every replica ran the watcher's
// bare runLoop, each writing back the cursor IT had reached. Re-scanning is
// idempotent, so this was only wasteful -- until I-52 made "the cursor did
// not move" the mechanism that stops a deposit from being lost, at which
// point a competing replica's write is how that refusal gets undone.
func TestOnchain_Watch_SkipsWhenAnotherReplicaHoldsTheLock(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithCeilings(chainID, token, "USDT-leader", 1,
		core.UnboundedAutoCredit, decimal.Zero)
	h := setupOnchain(t, chains, []string{"USDT-leader"},
		service.WithWatchInterval(10*time.Millisecond),
		service.WithRecheckInterval(time.Hour),
		service.WithReorgRecheckInterval(time.Hour),
	)
	ctx := context.Background()

	_, err := h.svc.EnsureDepositAddress(ctx, 9001)
	require.NoError(t, err)
	h.reader.setLatestBlock(chainID, 100)

	lockKey := service.AdvisoryLockKeyForTest(fmt.Sprintf("job:onchain_watch:%d", chainID))
	conn, err := h.pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	var acquired bool
	require.NoError(t, conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", lockKey).Scan(&acquired))
	require.True(t, acquired, "test setup: hold the lock before the watcher starts")
	defer func() {
		var released bool
		_ = conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", lockKey).Scan(&released)
	}()

	runCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	_ = h.svc.Run(runCtx)

	assert.Empty(t, h.reader.fetchWindows(),
		"the watcher must skip its tick entirely while another replica holds the per-chain lock")
}

// TestOnchain_Watch_RunsWhenLockIsFree is the positive contrast: a leader
// election that never elects a leader fails in exactly the same way as no
// election at all (working-agreements §3).
func TestOnchain_Watch_RunsWhenLockIsFree(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithCeilings(chainID, token, "USDT-leaderfree", 1,
		core.UnboundedAutoCredit, decimal.Zero)
	h := setupOnchain(t, chains, []string{"USDT-leaderfree"},
		service.WithWatchInterval(10*time.Millisecond),
		service.WithRecheckInterval(time.Hour),
		service.WithReorgRecheckInterval(time.Hour),
	)
	ctx := context.Background()

	_, err := h.svc.EnsureDepositAddress(ctx, 9002)
	require.NoError(t, err)
	h.reader.setLatestBlock(chainID, 100)

	runCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	_ = h.svc.Run(runCtx)

	assert.NotEmpty(t, h.reader.fetchWindows(), "with no competing holder the watcher must still scan")
}

// onchainDepsWithoutReorgRecorder builds a dependency set with a ChainReader
// but deliberately no ReorgRecorder, to pin Run's refusal (G-M8). Every other
// required dependency is real, so the error under test is the missing
// recorder and not validateCore tripping over something else.
func onchainDepsWithoutReorgRecorder(t *testing.T, pool *pgxpool.Pool) service.OnchainDeps {
	t.Helper()
	bookingStore := postgres.NewBookingStore(pool)
	ledgerStore := postgres.NewLedgerStore(pool)
	classStore := postgres.NewClassificationStore(pool)
	return service.OnchainDeps{
		Registry:            postgres.NewDepositAddressStore(pool),
		Cursors:             postgres.NewChainCursorStore(pool),
		Booker:              bookingStore,
		BookingReader:       bookingStore,
		Journals:            ledgerStore,
		TxComposer:          &testTxComposer{pool: pool, bookingStore: bookingStore, ledgerStore: ledgerStore},
		Reader:              newFakeChainReader(),
		RegistrationRescans: postgres.NewRegistrationRescanStore(pool),
		DeadLetters:         postgres.NewIngestDeadLetterStore(pool),
		Currencies:          postgres.NewCurrencyStore(pool),
		Classifications:     classStore,
		Logger:              newRecordingServiceLogger(),
	}
}

// TestOnchain_Recheck_TxIncludedErrorIsNotEvidence pins the other half of the
// shallow-reorg evidence rule: an RPC that FAILS has said nothing, and "said
// nothing" must never count as "the transaction is gone". A booking whose
// TxIncluded call errors stays where it is, no miss is counted against it,
// and nothing irreversible happens (working-agreements §3: 未运行 ≠ 通过).
func TestOnchain_Recheck_TxIncludedErrorIsNotEvidence(t *testing.T) {
	const (
		chainID       = int64(1)
		token         = "0xusdttoken"
		confirmations = int32(6)
		txHash        = "0xrpcbroken"
	)
	chains := chainSetWithToken(chainID, token, "USDT-rpcerr", confirmations)
	h := setupOnchain(t, chains, []string{"USDT-rpcerr"}, service.WithShallowReorgMisses(2))
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 9101)
	require.NoError(t, err)

	booking, err := h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: txHash, TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("100"),
		Confirmations: 0, BlockNumber: 100,
	})
	require.NoError(t, err)
	require.Equal(t, core.Status("confirming"), booking.Status)

	h.reader.setLatestBlock(chainID, 102) // still below the threshold
	h.reader.setIncludedErr(chainID, txHash, errors.New("rpc: 503 upstream unavailable"))

	for i := 0; i < 5; i++ {
		// The tick itself now REPORTS the RPC failure (M-9: these loops used
		// to swallow every error into a logger that is NopLogger() by
		// default). What must not happen is the booking moving.
		require.Error(t, h.svc.RunPendingRecheckOnce(ctx))
	}

	current, err := h.bookings.GetBooking(ctx, booking.UID)
	require.NoError(t, err)
	assert.Equal(t, core.Status("confirming"), current.Status,
		"an erroring inclusion check must never accumulate into a terminal failed transition")

	open, err := h.reorgs.ListOpenReorgs(ctx, 10)
	require.NoError(t, err)
	assert.Empty(t, open, "and it must not manufacture an anomaly either")

	// Once the RPC recovers and genuinely reports the tx missing, the normal
	// threshold applies -- proving the errors were skipped, not counted.
	h.reader.setIncludedErr(chainID, txHash, nil)
	h.reader.setIncluded(chainID, txHash, false)
	require.NoError(t, h.svc.RunPendingRecheckOnce(ctx))
	afterFirstRealMiss, err := h.bookings.GetBooking(ctx, booking.UID)
	require.NoError(t, err)
	require.Equal(t, core.Status("confirming"), afterFirstRealMiss.Status, "miss 1 of 2")
	require.NoError(t, h.svc.RunPendingRecheckOnce(ctx))
	afterSecondRealMiss, err := h.bookings.GetBooking(ctx, booking.UID)
	require.NoError(t, err)
	assert.Equal(t, core.Status("failed"), afterSecondRealMiss.Status)
}

// --- G-M4, second half: the ceiling must bound the RETRY's bid ----------

// TestOnchain_Sweep_GasBumpRespectsGasCeiling pins the half of G-M4 that is
// not about timing. The stuck path checked Sweeper.GasPrice -- the market
// basis -- against SweepPolicy.GasCeiling, but a replacement transaction has
// to beat what is still pending by the mempool's replacement margin, so what
// it actually bids is max(basis, prior x 1.125). The basis is therefore not
// an upper bound on the bid at all: on the retry path, which is the ONLY path
// where the fee escalates, the gate read as satisfied at every step while the
// bid climbed 1.125^n up the very gas spike that caused the stall.
//
// Here the market price stays at 1 gwei and the ceiling at 100, while the
// replacement quote is 150. The bump must not happen, and the quote must have
// been taken for the same (nonce, priorTxHash) pair BatchSweep would have
// been called with -- a ceiling checked against a different transaction's
// quote would be the same bug with extra steps.
func TestOnchain_Sweep_GasBumpRespectsGasCeiling(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithToken(chainID, token, "USDT-bumpceiling", 2)
	h := setupOnchain(t, chains, []string{"USDT-bumpceiling"},
		service.WithSweepStuckAfter(0), // every tick is "stuck"
		service.WithMaxSweepBumps(3),
	)
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 8951)
	require.NoError(t, err)
	h.scanner.balances[da.Address] = decimal.NewFromInt(50)

	policy := core.SweepPolicy{
		ChainID:      chainID,
		Token:        token,
		MinThreshold: decimal.NewFromInt(10),
		GasCeiling:   decimal.NewFromInt(100),
		BatchLimit:   10,
		Interval:     time.Millisecond,
	}

	require.NoError(t, h.svc.RunSweepOnce(ctx, policy)) // initial broadcast
	require.Len(t, h.sweeper.batchSweeps, 1)
	firstHash := h.sweeper.lastTxHash(t, 0)
	firstNonce := h.sweeper.batchSweeps[0].nonce
	h.reader.setIncluded(chainID, firstHash, false)

	// The market price is still 1 gwei -- far under the ceiling -- but the
	// replacement this bump would broadcast bids 150.
	h.sweeper.mu.Lock()
	h.sweeper.replacementGasPrice = decimal.NewFromInt(150)
	h.sweeper.mu.Unlock()

	require.NoError(t, h.svc.RunSweepOnce(ctx, policy))
	assert.Len(t, h.sweeper.batchSweeps, 1,
		"a gas-bump whose own bid exceeds GasCeiling must not be broadcast, however low the market price is")

	quotes := h.sweeper.replacementQuoteCalls()
	require.NotEmpty(t, quotes, "the ceiling must be checked against the replacement's own quote")
	last := quotes[len(quotes)-1]
	assert.Equal(t, firstNonce, last.nonce, "the quote must be for the nonce the bump would replace")
	assert.Equal(t, firstHash, last.priorTxHash, "the quote must be for the transaction the bump would replace")

	assert.True(t, h.logger(t).contains("gas-bump skipped"),
		"holding a sweep indefinitely because gas is high must be visible, not silent")

	// Contrast: once the replacement bid drops under the ceiling, the bump
	// proceeds -- so the assertion above is about the ceiling and not about
	// the bump path being broken outright.
	h.sweeper.mu.Lock()
	h.sweeper.replacementGasPrice = decimal.NewFromInt(50)
	h.sweeper.mu.Unlock()
	require.NoError(t, h.svc.RunSweepOnce(ctx, policy))
	assert.Len(t, h.sweeper.batchSweeps, 2)
}

// TestOnchain_Sweep_GasBumpFallsBackToChannelRefAfterRestart pins the
// restart half of G-M10. The in-memory hash tracking (o.sweepTx) does not
// survive a process restart; the booking's ChannelRef -- the first
// broadcast's hash -- does. A bump after a restart must therefore pass the
// ChannelRef, because that is the only hash left from which chains/evm's
// priorFeeFloor can rebuild a fee floor high enough to replace whatever is
// genuinely still pending. Passing "" instead is exactly the pre-2026-08-26
// bug: every subsequent bump goes out underpriced, forever.
//
// "A restart" here is a second service.Onchain over the same database and
// the same fakes -- which is what a restart looks like to persisted state.
func TestOnchain_Sweep_GasBumpFallsBackToChannelRefAfterRestart(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithToken(chainID, token, "USDT-bumprestart", 2)
	h := setupOnchain(t, chains, []string{"USDT-bumprestart"},
		service.WithSweepStuckAfter(0),
		service.WithMaxSweepBumps(3),
	)
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 8961)
	require.NoError(t, err)
	h.scanner.balances[da.Address] = decimal.NewFromInt(50)

	policy := core.SweepPolicy{
		ChainID:      chainID,
		Token:        token,
		MinThreshold: decimal.NewFromInt(10),
		GasCeiling:   decimal.NewFromInt(100),
		BatchLimit:   10,
		Interval:     time.Millisecond,
	}

	require.NoError(t, h.svc.RunSweepOnce(ctx, policy))
	require.Len(t, h.sweeper.batchSweeps, 1)

	sweepUID := h.classificationUID(t, "sweep")
	sent, _, err := h.bookings.ListBookings(ctx, core.BookingFilter{ClassificationUID: sweepUID, Status: "sent", Limit: 10})
	require.NoError(t, err)
	require.Len(t, sent, 1)
	channelRef := sent[0].ChannelRef
	require.NotEmpty(t, channelRef, "the first broadcast's hash must be persisted")
	h.reader.setIncluded(chainID, channelRef, false)

	// Restart: a brand-new Onchain, so o.sweepTx and the stuck clock are
	// both empty and the booking's ChannelRef is all that is left.
	restarted := h.rewire(t, chains,
		service.WithSweepStuckAfter(0),
		service.WithMaxSweepBumps(3),
	)

	// The first tick after a restart only starts the stuck clock (this
	// process has no evidence of how long the wait has already been), so it
	// takes two ticks with sweepStuckAfter(0) to reach the bump.
	require.NoError(t, restarted.RunSweepOnce(ctx, policy))
	require.NoError(t, restarted.RunSweepOnce(ctx, policy))
	require.Len(t, h.sweeper.batchSweeps, 2, "the restarted process must still bump a stuck sweep")
	assert.Equal(t, channelRef, h.sweeper.batchSweeps[1].priorTxHash,
		"with no in-memory tracking left, the bump must fall back to the booking's persisted ChannelRef")
}

// TestOnchain_Sweep_StuckTimerIgnoresDatabaseClockSkew pins G-m6. The stuck
// check used to be time.Since(bookings.updated_at): one endpoint written by
// Postgres' clock, the other read from this process's. A few minutes of
// container clock skew shifted the threshold by the same amount, in whichever
// direction the skew went -- and both directions are wrong. A DB clock
// running AHEAD makes time.Since negative, so the sweep is never considered
// stuck and never bumps (the money sits in an unswept deposit address
// indefinitely); a DB clock running BEHIND makes every sweep instantly stuck.
//
// This test forces both skews on the stored row and asserts the decision does
// not move, because both endpoints are now this process's own clock.
func TestOnchain_Sweep_StuckTimerIgnoresDatabaseClockSkew(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
		stuck   = time.Hour // so "instantly stuck" is unmistakable
	)
	chains := chainSetWithToken(chainID, token, "USDT-clockskew", 2)
	h := setupOnchain(t, chains, []string{"USDT-clockskew"},
		service.WithSweepStuckAfter(stuck),
		service.WithMaxSweepBumps(5),
	)
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 8971)
	require.NoError(t, err)
	h.scanner.balances[da.Address] = decimal.NewFromInt(50)

	policy := core.SweepPolicy{
		ChainID:      chainID,
		Token:        token,
		MinThreshold: decimal.NewFromInt(10),
		GasCeiling:   decimal.NewFromInt(100),
		BatchLimit:   10,
		Interval:     time.Millisecond,
	}

	require.NoError(t, h.svc.RunSweepOnce(ctx, policy))
	require.Len(t, h.sweeper.batchSweeps, 1)

	sweepUID := h.classificationUID(t, "sweep")
	sent, _, err := h.bookings.ListBookings(ctx, core.BookingFilter{ClassificationUID: sweepUID, Status: "sent", Limit: 10})
	require.NoError(t, err)
	require.Len(t, sent, 1)
	h.reader.setIncluded(chainID, sent[0].ChannelRef, false)

	// Skew A: the stored updated_at is two hours in the PAST (a DB clock
	// running behind, or simply an old row). Under the old comparison
	// time.Since(updated_at) = 2h > 1h, so this tick would bump immediately.
	setBookingUpdatedAt(t, h.pool, sent[0].UID, -2*time.Hour)
	require.NoError(t, h.svc.RunSweepOnce(ctx, policy))
	assert.Len(t, h.sweeper.batchSweeps, 1,
		"the stuck timer must not be driven by the stored updated_at: nothing has waited an hour yet")

	// Skew B: ten minutes in the FUTURE (a DB clock running ahead). This is
	// the direction that used to make time.Since(updated_at) NEGATIVE, so
	// the sweep was never considered stuck no matter how long it really sat
	// there -- a stuck sweep that could never be bumped. Driven here by an
	// instance whose threshold is zero, so a bump is unambiguously due: the
	// first tick starts this process's clock, the second must bump.
	setBookingUpdatedAt(t, h.pool, sent[0].UID, 10*time.Minute)
	zeroStuck := h.rewire(t, chains, service.WithSweepStuckAfter(0), service.WithMaxSweepBumps(5))
	require.NoError(t, zeroStuck.RunSweepOnce(ctx, policy)) // starts this process's clock
	require.NoError(t, zeroStuck.RunSweepOnce(ctx, policy))
	assert.Len(t, h.sweeper.batchSweeps, 2,
		"a stored updated_at in the future must not be able to suppress the bump forever")
}

// setBookingUpdatedAt forces a booking's stored updated_at to a fixed offset
// from the DATABASE's own now(), simulating clock skew between Postgres and
// this process (G-m6). Written straight through the pool: no store method
// exposes updated_at for writing, and rightly so.
func setBookingUpdatedAt(t *testing.T, pool *pgxpool.Pool, bookingUID string, offset time.Duration) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		"UPDATE bookings SET updated_at = now() + $2::interval WHERE uid = $1::uuid",
		bookingUID, fmt.Sprintf("%d milliseconds", offset.Milliseconds()))
	require.NoError(t, err)
}

// --- F-m10 (runtime half): the INSTALLED deposit lifecycle must be acyclic

// TestOnchain_Run_RefusesCyclicInstalledDepositLifecycle pins the runtime
// half of F-m10 (test-credibility.md Minor).
//
// depositTransitionKey keys Booker.Transition on (booking, to_status) alone.
// That shortcut is sound only because a deposit booking reaches each status
// at most once, and the only thing enforcing it was a unit test over the
// presets.DepositLifecycle Go VARIABLE -- while what this orchestration
// actually drives is whatever lifecycle the consumer installed in the
// database under code='deposit'. Install one with a cycle (a well-meaning
// failed->confirming "retry the reorg" edge is the obvious one, and G-M1's
// own TODO proposed exactly that) and the second visit to a status resolves
// to the FIRST visit's idempotency key: Transition reports success and does
// nothing. On the confirming->confirmed edge that is a deposit which is
// never credited and never errors.
//
// The check has to read the database, because the premise is about installed
// data, not about a Go variable. Delete the validateInstalledDepositLifecycle
// call from Run and this test goes green while that hole reopens.
func TestOnchain_Run_RefusesCyclicInstalledDepositLifecycle(t *testing.T) {
	chains := chainSetWithCeilings(1, "0xusdttoken", "USDT-cyclic", 2, core.UnboundedAutoCredit, decimal.Zero)
	h := setupOnchain(t, chains, []string{"USDT-cyclic"})
	ctx := context.Background()

	// Sanity: as installed by presets, the lifecycle is acyclic and Run's
	// startup checks pass. Without this the assertion below could be
	// satisfied by any unrelated startup failure.
	runCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	require.NoError(t, h.svc.Run(runCtx))

	// Now add the cycle to the row service.Onchain actually reads.
	_, err := h.pool.Exec(ctx, `
		UPDATE classifications
		   SET lifecycle = jsonb_set(
		           lifecycle,
		           '{transitions,failed}',
		           '["confirming"]'::jsonb)
		 WHERE code = 'deposit'`)
	require.NoError(t, err)

	runCtx2, cancel2 := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel2()
	err = h.svc.Run(runCtx2)
	require.Error(t, err, "a cyclic installed deposit lifecycle must refuse to start")
	assert.ErrorIs(t, err, core.ErrInvalidInput)
	assert.Contains(t, err.Error(), "cycle")
	assert.Contains(t, err.Error(), "depositTransitionKey",
		"the error must say WHY a cycle is unacceptable, not just that one exists")
}

// --- G-C2 (service half): both ingestion paths must agree on the key ----

// TestOnchain_Ingest_WatcherAndRescanAgreeOnTheSameTx is the service-side
// pin for G-C2, complementing chains/evm's
// TestReader_FetchDeposits_TxLogSeqIsIndependentOfAddressFilter.
//
// One transaction credits two registered holders. The watcher sees it with
// every address in its filter; a registration rescan sees it with one. With
// TxLogSeq defined as "position among the logs THIS call returned", the
// rescan derived seq=0 for a transfer the watcher had already booked under
// seq=1 -- so IngestDeposit either created a second booking for a transfer
// already credited, or (in the other ordering) hit
// ensureBookingMatchesInput's payload mismatch on a key another holder's
// transfer already owned and dead-lettered a real deposit forever.
//
// Under the corrected definition -- the log's position in its transaction's
// receipt, which no filter can change -- both paths derive the same key, so
// the second observation resolves to the SAME booking. This test states that
// as the property it is: same tx observed twice under different filters =>
// one booking per holder, zero dead letters.
func TestOnchain_Ingest_WatcherAndRescanAgreeOnTheSameTx(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
		txHash  = "0xmultisend"
	)
	chains := chainSetWithCeilings(chainID, token, "USDT-agree", 2, core.UnboundedAutoCredit, decimal.Zero)
	h := setupOnchain(t, chains, []string{"USDT-agree"}, service.WithShallowReorgMisses(3))
	ctx := context.Background()

	addrA, err := h.svc.EnsureDepositAddress(ctx, 7301)
	require.NoError(t, err)
	addrB, err := h.svc.EnsureDepositAddress(ctx, 7302)
	require.NoError(t, err)

	// The receipt-relative positions of the two Transfer logs inside this
	// one transaction. These are properties of the transaction, so both
	// paths below report the same value for the same transfer -- that is
	// precisely what the fix bought.
	sightingA := core.DepositSighting{
		ChainID: chainID, TxHash: txHash, TxLogSeq: 2, Token: token,
		From: "0xmultisender", To: addrA.Address, Amount: decimal.RequireFromString("10"),
		Confirmations: 1, BlockNumber: 500,
	}
	sightingB := core.DepositSighting{
		ChainID: chainID, TxHash: txHash, TxLogSeq: 5, Token: token,
		From: "0xmultisender", To: addrB.Address, Amount: decimal.RequireFromString("20"),
		Confirmations: 1, BlockNumber: 500,
	}

	// Pass 1 -- the watcher, both addresses in one window.
	bookingA, err := h.svc.IngestDeposit(ctx, sightingA)
	require.NoError(t, err)
	require.NotNil(t, bookingA)
	bookingB, err := h.svc.IngestDeposit(ctx, sightingB)
	require.NoError(t, err)
	require.NotNil(t, bookingB)
	require.NotEqual(t, bookingA.UID, bookingB.UID, "two holders, two bookings")

	// Pass 2 -- a registration rescan for holder B alone re-observes the
	// same transaction. Same transfer, same receipt position, therefore the
	// same idempotency key and the same booking.
	again, err := h.svc.IngestDeposit(ctx, sightingB)
	require.NoError(t, err)
	require.NotNil(t, again)
	assert.Equal(t, bookingB.UID, again.UID,
		"the rescan must resolve to the booking the watcher already created, not derive a different key")

	// And holder A's transfer, re-observed by a rescan for A alone.
	againA, err := h.svc.IngestDeposit(ctx, sightingA)
	require.NoError(t, err)
	require.NotNil(t, againA)
	assert.Equal(t, bookingA.UID, againA.UID)

	depositUID := h.classificationUID(t, "deposit")
	bookings, _, err := h.bookings.ListBookings(ctx, core.BookingFilter{ClassificationUID: depositUID, Limit: 50})
	require.NoError(t, err)
	assert.Len(t, bookings, 2, "a transaction crediting two holders must yield exactly two bookings, however often it is observed")
	assert.Zero(t, h.deadLetterCount(t), "no observation of a legitimate deposit may be dead-lettered")

	amounts := map[string]string{}
	for _, b := range bookings {
		amounts[b.UID] = b.Amount.String()
	}
	assert.Equal(t, "10", amounts[bookingA.UID])
	assert.Equal(t, "20", amounts[bookingB.UID])
}

// TestOnchain_Sweep_RevivedDispatchRespectsGasCeiling pins the SIBLING of
// G-M4's ceiling finding, found by scanning for the shape rather than the
// reported instance (contract §0).
//
// The gas-bump path was the reported one. But `advanceSweep`'s pending
// branch is also how a REVIVED booking is dispatched, and a revival keeps
// the failed booking's nonce -- whose slot the chain never freed, which is
// exactly why reviveFailedSweep exists. The adapter therefore still has its
// own record of what it last paid at that nonce (from the bump ladder that
// exhausted itself) and bids >=12.5% over it, while the only gate that had
// run was sweepTick's, comparing the market price before the nonce was even
// known. Same fail-open, different call site.
//
// Here the market price is 1 gwei, the ceiling 100, and the bid for this
// nonce 150. Nothing may be broadcast.
func TestOnchain_Sweep_RevivedDispatchRespectsGasCeiling(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithToken(chainID, token, "USDT-dispatchceiling", 2)
	h := setupOnchain(t, chains, []string{"USDT-dispatchceiling"},
		service.WithSweepStuckAfter(0),
		service.WithMaxSweepBumps(0), // the first stuck check fails the booking outright
	)
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 8981)
	require.NoError(t, err)
	h.scanner.balances[da.Address] = decimal.NewFromInt(50)

	policy := core.SweepPolicy{
		ChainID:      chainID,
		Token:        token,
		MinThreshold: decimal.NewFromInt(10),
		GasCeiling:   decimal.NewFromInt(100),
		BatchLimit:   10,
		Interval:     time.Millisecond,
	}

	// Broadcast, then let it exhaust its (zero) bump budget and go failed.
	require.NoError(t, h.svc.RunSweepOnce(ctx, policy))
	require.Len(t, h.sweeper.batchSweeps, 1)
	firstHash := h.sweeper.lastTxHash(t, 0)
	h.reader.setIncluded(chainID, firstHash, false)
	require.NoError(t, h.svc.RunSweepOnce(ctx, policy)) // bumps >= max -> failed

	sweepUID := h.classificationUID(t, "sweep")
	failed, _, err := h.bookings.ListBookings(ctx, core.BookingFilter{ClassificationUID: sweepUID, Status: "failed", Limit: 10})
	require.NoError(t, err)
	require.Len(t, failed, 1, "the booking must be parked in failed for the revival path to be reached")

	// Gas has spiked for THIS nonce (the exhausted ladder's floor), but the
	// market price the tick-level gate reads is still 1 gwei.
	h.sweeper.mu.Lock()
	h.sweeper.replacementGasPrice = decimal.NewFromInt(150)
	h.sweeper.mu.Unlock()

	require.NoError(t, h.svc.RunSweepOnce(ctx, policy))
	assert.Len(t, h.sweeper.batchSweeps, 1,
		"a revived dispatch whose bid for the reused nonce exceeds GasCeiling must not broadcast")
	assert.True(t, h.logger(t).contains("dispatch skipped"), "and it must say so")

	// Contrast: back under the ceiling, the revival goes out.
	h.sweeper.mu.Lock()
	h.sweeper.replacementGasPrice = decimal.NewFromInt(50)
	h.sweeper.mu.Unlock()
	require.NoError(t, h.svc.RunSweepOnce(ctx, policy))
	assert.Len(t, h.sweeper.batchSweeps, 2)
}
