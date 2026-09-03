// Package service_test: onchain_duplicate_deposit_test.go
//
// money-out N-1 (2026-09-03 independent review, re-check round) and its
// neighbour N-2. One on-chain transfer log may be booked once; a booking
// that is not corroborated must reach the review queue even when the log it
// names belongs to somebody else. docs/INVARIANTS.md I-71.
package service_test

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
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
