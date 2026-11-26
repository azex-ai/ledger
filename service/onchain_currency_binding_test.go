package service_test

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/presets"
	"github.com/azex-ai/ledger/service"
)

// TestOnchain_Recheck_CreditCurrencyMustMatchCorroboratingToken pins that the
// recheck loop credits a deposit only in the currency the corroborating
// token is configured for.
//
// corroborateBeforeConfirm checked the log's token against metadata["token"],
// its amount and its recipient, but never that the token maps to the
// booking's currency -- only IngestDeposit applied that binding. A booking
// created through Booker.CreateBooking (what POST /bookings calls) naming a
// cheap token's real transfer but currency USDC was corroborated and
// credited N USDC against N of the cheap token.
func TestOnchain_Recheck_CreditCurrencyMustMatchCorroboratingToken(t *testing.T) {
	const (
		chainID = int64(1)
		usdc    = "0xusdc"
		cheap   = "0xcheap"
	)
	chains := core.ChainSet{
		chainID: {
			ChainID:       chainID,
			Confirmations: 1,
			Factory:       itFactory,
			InitHash:      itInitHash,
			CreditTokens: map[string]core.TokenConfig{
				usdc:  {TokenAddress: usdc, CurrencyCode: "USDC-bind", AutoCreditCeiling: decimal.NewFromInt(1000)},
				cheap: {TokenAddress: cheap, CurrencyCode: "CHEAP-bind", AutoCreditCeiling: decimal.NewFromInt(1000)},
			},
		},
	}
	h := setupOnchain(t, chains, []string{"USDC-bind", "CHEAP-bind"}, service.WithConfirmationEvidenceMisses(1))
	ctx := context.Background()

	currencyUID := map[string]string{}
	all, err := h.currencies.ListCurrencies(ctx, false)
	require.NoError(t, err)
	for _, c := range all {
		currencyUID[c.Code] = c.UID
	}
	require.NotEmpty(t, currencyUID["USDC-bind"])
	require.NotEmpty(t, currencyUID["CHEAP-bind"])

	da, err := h.svc.EnsureDepositAddress(ctx, 9601)
	require.NoError(t, err)

	h.reader.setLatestBlock(chainID, 500)
	h.reader.setIncluded(chainID, "0xcheaptx", true)
	h.reader.setIncluded(chainID, "0xcheaptx2", true)
	h.reader.setSightings(chainID,
		core.DepositSighting{ChainID: chainID, TxHash: "0xcheaptx", TxLogSeq: 0, Token: cheap,
			From: "0xsender", To: da.Address, Amount: decimal.NewFromInt(500), BlockNumber: 400},
		core.DepositSighting{ChainID: chainID, TxHash: "0xcheaptx2", TxLogSeq: 0, Token: cheap,
			From: "0xsender", To: da.Address, Amount: decimal.NewFromInt(500), BlockNumber: 400},
	)

	create := func(key, currency, tx string) *core.Booking {
		b, err := h.bookings.CreateBooking(ctx, core.CreateBookingInput{
			ClassificationCode: presets.DepositClassificationCode,
			AccountHolder:      9601,
			CurrencyUID:        currency,
			Amount:             decimal.NewFromInt(500),
			IdempotencyKey:     key,
			ChannelName:        "manual-" + key,
			Metadata: map[string]string{
				"chain_id": "1", "tx_hash": tx, "txlog_seq": "0", "token": cheap, "block_number": "400",
			},
		})
		require.NoError(t, err)
		return b
	}
	// The cheap token's transfer, booked as USDC.
	mismatched := create("bind-mismatch", currencyUID["USDC-bind"], "0xcheaptx")
	// The same shape, booked in the token's own currency.
	matching := create("bind-match", currencyUID["CHEAP-bind"], "0xcheaptx2")

	require.NoError(t, h.svc.RunPendingRecheckOnce(ctx))

	got, err := h.bookings.GetBooking(ctx, mismatched.UID)
	require.NoError(t, err)
	assert.Equal(t, core.Status("review"), got.Status,
		"a transfer of one token must not corroborate a credit in another token's currency")
	assert.Empty(t, got.JournalUID)
	assert.Equal(t, "onchain_unverified", got.Metadata["review_reason"])

	got, err = h.bookings.GetBooking(ctx, matching.UID)
	require.NoError(t, err)
	assert.Equal(t, core.Status("confirmed"), got.Status, "the token's own currency still credits")
	assert.NotEmpty(t, got.JournalUID)
}
