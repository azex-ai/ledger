// Package service_test: onchain_duplicate_deposit_test.go
//
// money-out N-1 (2026-09-03 independent review, re-check round) and its
// neighbour N-2. One on-chain transfer log may be booked once; a booking
// that is not corroborated must reach the review queue even when the log it
// names belongs to somebody else. docs/INVARIANTS.md I-71.
package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/service"
)

// dropDepositIdentityFence removes migration 032's index, putting the schema
// back in the shape every deployment had before it.
//
// Used to prove the two halves of the fence are INDEPENDENTLY load-bearing:
// the application check (corroborateBeforeConfirm's already-booked lookup)
// must refuse to credit a duplicate even where the index does not exist --
// on a deployment mid-migration, on one that has not upgraded, and in the
// window between a consumer's INSERT and their next migration run. Issued as
// the migration credential, which is what a schema change is.
func dropDepositIdentityFence(t *testing.T, h *onchainHarness) {
	t.Helper()
	_, err := h.pool.Exec(context.Background(), "DROP INDEX uq_bookings_deposit_identity")
	require.NoError(t, err)
}

// insertDuplicateDepositBookings runs money-out N-1's attack statement
// verbatim in shape: one INSERT ... SELECT generate_series copying an
// already-booked on-chain log into `count` further bookings. Everything the
// honest writer would have written is copied faithfully -- same amount, same
// holder, same chain_id/tx_hash/txlog_seq/token/block_number -- because that
// is what makes the attack work.
//
// Each row gets a DISTINCT channel_name (`<channelName>-1`, `-2`, ...), which
// is the detail that makes N-1 different from the variant
// uq_bookings_channel_ref happens to block: that index is
// UNIQUE (channel_name, channel_ref), so rows differing in the first key
// column never collide, however identical everything that matters is.
// channel_ref is left empty as well, which opts out of the partial index
// entirely. Returns the error, if any.
func insertDuplicateDepositBookings(ctx context.Context, h *onchainHarness, holder int64, currencyCode, token, txHash string, amount decimal.Decimal, channelName string, count int) error {
	_, err := h.pool.Exec(ctx, `
		INSERT INTO bookings (classification_id, account_holder, currency_id, amount, status,
		                      channel_name, channel_ref, idempotency_key, metadata, uid)
		SELECT (SELECT id FROM classifications WHERE code = 'deposit'),
		       $1,
		       (SELECT id FROM currencies WHERE code = $2),
		       $3, 'pending',
		       $4::text || '-' || g, '', 'r3-dup-' || g,
		       jsonb_build_object('chain_id', '1', 'tx_hash', $5::text, 'txlog_seq', '0',
		                          'token', $6::text, 'block_number', '100'),
		       gen_random_uuid()
		FROM generate_series(1, $7) g`,
		holder, currencyCode, amount, channelName, txHash, token, count)
	return err
}

// realDepositHarness ingests one genuine deposit of 50 and hands back the
// harness plus that booking -- the "anchor" N-1 needs: a real, confirmed,
// verifiable transfer whose log the duplicates then copy.
func realDepositHarness(t *testing.T, chainID int64, token, currency, txHash string, holder int64) (*onchainHarness, *core.Booking, string) {
	t.Helper()
	chains := chainSetWithCeilings(chainID, token, currency, 1,
		decimal.NewFromInt(100_000), decimal.Zero)
	h := setupOnchain(t, chains, []string{currency})
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, holder)
	require.NoError(t, err)

	h.reader.setLatestBlock(chainID, 500)
	h.reader.setIncluded(chainID, txHash, true)
	h.reader.setSightings(chainID, core.DepositSighting{
		ChainID: chainID, TxHash: txHash, TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.NewFromInt(50),
		Confirmations: 0, BlockNumber: 100,
	})

	booking, err := h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: txHash, TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.NewFromInt(50),
		Confirmations: 5, BlockNumber: 100,
	})
	require.NoError(t, err)
	require.Equal(t, core.Status("confirmed"), booking.Status, "the anchor deposit is real and credited")
	require.NotEmpty(t, booking.JournalUID)
	return h, booking, da.Address
}

// TestDepositIdentity_DuplicateBookingsAreRejectedAtInsert pins the
// structural half of I-71: migration 032's unique index on the deposit's real
// identity, which is the (chain, transaction, log position) triple the row
// already carries.
//
// N-1's INSERT chose its own `channel_name` and left `channel_ref` empty --
// the two columns `uq_bookings_channel_ref` keys on -- so the only unique
// constraint that happened to cover this was opted out of. The new index
// keys on nothing the INSERT gets to choose.
func TestDepositIdentity_DuplicateBookingsAreRejectedAtInsert(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
		txHash  = "0xr3dup"
		holder  = int64(9701)
	)
	h, anchor, _ := realDepositHarness(t, chainID, token, "USDT-dup", txHash, holder)
	ctx := context.Background()

	// The attack exactly as reported: distinct channel names, empty
	// channel_ref -- past uq_bookings_channel_ref in both of the ways it can
	// be stepped around. The identity index is keyed on none of that.
	err := insertDuplicateDepositBookings(ctx, h, holder, "USDT-dup", token, txHash, decimal.NewFromInt(50), "r3-dup", 3)
	require.Error(t, err, "a second booking for one transfer must be impossible, not merely unusual")
	assert.Contains(t, err.Error(), "uq_bookings_deposit_identity")

	// And under the honest channel name too, since the constraint is about
	// the deposit's identity and not about who wrote the row.
	_, err = h.pool.Exec(ctx, `
		INSERT INTO bookings (classification_id, account_holder, currency_id, amount, status,
		                      channel_name, channel_ref, idempotency_key, metadata, uid)
		VALUES ((SELECT id FROM classifications WHERE code = 'deposit'), $1,
		        (SELECT id FROM currencies WHERE code = 'USDT-dup'), 50, 'pending',
		        'onchain', '', 'r3-dup-onchain',
		        jsonb_build_object('chain_id','1','tx_hash',$2::text,'txlog_seq','0','token',$3::text,'block_number','100'),
		        gen_random_uuid())`,
		holder, txHash, token)
	require.Error(t, err, "a second booking for one transfer must be impossible, not merely unusual")
	assert.Contains(t, err.Error(), "uq_bookings_deposit_identity")

	// The honest deposit is untouched, and is still the only booking.
	deposits, _, err := h.bookings.ListBookings(ctx, core.BookingFilter{
		ClassificationUID: h.classificationUID(t, "deposit"), Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, deposits, 1)
	assert.Equal(t, anchor.UID, deposits[0].UID)

	// The index does not stand in the way of a SECOND log in the same
	// transaction -- one transaction can credit several registered addresses,
	// which is why txlog_seq is part of the identity in the first place
	// (I-20).
	_, err = h.pool.Exec(ctx, `
		INSERT INTO bookings (classification_id, account_holder, currency_id, amount, status,
		                      channel_name, channel_ref, idempotency_key, metadata, uid)
		VALUES ((SELECT id FROM classifications WHERE code = 'deposit'), $1,
		        (SELECT id FROM currencies WHERE code = 'USDT-dup'), 50, 'pending',
		        'onchain', $2, 'honest-second-log',
		        jsonb_build_object('chain_id','1','tx_hash',$3::text,'txlog_seq','1','token',$4::text,'block_number','100'),
		        gen_random_uuid())`,
		holder, txHash+"#1", txHash, token)
	require.NoError(t, err, "a different log position in the same transaction is a different deposit")
}

// TestDepositIdentity_DuplicateBookingsAreNotCreditedWithoutTheIndex pins the
// application half of I-71, on the schema every deployment had before
// migration 032: even where nothing stops the rows existing, the honest
// recheck job must not credit them.
//
// This is the finding as measured. Three faithful copies of one real deposit
// were all confirmed and signed, verified balance went 50 -> 200, and
// solvency plus full reconciliation stayed green throughout -- because both
// sides of the accounting equation grew together and every check the ledger
// had was asking the chain "is this transfer real?" rather than asking
// itself "have I already counted it?".
func TestDepositIdentity_DuplicateBookingsAreNotCreditedWithoutTheIndex(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
		txHash  = "0xr3dupnoidx"
		holder  = int64(9801)
	)
	h, anchor, _ := realDepositHarness(t, chainID, token, "USDT-dup2", txHash, holder)
	ctx := context.Background()

	dropDepositIdentityFence(t, h)

	require.NoError(t, insertDuplicateDepositBookings(ctx, h, holder, "USDT-dup2", token, txHash, decimal.NewFromInt(50), "r3-dup", 3),
		"pre-032 schema: the rows go in, which is the premise of this half of the pin")

	// The honest job runs, unchanged, as many times as the threshold needs.
	for i := 0; i < 4; i++ {
		require.NoError(t, h.svc.RunPendingRecheckOnce(ctx))
	}

	deposits, _, err := h.bookings.ListBookings(ctx, core.BookingFilter{
		ClassificationUID: h.classificationUID(t, "deposit"), Limit: 20,
	})
	require.NoError(t, err)
	require.Len(t, deposits, 4, "one real deposit plus its three copies")

	credited := 0
	for _, b := range deposits {
		if b.UID == anchor.UID {
			assert.Equal(t, core.Status("confirmed"), b.Status, "the real deposit is unaffected")
			assert.NotEmpty(t, b.JournalUID)
			continue
		}
		assert.Equal(t, core.Status("review"), b.Status,
			"a duplicate must be parked for a human, never credited (booking %s)", b.UID)
		assert.Empty(t, b.JournalUID, "and no journal may be posted for it")
		assert.Equal(t, "onchain_unverified", b.Metadata["review_reason"])
		if b.JournalUID != "" {
			credited++
		}
	}
	assert.Zero(t, credited, "the chain moved 50 once; the ledger may credit it once")

	assert.True(t, h.logger(t).contains("already holds transfer"),
		"the refusal must name the booking that already holds the log -- that is what an operator needs")
}

// TestDepositIdentity_UncorroboratedBookingReachesReviewEvenWhenTheLogIsTaken
// pins N-2. The most natural forgery references a transfer that a REAL
// booking already holds and inflates the amount; corroboration correctly
// calls that a contradiction, and then the walk to review used the log's own
// channel_ref -- which the real booking owns, and which is unique per
// channel. The transition failed, routeToReview was never reached, and the
// booking sat in `pending` forever emitting one Error line per tick, with
// `review_reason` never landing and the review metric never firing. I-69
// promises "a queue an operator works, not a log line"; this is the case
// where it was exactly a log line.
//
// Runs on the pre-032 schema for the same reason the pin above does: after
// 032 the row cannot be inserted at all, and the two halves must each hold.
func TestDepositIdentity_UncorroboratedBookingReachesReviewEvenWhenTheLogIsTaken(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
		txHash  = "0xr3taken"
		holder  = int64(9901)
	)
	h, anchor, _ := realDepositHarness(t, chainID, token, "USDT-taken", txHash, holder)
	ctx := context.Background()

	dropDepositIdentityFence(t, h)

	// Same log as the real booking, amount inflated 50 -> 5000, and under the
	// SAME channel_name as the honest writer -- which is what makes the
	// collision happen at all (uq_bookings_channel_ref is
	// UNIQUE (channel_name, channel_ref); a forgery under its own channel
	// name never contends for the reference). After migration 032 this is
	// the only channel_name an appended deposit booking may have, so this is
	// not the awkward case, it is the ONLY case.
	_, err := h.pool.Exec(ctx, `
		INSERT INTO bookings (classification_id, account_holder, currency_id, amount, status,
		                      channel_name, channel_ref, idempotency_key, metadata, uid)
		VALUES ((SELECT id FROM classifications WHERE code = 'deposit'), $1,
		        (SELECT id FROM currencies WHERE code = 'USDT-taken'), 5000, 'pending',
		        'onchain', '', 'r3-taken-forged',
		        jsonb_build_object('chain_id','1','tx_hash',$2::text,'txlog_seq','0','token',$3::text,'block_number','100'),
		        gen_random_uuid())`,
		holder, txHash, token)
	require.NoError(t, err, "pre-032 schema: the row goes in, which is this pin's premise")

	for i := 0; i < 3; i++ {
		require.NoError(t, h.svc.RunPendingRecheckOnce(ctx),
			"the tick must not fail on a booking it is about to park: that is what made this invisible")
	}

	deposits, _, err := h.bookings.ListBookings(ctx, core.BookingFilter{
		ClassificationUID: h.classificationUID(t, "deposit"), Limit: 20,
	})
	require.NoError(t, err)
	require.Len(t, deposits, 2)

	var forged *core.Booking
	for i := range deposits {
		if deposits[i].UID != anchor.UID {
			forged = &deposits[i]
		}
	}
	require.NotNil(t, forged)

	assert.Equal(t, core.Status("review"), forged.Status,
		"an uncorroborated booking must reach the queue even when the log it names is taken")
	assert.Equal(t, "onchain_unverified", forged.Metadata["review_reason"],
		"and the reason must LAND -- an operator reads the booking, not the process's logs")
	assert.Empty(t, forged.JournalUID)
	assert.True(t, strings.HasPrefix(forged.ChannelRef, txHash+"#0#unverified-"),
		"the walk to review must not claim the reference the real booking holds")

	reasons := h.metrics.reviewReasons()
	assert.Contains(t, reasons, "onchain_unverified",
		"the review-required counter is the alert this whole path exists to raise")

	// The genuine deposit still owns its own reference and its own journal.
	genuine, err := h.bookings.GetBooking(ctx, anchor.UID)
	require.NoError(t, err)
	assert.Equal(t, txHash+"#0", genuine.ChannelRef)
	assert.Equal(t, core.Status("confirmed"), genuine.Status)
	assert.NotEmpty(t, genuine.JournalUID)
}

// TestDepositIdentity_HonestIngestIsUnaffected is the control group: the
// index and the guard constrain nothing the ledger legitimately does. Two
// observations of the same transfer (the watcher re-scanning a window, a
// webhook redelivering) still resolve to ONE booking rather than colliding,
// because IngestDeposit's idempotency resolves them before any INSERT is
// attempted -- which is the argument the index rests on (I-66's shape: a
// property of the only honest writer there is).
func TestDepositIdentity_HonestIngestIsUnaffected(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
		txHash  = "0xhonestrescan"
		holder  = int64(9601)
	)
	h, anchor, address := realDepositHarness(t, chainID, token, "USDT-honest", txHash, holder)
	ctx := context.Background()

	// The same sighting again, twice: re-scanned window, redelivered webhook.
	for i := 0; i < 2; i++ {
		again, err := h.svc.IngestDeposit(ctx, core.DepositSighting{
			ChainID: chainID, TxHash: txHash, TxLogSeq: 0, Token: token,
			From: "0xsender", To: address, Amount: decimal.NewFromInt(50),
			Confirmations: 5, BlockNumber: 100,
		})
		require.NoError(t, err, "re-observing a transfer is not an error")
		require.NotNil(t, again)
		assert.Equal(t, anchor.UID, again.UID, "and it resolves to the booking that already exists")
	}

	deposits, _, err := h.bookings.ListBookings(ctx, core.BookingFilter{
		ClassificationUID: h.classificationUID(t, "deposit"), Limit: 10,
	})
	require.NoError(t, err)
	assert.Len(t, deposits, 1)

	// And a forward scan over the same window, which is the other path that
	// re-observes an already-booked transfer.
	require.NoError(t, h.svc.RunWatchOnce(ctx, chainID))
	deposits, _, err = h.bookings.ListBookings(ctx, core.BookingFilter{
		ClassificationUID: h.classificationUID(t, "deposit"), Limit: 10,
	})
	require.NoError(t, err)
	assert.Len(t, deposits, 1, "a re-scan of a scanned window must stay a no-op")

	// The other half of the control, and the one that matters most: a
	// booking the recheck loop drives to confirmed must not be blocked by
	// the already-booked check finding ITSELF. A second transfer, still
	// below its confirmation threshold when first seen, then rechecked.
	const secondTx = "0xhonestsecond"
	h.reader.setIncluded(chainID, secondTx, true)
	h.reader.setSightings(chainID,
		core.DepositSighting{
			ChainID: chainID, TxHash: txHash, TxLogSeq: 0, Token: token,
			From: "0xsender", To: address, Amount: decimal.NewFromInt(50),
			Confirmations: 0, BlockNumber: 100,
		},
		core.DepositSighting{
			ChainID: chainID, TxHash: secondTx, TxLogSeq: 0, Token: token,
			From: "0xsender", To: address, Amount: decimal.NewFromInt(7),
			Confirmations: 0, BlockNumber: 400,
		})
	pending, err := h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: secondTx, TxLogSeq: 0, Token: token,
		From: "0xsender", To: address, Amount: decimal.NewFromInt(7),
		Confirmations: 0, BlockNumber: 400,
	})
	require.NoError(t, err)
	require.Equal(t, core.Status("confirming"), pending.Status)

	require.NoError(t, h.svc.RunPendingRecheckOnce(ctx))
	confirmed, err := h.bookings.GetBooking(ctx, pending.UID)
	require.NoError(t, err)
	assert.Equal(t, core.Status("confirmed"), confirmed.Status,
		"the only booking holding a log is not a duplicate of itself")
	assert.NotEmpty(t, confirmed.JournalUID, "and it must still be credited")
}

// --- N-1 (a): the same row is reachable from POST /bookings ---------------

// TestDepositIdentity_ACallerSuppliedBookingCannotStealATransfer covers the
// entry point the review named but did not measure (§5.1): N-1 does not need
// a database credential. `POST /api/v1/bookings` takes a write-scope API key
// and lets the caller choose classification_code, account_holder, amount,
// channel_name AND metadata, so the same row is one authenticated HTTP call
// away -- and the recheck job does not distinguish where a booking came
// from.
//
// This drives the domain call the handler makes (server's own
// TestCreateBooking_DepositMetadataPassesThrough pins that the handler
// passes those fields through unchanged, so the two together are the path).
// Two claims: a caller cannot take over a transfer another booking already
// holds, and a caller-invented transfer is not credited on its own say-so.
func TestDepositIdentity_ACallerSuppliedBookingCannotStealATransfer(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
		txHash  = "0xhttpcaller"
		holder  = int64(9301)
	)
	h, anchor, _ := realDepositHarness(t, chainID, token, "USDT-http", txHash, holder)
	ctx := context.Background()

	currencies, err := h.currencies.ListCurrencies(ctx, false)
	require.NoError(t, err)
	require.Len(t, currencies, 1)

	// Exactly what handleCreateBooking builds from a write-scope request
	// body naming the deposit classification and the real transfer.
	_, err = h.booker.CreateBooking(ctx, core.CreateBookingInput{
		ClassificationCode: "deposit",
		AccountHolder:      holder,
		CurrencyUID:        currencies[0].UID,
		Amount:             decimal.NewFromInt(50),
		IdempotencyKey:     "caller-chosen-key-1",
		ChannelName:        "my-own-channel",
		Metadata: map[string]string{
			"chain_id": "1", "tx_hash": txHash, "txlog_seq": "0",
			"token": token, "block_number": "100",
		},
	})
	require.Error(t, err, "a write-scope key must not be able to file a second claim on a booked transfer")
	assert.ErrorIs(t, err, core.ErrConflict)

	// And an invented transfer, which no index can refuse because it is the
	// first claim on it, is still not credited: the corroboration re-read
	// finds no such log.
	invented, err := h.booker.CreateBooking(ctx, core.CreateBookingInput{
		ClassificationCode: "deposit",
		AccountHolder:      holder,
		CurrencyUID:        currencies[0].UID,
		Amount:             decimal.NewFromInt(9999),
		IdempotencyKey:     "caller-chosen-key-2",
		ChannelName:        "my-own-channel",
		Metadata: map[string]string{
			"chain_id": "1", "tx_hash": "0xnosuchtransfer", "txlog_seq": "0",
			"token": token, "block_number": "100",
		},
	})
	require.NoError(t, err, "the ledger has no way to refuse a first claim at creation time -- that is what I-69 is for")

	for i := 0; i < 3; i++ {
		require.NoError(t, h.svc.RunPendingRecheckOnce(ctx))
	}
	after, err := h.bookings.GetBooking(ctx, invented.UID)
	require.NoError(t, err)
	assert.Equal(t, core.Status("review"), after.Status)
	assert.Empty(t, after.JournalUID, "a caller-supplied booking is a claim, not a credit")

	// The genuine deposit is untouched throughout.
	genuine, err := h.bookings.GetBooking(ctx, anchor.UID)
	require.NoError(t, err)
	assert.Equal(t, core.Status("confirmed"), genuine.Status)
}

// --- N-1 (b): the honest deposit must not be lost behind a squatted row ---

// TestDepositIdentity_HonestIngestSaysWhoTookTheTransfer pins the boundary
// migration 032 introduces. A caller-supplied booking (write-scope HTTP key,
// or a leaked database credential) can claim a REAL transfer's identity
// BEFORE the watcher reaches it. The honest IngestDeposit then cannot create
// its own booking -- the index refuses it -- and that must not be a silent
// loss: the deposit is real, unbooked, and nothing else will revisit it.
//
// So the dead letter that results says which booking is in the way, under
// its own bounded reason, and the counter fires. RUNBOOK section 18 has the
// triage; the row carries the sighting, so a replay works once the squatter
// is resolved.
func TestDepositIdentity_HonestIngestSaysWhoTookTheTransfer(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
		txHash  = "0xsquatted"
		holder  = int64(9302)
	)
	chains := chainSetWithCeilings(chainID, token, "USDT-squat", 1,
		decimal.NewFromInt(100_000), decimal.Zero)
	h := setupOnchain(t, chains, []string{"USDT-squat"})
	ctx := context.Background()

	da, err := h.svc.EnsureDepositAddress(ctx, holder)
	require.NoError(t, err)
	currencies, err := h.currencies.ListCurrencies(ctx, false)
	require.NoError(t, err)

	// The squatter gets there first, with its own idempotency key.
	squatter, err := h.booker.CreateBooking(ctx, core.CreateBookingInput{
		ClassificationCode: "deposit",
		AccountHolder:      holder,
		CurrencyUID:        currencies[0].UID,
		Amount:             decimal.NewFromInt(1),
		IdempotencyKey:     "squatter-key",
		ChannelName:        "not-the-watcher",
		Metadata: map[string]string{
			"chain_id": "1", "tx_hash": txHash, "txlog_seq": "0",
			"token": token, "block_number": "100",
		},
	})
	require.NoError(t, err)

	// Then the real transfer arrives.
	h.reader.setLatestBlock(chainID, 500)
	h.reader.setIncluded(chainID, txHash, true)
	_, err = h.svc.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: txHash, TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.NewFromInt(50),
		Confirmations: 5, BlockNumber: 100,
	})
	require.Error(t, err, "the index refuses the second booking, which is the point -- but the deposit is real")

	letters, _, err := h.deadLetters.ListDeadLetters(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, letters, 1, "a real deposit the ledger could not book must leave the only trace it can")
	assert.Contains(t, letters[0].Reason, squatter.UID,
		"and the trace must name the booking that is in the way -- an operator cannot resolve what it cannot find")
	assert.Equal(t, decimal.NewFromInt(50).String(), letters[0].Sighting.Amount.String(),
		"the sighting rides along, so a replay works once the squatter is dealt with")

	assert.Equal(t, []deadLetteredCall{{chainID: chainID, reason: "identity_already_booked"}},
		h.metrics.deadLetteredCalls(),
		"and it is counted under its own reason: this is not a normalization bug, it is a stolen identity")

	assert.True(t, h.logger(t).contains("deposit.identity_already_booked"),
		"the log line names it too -- this is the one dead-letter reason where the SIGHTING is not at fault")
}

// --- webhook-only deployments get the dead-letter signals too -------------

// TestDeadLetterSignals_ReachAWebhookOnlyDeployment pins the two signals a
// push-only deployment was missing (2026-09-03 re-check, onchain-ops).
//
// A webhook-only consumer drives IngestDeposit straight from an HTTP handler
// and configures no ChainReader -- a supported configuration (Run says so
// and skips the watcher jobs). Both dead-letter signals were nevertheless
// wired to the watcher's world: the counter was only emitted on the
// scanChainOnce path, because IngestDeposit's own conflict branch called the
// store directly and skipped it, and the backlog gauge was sampled from the
// deep-reorg tick, which such a deployment never runs. So the one ingestion
// path they DO have could dead-letter a deposit with both signals reading
// zero.
func TestDeadLetterSignals_ReachAWebhookOnlyDeployment(t *testing.T) {
	const (
		chainID = int64(1)
		token   = "0xusdttoken"
		txHash  = "0xwebhookonly"
		holder  = int64(9401)
	)
	chains := chainSetWithCeilings(chainID, token, "USDT-push", 1,
		decimal.NewFromInt(100_000), decimal.Zero)
	h := setupOnchain(t, chains, []string{"USDT-push"})
	ctx := context.Background()

	// Webhook-only: same deps, no chain reader, no scanner, no sweeper.
	deps := h.deps
	deps.Reader = nil
	deps.Scanner = nil
	deps.Sweeper = nil
	push := service.NewOnchain(deps, chains,
		service.WithPool(h.pool),
		service.WithDeadLetterSampleInterval(20*time.Millisecond),
	)

	da, err := push.EnsureDepositAddress(ctx, holder)
	require.NoError(t, err)

	booked, err := push.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: txHash, TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.NewFromInt(50),
		Confirmations: 0, BlockNumber: 100,
	})
	require.NoError(t, err)
	require.NotNil(t, booked)

	// The same transfer redelivered with a different amount: one sighting,
	// two stories. CreateBooking refuses it (I-3's three-state idempotency)
	// and the ledger dead-letters it, which is the design -- what must not
	// happen is that nothing says so.
	_, err = push.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: txHash, TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.NewFromInt(5000),
		Confirmations: 0, BlockNumber: 100,
	})
	require.Error(t, err)

	assert.Equal(t, []deadLetteredCall{{chainID: chainID, reason: "payload_conflict"}},
		h.metrics.deadLetteredCalls(),
		"IngestDeposit's own conflict branch must count the dead letter, not just write the row")

	letters, _, err := h.deadLetters.ListDeadLetters(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, letters, 1)
	assert.True(t, letters[0].Booked,
		"this transfer IS booked -- the redelivery disagreed about the amount, and the original booking stands")

	// A second transfer, whose identity a caller-supplied booking took
	// first: this one leaves a dead letter that is genuinely OWED, which is
	// what the backlog gauge is for.
	const squatted = "0xwebhooksquatted"
	currencies, err := h.currencies.ListCurrencies(ctx, false)
	require.NoError(t, err)
	_, err = h.booker.CreateBooking(ctx, core.CreateBookingInput{
		ClassificationCode: "deposit",
		AccountHolder:      holder,
		CurrencyUID:        currencies[0].UID,
		Amount:             decimal.NewFromInt(1),
		IdempotencyKey:     "webhook-squatter-key",
		ChannelName:        "not-the-webhook",
		Metadata: map[string]string{
			"chain_id": "1", "tx_hash": squatted, "txlog_seq": "0",
			"token": token, "block_number": "100",
		},
	})
	require.NoError(t, err)

	_, err = push.IngestDeposit(ctx, core.DepositSighting{
		ChainID: chainID, TxHash: squatted, TxLogSeq: 0, Token: token,
		From: "0xsender", To: da.Address, Amount: decimal.NewFromInt(77),
		Confirmations: 0, BlockNumber: 100,
	})
	require.Error(t, err)
	assert.Equal(t,
		[]deadLetteredCall{
			{chainID: chainID, reason: "payload_conflict"},
			{chainID: chainID, reason: "identity_already_booked"},
		},
		h.metrics.deadLetteredCalls(),
		"the two conflicts are different incidents with different fixes, so they carry different reasons")

	// And the backlog gauge is sampled by a job that does not need a chain
	// reader to exist.
	runCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	require.NoError(t, push.Run(runCtx))

	backlog := h.metrics.backlogCalls()
	require.NotEmpty(t, backlog, "a deployment that can create a dead letter must be able to see how many it has")
	assert.Equal(t, int64(1), backlog[len(backlog)-1].count,
		"one owed deposit, not two: the redelivered one was booked all along, and the queue answers that from bookings")
	assert.Positive(t, backlog[len(backlog)-1].oldestAge)

	completed, failed := h.metrics.tickCounts()
	assert.Positive(t, completed["onchain_dead_letter_backlog"]+failed["onchain_dead_letter_backlog"],
		"and the sampling job reports its own ticks like every other (M-9)")
}
