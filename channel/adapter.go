package channel

import (
	"net/http"

	"github.com/shopspring/decimal"
)

// CallbackPayload is the standardized result of parsing a channel webhook callback.
type CallbackPayload struct {
	BookingID    int64
	ChannelRef   string
	Status       string
	ActualAmount decimal.Decimal
	Metadata     map[string]any
}

// Adapter parses and verifies inbound webhook callbacks from external channels.
// Each channel (EVM, TRON, bank, etc.) implements this interface.
type Adapter interface {
	Name() string
	VerifySignature(header http.Header, body []byte) error
	ParseCallback(header http.Header, body []byte) (*CallbackPayload, error)
}
