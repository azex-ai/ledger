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
	"github.com/azex-ai/ledger/core"
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

// sightingPayload is the wire shape of an on-chain deposit sighting pushed by
// an external block-scanner webhook -- one-to-one with core.DepositSighting.
// This is the push-path counterpart to chains/evm's eth_getLogs watcher (pull
// path); see ParseSighting.
type sightingPayload struct {
	ChainID  int64  `json:"chain_id"`
	TxHash   string `json:"tx_hash"`
	TxLogSeq int32  `json:"txlog_seq"`
	Token    string `json:"token"`
	From     string `json:"from"`
	To       string `json:"to"`
	Amount   string `json:"amount"`
	// BlockNumber is required -- see core.DepositSighting.BlockNumber's doc
	// comment and Validate(), which rejects <= 0. The external block scanner
	// pushing this webhook must report the block the transfer log was mined
	// in, not just its confirmation count at push time.
	BlockNumber   int64 `json:"block_number"`
	Confirmations int32 `json:"confirmations"`
}

// ParseSighting normalizes a webhook body into a core.DepositSighting
// (design doc §3): the watcher (pull) and this webhook (push) are the two
// ingestion paths that converge on the caller's single IngestDeposit
// orchestration. A server routes here instead of ParseCallback whenever the
// resolved channel.Adapter implements this method (see the server package's
// sightingParser type assertion in handleWebhookCallback) -- ParseCallback
// stays implemented for channel.Adapter compliance and any caller still
// using the legacy "transition an existing booking_uid" shape.
func (a *EVMAdapter) ParseSighting(header http.Header, body []byte) (*core.DepositSighting, error) {
	var raw sightingPayload
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("channel: evm: parse sighting: %w", err)
	}
	amount, err := decimal.NewFromString(raw.Amount)
	if err != nil {
		return nil, fmt.Errorf("channel: evm: invalid amount %q: %w", raw.Amount, err)
	}
	sighting := &core.DepositSighting{
		ChainID:       raw.ChainID,
		TxHash:        raw.TxHash,
		TxLogSeq:      raw.TxLogSeq,
		Token:         raw.Token,
		From:          raw.From,
		To:            raw.To,
		Amount:        amount,
		Confirmations: raw.Confirmations,
		BlockNumber:   raw.BlockNumber,
	}
	if err := sighting.Validate(); err != nil {
		return nil, fmt.Errorf("channel: evm: %w", err)
	}
	return sighting, nil
}

func (a *EVMAdapter) ParseCallback(header http.Header, body []byte) (*channel.CallbackPayload, error) {
	var raw struct {
		TxHash        string `json:"tx_hash"`
		BookingUID    string `json:"booking_uid"`
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
		BookingUID:   raw.BookingUID,
		ChannelRef:   raw.TxHash,
		Status:       raw.Status,
		ActualAmount: amount,
		Metadata: map[string]string{
			"confirmations": strconv.Itoa(raw.Confirmations),
			"tx_hash":       raw.TxHash,
		},
	}, nil
}
