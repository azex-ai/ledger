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
	Metadata     map[string]string
}

// Adapter parses and verifies inbound webhook callbacks from external channels.
// Each channel (EVM, TRON, bank, etc.) implements this interface.
//
// Responsibility split for new implementers: VerifySignature (HMAC + a
// timestamp/time-window check) only proves the payload wasn't tampered with
// and isn't stale — it does NOT prevent a valid signed callback from being
// replayed again within that window (e.g. a third party or the sender itself
// re-POSTing a captured request while the timestamp is still fresh).
// Replay protection is NOT this adapter's job: it is provided downstream by
// the idempotency of Booker.Transition, which resolves a repeated transition
// for the same booking/channel_ref to the original result rather than
// double-processing it. An Adapter implementation must not assume
// VerifySignature alone makes replays safe.
type Adapter interface {
	Name() string
	VerifySignature(header http.Header, body []byte) error
	ParseCallback(header http.Header, body []byte) (*CallbackPayload, error)
}
