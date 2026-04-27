package onchain

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testKey = []byte("test-signing-key")

// computeHMAC mirrors the production signature: HMAC-SHA256 over
// "<timestamp>.<body>".
func computeHMAC(key []byte, ts string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
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
	now := time.Unix(1_700_000_000, 0)
	a.now = func() time.Time { return now }
	tsStr := strconv.FormatInt(now.Unix(), 10)

	t.Run("valid signature", func(t *testing.T) {
		header := http.Header{}
		header.Set("X-Timestamp", tsStr)
		header.Set("X-Signature", computeHMAC(testKey, tsStr, body))
		assert.NoError(t, a.VerifySignature(header, body))
	})

	t.Run("missing timestamp", func(t *testing.T) {
		header := http.Header{}
		header.Set("X-Signature", computeHMAC(testKey, tsStr, body))
		err := a.VerifySignature(header, body)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing X-Timestamp")
	})

	t.Run("invalid timestamp", func(t *testing.T) {
		header := http.Header{}
		header.Set("X-Timestamp", "not-a-number")
		header.Set("X-Signature", "deadbeef")
		err := a.VerifySignature(header, body)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid X-Timestamp")
	})

	t.Run("timestamp too old (replay)", func(t *testing.T) {
		oldTS := strconv.FormatInt(now.Add(-10*time.Minute).Unix(), 10)
		header := http.Header{}
		header.Set("X-Timestamp", oldTS)
		header.Set("X-Signature", computeHMAC(testKey, oldTS, body))
		err := a.VerifySignature(header, body)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "outside acceptance window")
	})

	t.Run("timestamp too far in future", func(t *testing.T) {
		futureTS := strconv.FormatInt(now.Add(10*time.Minute).Unix(), 10)
		header := http.Header{}
		header.Set("X-Timestamp", futureTS)
		header.Set("X-Signature", computeHMAC(testKey, futureTS, body))
		err := a.VerifySignature(header, body)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "outside acceptance window")
	})

	t.Run("missing signature", func(t *testing.T) {
		header := http.Header{}
		header.Set("X-Timestamp", tsStr)
		err := a.VerifySignature(header, body)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing X-Signature")
	})

	t.Run("wrong signature", func(t *testing.T) {
		header := http.Header{}
		header.Set("X-Timestamp", tsStr)
		header.Set("X-Signature", "deadbeef")
		err := a.VerifySignature(header, body)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "signature mismatch")
	})

	t.Run("signature without timestamp prefix is rejected", func(t *testing.T) {
		// Signing only the body (no timestamp) — the old behaviour — must fail.
		mac := hmac.New(sha256.New, testKey)
		mac.Write(body)
		bodyOnlySig := hex.EncodeToString(mac.Sum(nil))

		header := http.Header{}
		header.Set("X-Timestamp", tsStr)
		header.Set("X-Signature", bodyOnlySig)
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
