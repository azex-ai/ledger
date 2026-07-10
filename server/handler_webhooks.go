package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/bizcode"
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

	// On-chain deposit sightings converge here regardless of ingestion path
	// (watcher pull vs this webhook push, design doc §3): an adapter offering
	// this shape is routed to IngestDeposit instead of the legacy
	// booking_uid transition flow below. This structurally closes design doc
	// §5-5's "forge a transition on an unrelated booking" concern for this
	// channel — IngestDeposit only ever creates/advances
	// deposit-classification bookings and never accepts a caller-supplied
	// booking_uid.
	if sp, ok := adapter.(sightingParser); ok {
		s.handleDepositSighting(w, r, sp, body)
		return
	}

	payload, err := adapter.ParseCallback(r.Header, body)
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid callback"))
		return
	}

	// Ownership check: a compromised channel adapter could otherwise transition
	// any booking by passing an arbitrary booking_uid in the payload. Trust the
	// channel→booking mapping in the database, not what the payload claims.
	booking, err := s.bookingReader.GetBooking(r.Context(), payload.BookingUID)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if booking.ChannelName != channelName {
		httpx.Error(w, httpx.ErrForbidden("channel mismatch for booking"))
		return
	}

	// Classification confinement (design doc §5-5): even with a valid
	// signature and a matching channel name, a webhook must never be able to
	// transition a booking outside the deposit lifecycle it exists to serve
	// — most importantly a `sweep` booking, which has no journal and so a
	// forged "confirmed" would leave no accounting trace to catch it.
	depositClass, err := s.classifications.GetByCode(r.Context(), depositClassificationCode)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if booking.ClassificationUID != depositClass.UID {
		httpx.Error(w, httpx.ErrForbidden("webhook channel may only transition deposit bookings"))
		return
	}

	evt, err := s.booker.Transition(r.Context(), core.TransitionInput{
		BookingUID: payload.BookingUID,
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

// handleDepositSighting is the push-path counterpart to chains/evm's watcher
// (pull path): both normalize into a core.DepositSighting and hand off to
// the same IngestDeposit orchestration (design doc §3).
func (s *Server) handleDepositSighting(w http.ResponseWriter, r *http.Request, sp sightingParser, body []byte) {
	if s.depositIngester == nil {
		httpx.Error(w, bizcode.FeatureNotEnabled)
		return
	}
	sighting, err := sp.ParseSighting(r.Header, body)
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid callback"))
		return
	}
	booking, err := s.depositIngester.IngestDeposit(r.Context(), *sighting)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, bookingToResponse(booking))
}
