package onchain

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/azex-ai/ledger/channel"
	"github.com/shopspring/decimal"
)

// EVMAdapter handles callbacks from an external EVM block scanner.
type EVMAdapter struct {
	signingKey []byte
}

// New creates an EVMAdapter with the given HMAC signing key.
func New(signingKey []byte) *EVMAdapter {
	return &EVMAdapter{signingKey: signingKey}
}

func (a *EVMAdapter) Name() string { return "evm" }

func (a *EVMAdapter) VerifySignature(header http.Header, body []byte) error {
	sig := header.Get("X-Signature")
	if sig == "" {
		return fmt.Errorf("channel: evm: missing X-Signature header")
	}
	mac := hmac.New(sha256.New, a.signingKey)
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
		OperationID   int64  `json:"operation_id"`
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
		OperationID:  raw.OperationID,
		ChannelRef:   raw.TxHash,
		Status:       raw.Status,
		ActualAmount: amount,
		Metadata: map[string]any{
			"confirmations": raw.Confirmations,
			"tx_hash":       raw.TxHash,
		},
	}, nil
}
