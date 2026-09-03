// Package service_test: onchain_ops_signals_test.go
//
// The pins for the operability half of
// docs/audits/2026-09-03-independent-review/onchain-ops.md: C-2 (a
// dead-lettered deposit had no counter, no operator surface and no way back
// into the ledger), M-1 (auto_reverse debited a holder on ONE observation
// while the less consequential shallow path required three), M-2 (a token
// whose decimals exceed its currency's exponent dead-letters every deposit
// it ever sees, and nothing said so at startup), M-3 (the one error that
// blocks a chain's collection indefinitely had no counter), M-8
// (chain_cursor_lag_blocks freezes rather than grows under the most common
// watcher stalls) and M-9 (three of the five onchain jobs had no liveness
// and no failure signal at all).
//
// Every pin here drives a real entry point -- RunWatchOnce, RunReorgRecheckOnce,
// ReplayDeadLetter, Run -- against the postgres-backed harness, not a
// hand-assembled store (contract §3 rule 6).
package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

// --- C-2: the dead-letter queue is countable, readable and replayable -----

// TestOnchain_DeadLetter_IsCountedBackloggedAndReplayable is C-2's pin. A
// dead letter means a real, on-chain, already-visible transfer to a
// registered address in a whitelisted token that this ledger decided never
// to book, after which the cursor moved past it: no booking exists, so no
// recheck loop revisits it, and no forward scan will see it again. Its total
// observability used to be one row nothing read plus an Error log line that
// lands in core.NopLogger() unless the consumer injected a logger, and there
// was no path back into the ledger after the cause was fixed.
//
// Four claims, in the order an on-call engineer meets them: it is counted
// with a bounded reason, it shows up as a queue with an age, it can be
// replayed through the real ingestion path once the cause is fixed, and the
// queue then clears itself without anything having to remember to resolve a
// row.
func TestOnchain_DeadLetter_IsCountedBackloggedAndReplayable(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
		txHash  = "0xdeadletterreplay"
	)
	// The chain credits "USDT-late", deliberately NOT registered yet:
	// currencyResolver.resolve returns core.ErrNotFound, which
	// core.IsRetryable classifies as permanent.
	chains := chainSetWithCeilings(chainID, token, "USDT-late", 1,
		decimal.NewFromInt(100_000), decimal.Zero)
	h := setupOnchain(t, chains, []string{"USDT-other"})
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 9101)
	require.NoError(t, err)

	h.reader.setLatestBlock(chainID, 700)
	h.reader.setSightings(chainID, core.DepositSighting{
		ChainID: chainID, TxHash: txHash, TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("7"),
		Confirmations: 0, BlockNumber: 690,
	})

	require.NoError(t, h.svc.RunWatchOnce(ctx, chainID))
	require.Equal(t, int64(700), h.cursorBlock(t, chainID),
		"the cursor is past it -- which is exactly why the row has to be actionable")

	// 1. Counted, with a BOUNDED reason (not the error text, which is
	//    free-form and would blow up Prometheus cardinality).
	require.Equal(t, []deadLetteredCall{{chainID: chainID, reason: "currency_unregistered"}},
		h.metrics.deadLetteredCalls(),
		"a dead letter is the single most payment-affecting event on the pull path; it must be countable")

	// 2. A queue with a depth and an age. Sampled from the reorg-recheck
	//    tick, which is where "is anything still waiting on a human" lives.
	require.NoError(t, h.svc.RunReorgRecheckOnce(ctx))
	backlog := h.metrics.backlogCalls()
	require.NotEmpty(t, backlog, "the backlog must be sampled, or nothing reveals a forgotten queue")
	assert.Equal(t, int64(1), backlog[len(backlog)-1].count)
	assert.Positive(t, backlog[len(backlog)-1].oldestAge,
		"a depth without an age cannot tell an operator's inbox from a forgotten one")

	letters, _, err := h.deadLetters.ListDeadLetters(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, letters, 1)
	assert.False(t, letters[0].Booked, "nothing has credited this deposit yet")
	assert.Equal(t, decimal.RequireFromString("7").String(), letters[0].Sighting.Amount.String(),
		"the row must carry the sighting: it is the only thing left to replay from")

	// 3. Fix the cause, then replay through the REAL ingestion path.
	_, err = h.currencies.CreateCurrency(ctx, core.CurrencyInput{Code: "USDT-late", Name: "USDT-late", Exponent: 18})
	require.NoError(t, err)

	booking, err := h.svc.ReplayDeadLetter(ctx, letters[0].UID)
	require.NoError(t, err, "after the configuration is fixed there must be a way back in")
	require.NotNil(t, booking)
	assert.Equal(t, "deposit-1-"+txHash+"-0", booking.IdempotencyKey,
		"the replay must derive the same idempotency key the watcher would have")

	// Replaying twice is a no-op, not a second booking: IngestDeposit's own
	// idempotency is the whole dedupe mechanism, so nothing has to track
	// whether a replay already happened.
	again, err := h.svc.ReplayDeadLetter(ctx, letters[0].UID)
	require.NoError(t, err)
	assert.Equal(t, booking.UID, again.UID)

	deposits, _, err := h.bookings.ListBookings(ctx, core.BookingFilter{
		ClassificationUID: h.classificationUID(t, "deposit"), Limit: 10,
	})
	require.NoError(t, err)
	assert.Len(t, deposits, 1, "two replays, one booking")

	// 4. The queue clears itself. No resolution column, no operator step,
	//    no alarm nailed to ON.
	replayed, err := h.deadLetters.GetDeadLetter(ctx, letters[0].UID)
	require.NoError(t, err)
	assert.True(t, replayed.Booked, "the row is never rewritten -- 'is it still true' is recomputed from bookings")

	require.NoError(t, h.svc.RunReorgRecheckOnce(ctx))
	backlog = h.metrics.backlogCalls()
	assert.Equal(t, int64(0), backlog[len(backlog)-1].count,
		"a dead letter whose deposit was credited in the end must leave the queue on its own")
}

// TestOnchain_DeadLetterReplay_RefusesWhenThereIsNothingToBook: IngestDeposit
// answers (nil, nil) for a sighting this ledger has no business booking (an
// unregistered address, a token outside the allowlist). On the watcher path
// that is correctly a no-op. For a replay an operator explicitly asked for,
// "nothing happened" must not come back as success (working-agreements §3).
func TestOnchain_DeadLetterReplay_RefusesWhenThereIsNothingToBook(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithToken(chainID, token, "USDT-nobook", 1)
	h := setupOnchain(t, chains, []string{"USDT-nobook"})
	ctx := context.Background()

	// A sighting to an address this ledger never registered, recorded
	// directly: the watcher would never dead-letter this one, which is the
	// point -- the replay path must not assume the row it is handed is
	// bookable.
	sighting := core.DepositSighting{
		ChainID: chainID, TxHash: "0xnotours", TxLogSeq: 0, Token: token,
		From: "0xsender", To: "0x000000000000000000000000000000000000dEaD",
		Amount: decimal.RequireFromString("3"), Confirmations: 3, BlockNumber: 10,
	}
	require.NoError(t, h.deadLetters.RecordDeadLetter(ctx, sighting, "deposit-1-0xnotours-0", "manually recorded fixture"))

	letters, _, err := h.deadLetters.ListDeadLetters(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, letters, 1)

	_, err = h.svc.ReplayDeadLetter(ctx, letters[0].UID)
	require.Error(t, err, "a replay that books nothing must say so")
	assert.ErrorIs(t, err, core.ErrInvalidInput)

	_, err = h.svc.ReplayDeadLetter(ctx, "00000000-0000-0000-0000-000000000000")
	assert.ErrorIs(t, err, core.ErrNotFound, "an unknown uid is not a successful no-op either")
}

// --- M-1: the irreversible half of a deep reorg needs corroboration -------

// TestOnchain_AutoReverse_WaitsForConsecutiveObservations pins M-1. Under
// ReorgPolicyAutoReverse a single TxIncluded=false used to post the reversal
// journal that debits the holder -- with no human in the loop, off one
// answer from the single RPC endpoint chains/evm dials per chain. The
// shallow path, whose consequence (refusing a not-yet-credited deposit) is no
// worse, already demanded three consecutive observations and said why in
// WithShallowReorgMisses' doc comment.
//
// What the threshold must NOT delay is the reporting: the anomaly row and
// the counter fire on the first observation either way, or an operator
// watching for a deep reorg would learn about it late.
func TestOnchain_AutoReverse_WaitsForConsecutiveObservations(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
		txHash  = "0xautoreverse"
	)
	chains := chainSetWithToken(chainID, token, "USDT-ar", 1)
	h := setupOnchain(t, chains, []string{"USDT-ar"},
		service.WithReorgPolicy(core.ReorgPolicyAutoReverse),
		service.WithReorgRecheckWindow(500),
	)
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 9201)
	require.NoError(t, err)

	h.reader.setLatestBlock(chainID, 1000)
	booking, err := h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: txHash, TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("100"),
		Confirmations: 5, BlockNumber: 990,
	})
	require.NoError(t, err)
	require.Equal(t, core.Status("confirmed"), booking.Status)
	require.NotEmpty(t, booking.JournalUID, "the holder's balance has moved -- that is what a reversal takes back")

	audit := postgres.NewAuditStore(h.pool)
	reversals := func() int {
		t.Helper()
		list, err := audit.ListReversals(ctx, booking.JournalUID)
		require.NoError(t, err)
		// ListReversals returns the whole chain including the original.
		return len(list) - 1
	}

	// The node stops reporting the transaction. Two observations: reported
	// both times, and the holder's balance is NOT touched.
	h.reader.setIncluded(chainID, txHash, false)
	for i := 1; i <= 2; i++ {
		require.NoError(t, h.svc.RunReorgRecheckOnce(ctx))
		assert.Zero(t, reversals(), "observation %d of 3 must not debit anybody", i)
	}
	open, err := h.reorgs.ListOpenReorgs(ctx, 10)
	require.NoError(t, err)
	require.Len(t, open, 1, "withholding the DEBIT must not withhold the DETECTION")
	assert.Positive(t, h.metrics.reorgDetections(),
		"the counter is what tells an operator on the first tick; only the reversal waits")
	assert.True(t, h.logger(t).contains("auto-reverse withheld"),
		"a withheld automatic debit must be visible, not merely absent")

	// One corroborating observation resets the streak -- the threshold
	// counts CONSECUTIVE misses, exactly like the shallow path.
	h.reader.setIncluded(chainID, txHash, true)
	require.NoError(t, h.svc.RunReorgRecheckOnce(ctx))
	h.reader.setIncluded(chainID, txHash, false)
	for i := 1; i <= 2; i++ {
		require.NoError(t, h.svc.RunReorgRecheckOnce(ctx))
		assert.Zero(t, reversals(), "the streak restarted, so observation %d must still not debit", i)
	}

	// Third consecutive observation: now the reversal is posted.
	require.NoError(t, h.svc.RunReorgRecheckOnce(ctx))
	assert.Equal(t, 1, reversals(), "three consecutive observations is the configured evidence bar")

	// And it does not keep re-reversing on every later tick.
	require.NoError(t, h.svc.RunReorgRecheckOnce(ctx))
	assert.Equal(t, 1, reversals(), "ReverseJournal's own idempotency guard is the dedupe here")
}

// TestOnchain_AutoReverse_MissThresholdIsConfigurable pins the option half:
// a consumer who wants a different evidence bar sets it, and 1 is a
// deliberate choice they can still make (it is what the code did
// unconditionally before M-1).
func TestOnchain_AutoReverse_MissThresholdIsConfigurable(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
		txHash  = "0xautoreverseone"
	)
	chains := chainSetWithToken(chainID, token, "USDT-ar1", 1)
	h := setupOnchain(t, chains, []string{"USDT-ar1"},
		service.WithReorgPolicy(core.ReorgPolicyAutoReverse),
		service.WithDeepReorgMisses(1),
	)
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 9301)
	require.NoError(t, err)
	h.reader.setLatestBlock(chainID, 1000)
	booking, err := h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: txHash, TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("100"),
		Confirmations: 5, BlockNumber: 990,
	})
	require.NoError(t, err)
	require.NotEmpty(t, booking.JournalUID)

	h.reader.setIncluded(chainID, txHash, false)
	require.NoError(t, h.svc.RunReorgRecheckOnce(ctx))

	list, err := postgres.NewAuditStore(h.pool).ListReversals(ctx, booking.JournalUID)
	require.NoError(t, err)
	assert.Len(t, list, 2, "WithDeepReorgMisses(1) must mean one observation: the original plus its reversal")
}

// --- M-2: decimals x exponent is refused at startup, not per deposit ------

// TestOnchain_Run_RefusesTokenDecimalsAboveCurrencyExponent pins M-2. The
// adapter normalizes a raw amount to TokenConfig.Decimals places --
// correctly, and VerifyTokenDecimals will even confirm the value against the
// contract -- and CreateBooking then refuses it with ErrPrecisionExceeded,
// which is classified permanent, which makes it a dead letter and lets the
// cursor move past. Every deposit of that token with a fraction the currency
// cannot hold is written off, one row at a time, and nothing about the
// configuration looked wrong.
func TestOnchain_Run_RefusesTokenDecimalsAboveCurrencyExponent(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithToken(chainID, token, "USDC-6", 1)
	// The token reports 18 decimals (a real value, as the chain would),
	// while its ledger currency can only represent 6.
	cfg := chains[chainID]
	credit := cfg.CreditTokens[token]
	credit.Decimals = 18
	credit.AutoCreditCeiling = decimal.NewFromInt(1000)
	cfg.CreditTokens[token] = credit
	chains[chainID] = cfg

	h := setupOnchain(t, chains, nil)
	ctx := context.Background()
	_, err := h.currencies.CreateCurrency(ctx, core.CurrencyInput{Code: "USDC-6", Name: "USDC-6", Exponent: 6})
	require.NoError(t, err)

	runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	err = h.rewire(t, chains).Run(runCtx)
	require.Error(t, err, "a configuration that dead-letters every fractional deposit must not start")
	assert.ErrorIs(t, err, core.ErrInvalidInput)
	assert.Contains(t, err.Error(), "exponent=6")
	assert.Contains(t, err.Error(), "dead-lettered")
}

// TestTokenConfig_Validate_RefusesDecimalsNoCurrencyCouldRepresent is the
// half that needs no database, and so still protects a webhook-only consumer
// that never calls Run: a currency's exponent caps at 18
// (core.CurrencyInput.Validate), so a token above that has no possible
// currency. This used to be capped at 36 as "generous headroom", which is
// how the two limits came to disagree.
func TestTokenConfig_Validate_RefusesDecimalsNoCurrencyCouldRepresent(t *testing.T) {
	require.NoError(t, core.TokenConfig{Decimals: 18}.Validate(), "18 is the highest usable value, not an error")

	err := core.TokenConfig{Decimals: 19}.Validate()
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)
	assert.Contains(t, err.Error(), "dead-lettered")
}

// --- M-8: cursor liveness survives the failures the lag gauge cannot see --

// TestOnchain_ChainCursorAdvanceAge_GrowsWhenTheTipIsUnreachable pins M-8.
// chain_cursor_lag_blocks can only be computed once LatestBlock has
// answered, so an RPC outage -- the single most common watcher stall --
// leaves it FROZEN at its last reading, which is indistinguishable from a
// healthy watcher. RUNBOOK §14 used to teach an alert on that gauge
// climbing, which is the one thing it cannot do in this scenario.
func TestOnchain_ChainCursorAdvanceAge_GrowsWhenTheTipIsUnreachable(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithToken(chainID, token, "USDT-lag", 1)
	h := setupOnchain(t, chains, []string{"USDT-lag"})
	ctx := context.Background()

	// One healthy tick, so there is a lag reading to freeze.
	h.reader.setLatestBlock(chainID, 500)
	require.NoError(t, h.svc.RunWatchOnce(ctx, chainID))
	require.NotEmpty(t, h.metrics.cursorLagCalls())
	lagReadings := len(h.metrics.cursorLagCalls())

	// The provider goes away.
	h.reader.setLatestBlockErr(chainID, errors.New("dial tcp: connection refused"))
	time.Sleep(5 * time.Millisecond)
	require.Error(t, h.svc.RunWatchOnce(ctx, chainID))
	time.Sleep(5 * time.Millisecond)
	require.Error(t, h.svc.RunWatchOnce(ctx, chainID))

	assert.Len(t, h.metrics.cursorLagCalls(), lagReadings,
		"the lag gauge cannot be reported at all here -- that is the gap this pin is about")

	ages := h.metrics.cursorAgeCalls()
	require.GreaterOrEqual(t, len(ages), 3, "the advance age must be reported on EVERY tick, including the failing ones")
	assert.Greater(t, ages[len(ages)-1].age, ages[0].age,
		"with the cursor unable to move, the age must grow -- this is the watcher-liveness signal")
	assert.Equal(t, chainID, ages[len(ages)-1].chainID)
}

// --- M-9: every onchain job reports whether its tick ran ------------------

// TestOnchain_Run_EveryJobReportsItsTicks pins M-9. Onchain keeps its own
// runLoop rather than service.Worker's, and that loop emitted ONLY
// JobPanicked: JobTickCompleted/JobTickFailed came from LockedJob.Run, which
// wraps just the watch and sweep jobs. So `onchain_recheck` -- the loop that
// actually credits pull-path deposits -- `onchain_reorg_recheck` (the only
// reorg detector) and `onchain_registration_rescan` had NO liveness signal
// and NO failure signal, with every internal error going to a logger that is
// NopLogger() by default. `increase(job_tick_completed_total{job=...}) == 0`,
// the alert core.Metrics' own doc comment recommends, was structurally
// unable to fire for three of the five jobs.
//
// Driven through Run, not through runLoop directly: the regression this
// catches is a job wired without counting, which only Run can get wrong.
func TestOnchain_Run_EveryJobReportsItsTicks(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithCeilings(chainID, token, "USDT-jobs", 1,
		decimal.NewFromInt(100_000), decimal.Zero)
	h := setupOnchain(t, chains, []string{"USDT-jobs"},
		service.WithWatchInterval(20*time.Millisecond),
		service.WithRecheckInterval(20*time.Millisecond),
		service.WithReorgRecheckInterval(20*time.Millisecond),
	)
	h.reader.setLatestBlock(chainID, 100)

	// The registration-rescan loop ticks every second (not configurable), so
	// the window has to outlast one of its ticks.
	runCtx, cancel := context.WithTimeout(context.Background(), 1300*time.Millisecond)
	defer cancel()
	require.NoError(t, h.svc.Run(runCtx), "Run exits cleanly on context cancellation")

	completed, failed := h.metrics.tickCounts()
	for _, job := range []string{
		"onchain_recheck",
		"onchain_reorg_recheck",
		"onchain_registration_rescan",
		"onchain_watch:1",
	} {
		assert.Positive(t, completed[job]+failed[job],
			"job %q must report the outcome of its ticks -- otherwise a stalled loop looks exactly like an idle one", job)
	}
	// The watch job's label must be the LockedJob's, so the same job is not
	// filed under two names across the job_tick_* and job_panicked families.
	assert.Zero(t, completed["onchain_watch"]+failed["onchain_watch"],
		"the watcher counts under its per-chain label, not under a second bare one")
}

// --- money-out C-2: a booking row is not evidence ------------------------

// TestOnchain_Recheck_ForgedBookingIsNotCredited pins money-out C-2
// (docs/audits/2026-09-03-independent-review/money-out.md). The recheck loop
// derived `confirmations = latest - metadata.block_number + 1` from the
// booking's own row and, once that cleared the threshold, credited
// `bookings.amount` -- consulting the chain ONLY on the other branch, while
// the booking was still below the threshold. So one INSERT that `ledger_app`
// is allowed to make, naming a transaction that does not exist, was signed
// out by the honest recheck job into a real credit:
//
//	status=confirmed  journal_uid=...  auth_status=signed
//	VerifiedBalance(main_wallet) = 999   (there was never a deposit)
//
// None of the existing controls covered it: I-21's trust boundary is about
// the SIGHTING's provenance, I-25's immutable columns are UPDATE semantics,
// I-3's idempotency says the key is unique rather than real, and P5
// faithfully signed the amount the row claimed.
//
// The fix is not to trust the row: before crediting, the chain is re-read
// for that block and must produce a log with this tx hash, log position,
// token, amount and a recipient registered to this holder. What it cannot
// produce, it does not credit -- it parks for a human (I-69).
func TestOnchain_Recheck_ForgedBookingIsNotCredited(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithCeilings(chainID, token, "USDT-forge", 1,
		decimal.NewFromInt(100_000), decimal.Zero)
	h := setupOnchain(t, chains, []string{"USDT-forge"})
	ctx := context.Background()

	// A registered holder, so the forged row looks entirely ordinary.
	da, err := h.svc.EnsureDepositAddress(ctx, 9401)
	require.NoError(t, err)
	require.NotEmpty(t, da.Address)

	currencies, err := h.currencies.ListCurrencies(ctx, false)
	require.NoError(t, err)
	require.Len(t, currencies, 1)

	// The attack, as the audit ran it: one INSERT, no chain event anywhere.
	// Written with the migration credential here only because the
	// testcontainer has no separate ledger_app connection -- the audit
	// proved the grant, this pin is about what the honest job does next.
	//
	// `pending`, not `confirming`: migration 029's BEFORE INSERT guard
	// constrains an appended booking to its lifecycle's INITIAL status, for
	// every role. That narrows the attack without closing it -- the recheck
	// loop scans `pending` and `confirming` alike, and a low `block_number`
	// clears the confirmation threshold from either -- which is precisely
	// the residual surface 029's own header admits to and this pin covers.
	var forgedUID string
	err = h.pool.QueryRow(ctx, `
		INSERT INTO bookings (classification_id, account_holder, currency_id, amount, status,
		                      channel_name, channel_ref, idempotency_key, metadata, uid)
		VALUES ((SELECT id FROM classifications WHERE code = 'deposit'),
		        9401,
		        (SELECT id FROM currencies WHERE code = 'USDT-forge'),
		        999, 'pending', 'onchain', '0xforged#0', 'deposit-1-0xforged-0',
		        '{"chain_id":"1","tx_hash":"0xforged","txlog_seq":"0","token":"`+token+`","block_number":"1"}',
		        gen_random_uuid())
		RETURNING uid`).Scan(&forgedUID)
	require.NoError(t, err, "the INSERT itself is not what this pin is about -- migration 029 constrains its shape and audits it")

	// The chain head is far past the forged block_number, so the row's own
	// arithmetic says "7-plus confirmations, credit it".
	h.reader.setLatestBlock(chainID, 500)

	// Below the corroboration threshold the booking simply does not move.
	// Nothing is credited on one disagreement either -- the threshold decides
	// when a disagreement becomes an operator's problem, not whether to act
	// on it.
	for i := 1; i < 3; i++ {
		require.NoError(t, h.svc.RunPendingRecheckOnce(ctx))
		held, err := h.bookings.GetBooking(ctx, forgedUID)
		require.NoError(t, err)
		require.NotEqual(t, core.Status("confirmed"), held.Status, "observation %d must not credit anything", i)
		require.Empty(t, held.JournalUID, "no journal may exist at any point in this test")
	}

	require.NoError(t, h.svc.RunPendingRecheckOnce(ctx))

	after, err := h.bookings.GetBooking(ctx, forgedUID)
	require.NoError(t, err)
	assert.Equal(t, core.Status("review"), after.Status,
		"a booking the chain does not corroborate must be parked for a human, never credited")
	assert.Empty(t, after.JournalUID, "and no journal may be posted for it (I-21)")
	assert.Equal(t, "onchain_unverified", after.Metadata["review_reason"])
	assert.True(t, h.logger(t).contains("deposit.onchain_unverified"),
		"the refusal must name itself -- it is the only trace an operator gets")
}

// TestOnchain_Recheck_CorroborationRejectsATamperedAmount is the other half:
// referencing a transaction that DOES exist does not buy an attacker the
// amount of their choosing. The audit called this out explicitly as the
// limit of a bare TxIncluded check ("攻击者可以引用一笔真实存在的 tx hash 并
// 自定 amount"), which is why the re-read compares the log's amount, token
// and recipient rather than only its existence.
func TestOnchain_Recheck_CorroborationRejectsATamperedAmount(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
		txHash  = "0xrealtransfer"
	)
	chains := chainSetWithCeilings(chainID, token, "USDT-tamper", 1,
		decimal.NewFromInt(100_000), decimal.Zero)
	h := setupOnchain(t, chains, []string{"USDT-tamper"}, service.WithConfirmationEvidenceMisses(1))
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 9501)
	require.NoError(t, err)

	// A real transfer of 10, on chain, to this holder's own address.
	h.reader.setLatestBlock(chainID, 500)
	h.reader.setIncluded(chainID, txHash, true)
	h.reader.setSightings(chainID, core.DepositSighting{
		ChainID: chainID, TxHash: txHash, TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("10"),
		Confirmations: 0, BlockNumber: 400,
	})

	// The booking claims 10000 for the same log. `pending` for the same
	// reason as the pin above: migration 029's INSERT guard allows only the
	// lifecycle's initial status, and the recheck loop drives both.
	var forgedUID string
	err = h.pool.QueryRow(ctx, `
		INSERT INTO bookings (classification_id, account_holder, currency_id, amount, status,
		                      channel_name, channel_ref, idempotency_key, metadata, uid)
		VALUES ((SELECT id FROM classifications WHERE code = 'deposit'),
		        9501,
		        (SELECT id FROM currencies WHERE code = 'USDT-tamper'),
		        10000, 'pending', 'onchain', '`+txHash+`#0', 'deposit-1-`+txHash+`-0',
		        '{"chain_id":"1","tx_hash":"`+txHash+`","txlog_seq":"0","token":"`+token+`","block_number":"400"}',
		        gen_random_uuid())
		RETURNING uid`).Scan(&forgedUID)
	require.NoError(t, err)

	require.NoError(t, h.svc.RunPendingRecheckOnce(ctx))

	after, err := h.bookings.GetBooking(ctx, forgedUID)
	require.NoError(t, err)
	assert.Equal(t, core.Status("review"), after.Status,
		"a real tx hash must not launder an amount the log does not carry")
	assert.Empty(t, after.JournalUID)
	assert.Equal(t, "onchain_unverified", after.Metadata["review_reason"])
}

// TestOnchain_Recheck_CorroborationFailureIsNotAVerdict: an RPC that fails
// has said nothing. It must not park a legitimate deposit for review any
// more than it may credit a forged one -- the booking stays put, and the
// tick reports the failure so the loop's health is visible (M-9).
func TestOnchain_Recheck_CorroborationFailureIsNotAVerdict(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
		txHash  = "0xrpcflaky"
	)
	chains := chainSetWithCeilings(chainID, token, "USDT-flaky", 1,
		decimal.NewFromInt(100_000), decimal.Zero)
	h := setupOnchain(t, chains, []string{"USDT-flaky"}, service.WithConfirmationEvidenceMisses(1))
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 9601)
	require.NoError(t, err)

	h.reader.setLatestBlock(chainID, 500)
	booking, err := h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: txHash, TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("100"),
		Confirmations: 0, BlockNumber: 400,
	})
	require.NoError(t, err)
	require.Equal(t, core.Status("confirming"), booking.Status)

	h.reader.setIncluded(chainID, txHash, true)
	h.reader.setFetchErr(errors.New("rpc: 429 too many requests"))

	require.Error(t, h.svc.RunPendingRecheckOnce(ctx), "an unanswerable re-read is a failed tick, not a silent skip")
	held, err := h.bookings.GetBooking(ctx, booking.UID)
	require.NoError(t, err)
	assert.Equal(t, core.Status("confirming"), held.Status,
		"the deposit is real; a broken RPC must not park it for review")
	assert.Empty(t, held.JournalUID, "and it must not be credited on unread evidence either")

	// Once the chain can answer, the same deposit confirms normally.
	h.reader.setFetchErr(nil)
	h.reader.setSightings(chainID, core.DepositSighting{
		ChainID: chainID, TxHash: txHash, TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("100"),
		Confirmations: 0, BlockNumber: 400,
	})
	require.NoError(t, h.svc.RunPendingRecheckOnce(ctx))
	confirmed, err := h.bookings.GetBooking(ctx, booking.UID)
	require.NoError(t, err)
	assert.Equal(t, core.Status("confirmed"), confirmed.Status)
	assert.NotEmpty(t, confirmed.JournalUID)
}
