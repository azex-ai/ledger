package service_test

// Pins for docs/INVARIANTS.md I-67 (the forward scan's lookback) and the
// application half of the 2026-09-03 independent review's onchain-ops C-1.
//
// chain_cursors.last_scanned_block is the only state deciding which on-chain
// money the ledger is ever told about. Migration 029 made a forged advance
// bounded and audited; these pins cover what the scanner itself must do about
// one that got through anyway.

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/service"
)

// TestOnchain_Watch_LookbackRecoversAForgedCursorAdvance is the review's
// scenario end to end: a deposit is confirmed on chain, someone moves the
// cursor past it before the scanner gets there, and the deposit must still be
// booked.
//
// Before the lookback, `from` was cursor+1 and nothing ever looked below the
// cursor again -- scanChainOnce's own I-52 comment says so -- so the deposit
// left no booking, no event, no journal and no entry. Nothing in the sixteen
// reconciliation checks can see a deposit the ledger was never told about,
// and solvency reads HEALTHIER for it (the liability side is the one that is
// missing) while the funds are still swept into treasury.
func TestOnchain_Watch_LookbackRecoversAForgedCursorAdvance(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithCeilings(chainID, token, "USDT-lookback", 1,
		decimal.NewFromInt(100_000), decimal.Zero)
	h := setupOnchain(t, chains, []string{"USDT-lookback"}, service.WithRescanLookback(64))
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 9101)
	require.NoError(t, err)

	// A real, confirmed deposit in block 950.
	h.reader.setLatestBlock(chainID, 1000)
	h.reader.setSightings(chainID, core.DepositSighting{
		ChainID: chainID, TxHash: "0xskippedtx", TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("42"),
		Confirmations: 50, BlockNumber: 950,
	})

	// The forged advance: the cursor is pushed past that block before the
	// scanner ever covered it. This is the residual migration 029 cannot
	// reach -- 029 bounds how far one statement may skip and records that it
	// happened, but a skip inside the bound is still a skip, and nothing in
	// the database can un-skip it.
	require.NoError(t, h.cursors.SetCursor(ctx, chainID, 990))

	require.NoError(t, h.svc.RunWatchOnce(ctx, chainID))

	// The load-bearing assertion is the WINDOW, stated first and deliberately.
	// fakeChainReader.FetchDeposits returns its configured sightings whatever
	// range it is asked about, so the booking assertions below would pass
	// even with the lookback removed; measured (with the lookback stubbed
	// out, `from` came back as 991 and only this line went red).
	windows := h.reader.fetchWindows()
	require.NotEmpty(t, windows)
	assert.LessOrEqual(t, windows[0].fromBlock, int64(950),
		"the tick must have re-covered the skipped block, not merely reported a lag")

	bookings, _, err := h.bookings.ListBookings(ctx, core.BookingFilter{
		ClassificationUID: h.classificationUID(t, "deposit"), Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, bookings, 1,
		"the deposit in the skipped window must still be booked -- without the lookback `from` is cursor+1 = 991 and block 950 is never read again")
	assert.Equal(t, core.Status("confirmed"), bookings[0].Status)
	assert.True(t, bookings[0].Amount.Equal(decimal.NewFromInt(42)))

	// Re-running is free: the idempotency key absorbs the duplicate, which is
	// what makes a permanent lookback affordable at all (I-3).
	require.NoError(t, h.svc.RunWatchOnce(ctx, chainID))
	bookings, _, err = h.bookings.ListBookings(ctx, core.BookingFilter{
		ClassificationUID: h.classificationUID(t, "deposit"), Limit: 10,
	})
	require.NoError(t, err)
	assert.Len(t, bookings, 1, "re-scanning the same window must not book the same transfer twice")
}

// TestOnchain_Watch_CursorAheadOfTheChainIsLoudAndStillScans covers the other
// shape the review measured -- `UPDATE chain_cursors SET last_scanned_block =
// 99999999`, far past the chain head.
//
// That used to wedge the scanner completely: `safeTip < from` was true on
// every future tick, so it returned nil having done nothing, forever, and the
// only signal was a ChainCursorLag that had gone NEGATIVE. Two things are
// required now -- say so (working-agreements.md §3: a step that did nothing
// must be distinguishable from one that worked), and keep scanning the
// lookback window so new deposits are still ingested while an operator
// rewinds the cursor.
func TestOnchain_Watch_CursorAheadOfTheChainIsLoudAndStillScans(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithCeilings(chainID, token, "USDT-ahead", 1,
		decimal.NewFromInt(100_000), decimal.Zero)
	h := setupOnchain(t, chains, []string{"USDT-ahead"}, service.WithRescanLookback(64))
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, 9201)
	require.NoError(t, err)

	h.reader.setLatestBlock(chainID, 1000)
	h.reader.setSightings(chainID, core.DepositSighting{
		ChainID: chainID, TxHash: "0xstilltx", TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.RequireFromString("7"),
		Confirmations: 20, BlockNumber: 980,
	})

	// Seeded through the store the way an operator's UPDATE would land it.
	require.NoError(t, h.cursors.SetCursor(ctx, chainID, 99999999))

	require.NoError(t, h.svc.RunWatchOnce(ctx, chainID))

	assert.True(t, h.logger(t).contains("cursor is ahead of the chain head"),
		"a cursor this scanner cannot have written must be reported, not silently obeyed")

	bookings, _, err := h.bookings.ListBookings(ctx, core.BookingFilter{
		ClassificationUID: h.classificationUID(t, "deposit"), Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, bookings, 1,
		"a cursor past the chain head must not stop ingestion of blocks that are actually on chain")

	// The cursor is not dragged backwards by the lookback: SetChainCursor is
	// monotonic on purpose (a lagging replica must not rewind it), and
	// undoing this one is the operator's owner-only rewind, not the
	// scanner's.
	assert.Equal(t, int64(99999999), h.cursorBlock(t, chainID))
}

// TestOnchain_Watch_LookbackDisabledKeepsTheOldWindow pins that the lookback
// is a knob and not a hidden behaviour change: with it off, the scan window
// is exactly cursor+1..safeTip, which is what every I-52/I-53 pin next door
// asserts.
func TestOnchain_Watch_LookbackDisabledKeepsTheOldWindow(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
	)
	chains := chainSetWithCeilings(chainID, token, "USDT-nolookback", 1,
		decimal.NewFromInt(100_000), decimal.Zero)
	h := setupOnchain(t, chains, []string{"USDT-nolookback"}, service.WithRescanLookback(0))
	ctx := context.Background()

	_, err := h.svc.EnsureDepositAddress(ctx, 9301)
	require.NoError(t, err)

	h.reader.setLatestBlock(chainID, 1000)
	require.NoError(t, h.cursors.SetCursor(ctx, chainID, 900))
	require.NoError(t, h.svc.RunWatchOnce(ctx, chainID))

	windows := h.reader.fetchWindows()
	require.NotEmpty(t, windows)
	assert.Equal(t, int64(901), windows[0].fromBlock)
	assert.Equal(t, int64(1000), windows[0].toBlock)
}
