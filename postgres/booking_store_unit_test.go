package postgres

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

func TestIdempotentTransitionEvent(t *testing.T) {
	current := &core.Booking{
		UID:           "uid-1",
		Status:        "confirmed",
		ChannelRef:    "tx-1",
		SettledAmount: decimal.NewFromInt(100),
	}
	latest := &core.Event{
		UID:        "uid-10",
		BookingUID: "bk-1",
		ToStatus:   "confirmed",
		Amount:     decimal.NewFromInt(100),
		Metadata:   map[string]string{"tx_hash": "tx-1"},
		JournalUID: "",
	}

	t.Run("reuse matching transition", func(t *testing.T) {
		reused, err := idempotentTransitionEvent(current, latest, core.TransitionInput{
			BookingUID: "bk-1",
			ToStatus:   "confirmed",
			ChannelRef: "tx-1",
			Amount:     decimal.NewFromInt(100),
		})
		require.NoError(t, err)
		require.NotNil(t, reused)
		assert.Equal(t, latest.UID, reused.UID)
	})

	t.Run("channel mismatch conflicts", func(t *testing.T) {
		reused, err := idempotentTransitionEvent(current, latest, core.TransitionInput{
			BookingUID: "bk-1",
			ToStatus:   "confirmed",
			ChannelRef: "tx-2",
			Amount:     decimal.NewFromInt(100),
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, core.ErrConflict)
		assert.Nil(t, reused)
	})

	t.Run("amount mismatch conflicts", func(t *testing.T) {
		reused, err := idempotentTransitionEvent(current, latest, core.TransitionInput{
			BookingUID: "bk-1",
			ToStatus:   "confirmed",
			ChannelRef: "tx-1",
			Amount:     decimal.NewFromInt(90),
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, core.ErrConflict)
		assert.Nil(t, reused)
	})

	t.Run("different status is not idempotent", func(t *testing.T) {
		reused, err := idempotentTransitionEvent(current, latest, core.TransitionInput{
			BookingUID: "bk-1",
			ToStatus:   "failed",
		})
		require.NoError(t, err)
		assert.Nil(t, reused)
	})
}
