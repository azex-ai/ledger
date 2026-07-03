package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/httpx"
)

// WebhookNonceRecorder is the replay cache the webhook handler consults after
// signature verification. Nil disables the check (library consumers wiring
// their own HTTP layer, tests). See postgres.WebhookSubscriberStore.TryRecordNonce.
type WebhookNonceRecorder interface {
	TryRecordNonce(ctx context.Context, nonce string) (bool, error)
}

// SetWebhookNonceRecorder installs the inbound-webhook replay cache. Optional:
// without it, replay protection inside the signature timestamp window relies
// solely on downstream Transition idempotency (the pre-v0.3 behavior).
func (s *Server) SetWebhookNonceRecorder(r WebhookNonceRecorder) { s.webhookNonces = r }

func (s *Server) handleWebhookCallback(w http.ResponseWriter, r *http.Request) {
	channelName := chi.URLParam(r, "channel")
	adapter, ok := s.channels[channelName]
	if !ok {
		httpx.Error(w, httpx.ErrNotFound("unknown channel"))
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB limit
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("read body failed"))
		return
	}

	if err := adapter.VerifySignature(r.Header, body); err != nil {
		httpx.Error(w, httpx.ErrBadRequest("signature verification failed"))
		return
	}

	// Replay check: the signature window (±5 min) only rejects stale replays;
	// an identical request replayed inside the window still verifies. The
	// nonce is a digest of everything that makes the request unique — an
	// exact resend hits the cache and is rejected before touching bookings.
	if s.webhookNonces != nil {
		sum := sha256.Sum256([]byte(channelName + "\x00" + r.Header.Get("X-Timestamp") + "\x00" + r.Header.Get("X-Signature") + "\x00" + string(body)))
		fresh, err := s.webhookNonces.TryRecordNonce(r.Context(), hex.EncodeToString(sum[:]))
		if err != nil {
			httpx.Error(w, err)
			return
		}
		if !fresh {
			httpx.Error(w, httpx.ErrConflict("replayed webhook callback"))
			return
		}
	}

	payload, err := adapter.ParseCallback(r.Header, body)
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid callback"))
		return
	}

	// Ownership check: a compromised channel adapter could otherwise transition
	// any booking by passing an arbitrary booking_id in the payload. Trust the
	// channel→booking mapping in the database, not what the payload claims.
	booking, err := s.bookingReader.GetBooking(r.Context(), payload.BookingID)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if booking.ChannelName != channelName {
		httpx.Error(w, httpx.ErrForbidden("channel mismatch for booking"))
		return
	}

	evt, err := s.booker.Transition(r.Context(), core.TransitionInput{
		BookingID:  payload.BookingID,
		ToStatus:   core.Status(payload.Status),
		ChannelRef: payload.ChannelRef,
		Amount:     payload.ActualAmount,
		Metadata:   payload.Metadata,
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}

	httpx.OK(w, eventToResponse(evt))
}
