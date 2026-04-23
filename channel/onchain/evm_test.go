package onchain

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testKey = []byte("test-signing-key")

func computeHMAC(key, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestEVMAdapter_Name(t *testing.T) {
	a := New(testKey)
	assert.Equal(t, "evm", a.Name())
}

func TestEVMAdapter_VerifySignature(t *testing.T) {
	a := New(testKey)
	body := []byte(`{"tx_hash":"0xabc","booking_id":1,"amount":"100.5","confirmations":12,"status":"confirmed"}`)

	t.Run("valid signature", func(t *testing.T) {
		header := http.Header{}
		header.Set("X-Signature", computeHMAC(testKey, body))
		assert.NoError(t, a.VerifySignature(header, body))
	})

	t.Run("missing header", func(t *testing.T) {
		header := http.Header{}
		err := a.VerifySignature(header, body)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing X-Signature")
	})

	t.Run("wrong signature", func(t *testing.T) {
		header := http.Header{}
		header.Set("X-Signature", "deadbeef")
		err := a.VerifySignature(header, body)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "signature mismatch")
	})
}

func TestEVMAdapter_ParseCallback(t *testing.T) {
	a := New(testKey)

	t.Run("valid JSON", func(t *testing.T) {
		body := []byte(`{"tx_hash":"0xabc123","booking_id":42,"amount":"1.5","confirmations":12,"status":"confirmed"}`)
		header := http.Header{}

		payload, err := a.ParseCallback(header, body)
		require.NoError(t, err)
		assert.Equal(t, int64(42), payload.BookingID)
		assert.Equal(t, "0xabc123", payload.ChannelRef)
		assert.Equal(t, "confirmed", payload.Status)
		assert.True(t, decimal.NewFromFloat(1.5).Equal(payload.ActualAmount))
		assert.Equal(t, 12, payload.Metadata["confirmations"])
		assert.Equal(t, "0xabc123", payload.Metadata["tx_hash"])
	})

	t.Run("invalid JSON", func(t *testing.T) {
		body := []byte(`not json`)
		header := http.Header{}

		_, err := a.ParseCallback(header, body)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "channel: evm: parse")
	})

	t.Run("invalid amount", func(t *testing.T) {
		body := []byte(`{"tx_hash":"0x1","booking_id":1,"amount":"not-a-number","confirmations":1,"status":"pending"}`)
		header := http.Header{}

		_, err := a.ParseCallback(header, body)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid amount")
	})
}
