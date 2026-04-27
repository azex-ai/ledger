// Package onchain: evm.go
// EVM block-scanner inbound webhook adapter with HMAC + replay protection.
package onchain

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/azex-ai/ledger/channel"
	"github.com/shopspring/decimal"
)

// signatureFreshness bounds how far the signed timestamp may drift from now.
// Anything older / newer than ±5 minutes is rejected as a replay or clock skew.
const signatureFreshness = 5 * time.Minute

// EVMAdapter handles callbacks from an external EVM block scanner.
type EVMAdapter struct {
	signingKey []byte
	now        func() time.Time // injectable for tests
}

// New creates an EVMAdapter with the given HMAC signing key.
func New(signingKey []byte) *EVMAdapter {
	return &EVMAdapter{signingKey: signingKey, now: time.Now}
}

func (a *EVMAdapter) Name() string { return "evm" }

// VerifySignature requires X-Timestamp (unix seconds) and X-Signature
// (hex-encoded HMAC-SHA256 of "<timestamp>.<body>"). The timestamp must be
// within ±signatureFreshness of now to defeat replay attacks.
func (a *EVMAdapter) VerifySignature(header http.Header, body []byte) error {
	tsHeader := header.Get("X-Timestamp")
	if tsHeader == "" {
		return fmt.Errorf("channel: evm: missing X-Timestamp header")
	}
	ts, err := strconv.ParseInt(tsHeader, 10, 64)
	if err != nil {
		return fmt.Errorf("channel: evm: invalid X-Timestamp %q: %w", tsHeader, err)
	}
	now := a.now()
	signed := time.Unix(ts, 0)
	skew := now.Sub(signed)
	if skew < 0 {
		skew = -skew
	}
	if skew > signatureFreshness {
		return fmt.Errorf("channel: evm: timestamp outside acceptance window (%s skew)", skew)
	}

	sig := header.Get("X-Signature")
	if sig == "" {
		return fmt.Errorf("channel: evm: missing X-Signature header")
	}

	mac := hmac.New(sha256.New, a.signingKey)
	mac.Write([]byte(tsHeader))
	mac.Write([]byte("."))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return fmt.Errorf("channel: evm: signature mismatch")
	}
	return nil
}

func (a *EVMAdapter) ParseCallback(header http.Header, body []byte) (*channel.CallbackPayload, error) {
	var raw struct {
		TxHash        string `json:"tx_hash"`
		BookingID     int64  `json:"booking_id"`
		Amount        string `json:"amount"`
		Confirmations int    `json:"confirmations"`
		Status        string `json:"status"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("channel: evm: parse: %w", err)
	}
	amount, err := decimal.NewFromString(raw.Amount)
	if err != nil {
		return nil, fmt.Errorf("channel: evm: invalid amount %q: %w", raw.Amount, err)
	}
	return &channel.CallbackPayload{
		BookingID:    raw.BookingID,
		ChannelRef:   raw.TxHash,
		Status:       raw.Status,
		ActualAmount: amount,
		Metadata: map[string]any{
			"confirmations": raw.Confirmations,
			"tx_hash":       raw.TxHash,
		},
	}, nil
}
