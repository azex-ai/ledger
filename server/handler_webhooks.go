package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/bizcode"
	"github.com/azex-ai/ledger/pkg/httpx"
)

// depositConfirmedStatus is the terminal state a deposit lifecycle reaches
// only once its journal has been posted. Named here because the legacy
// callback path has to refuse it by value (see handleWebhookCallback); the
// lifecycle itself lives in presets.DepositLifecycle.
const depositConfirmedStatus core.Status = "confirmed"

// WebhookNonceRecorder is the replay cache the webhook handler consults after
// signature verification. Nil disables the check (library consumers wiring
// their own HTTP layer, tests). See postgres.WebhookSubscriberStore.TryRecordNonce.
type WebhookNonceRecorder interface {
	TryRecordNonce(ctx context.Context, nonce string) (bool, error)
}

// SetWebhookNonceRecorder installs the inbound-webhook replay cache.
//
// Optional, but leaving it out is a real reduction in what this endpoint
// guarantees, not a configuration nicety: the signature covers a timestamp and
// the adapter rejects timestamps outside a +/-5 minute window, so an identical
// request replayed INSIDE that window verifies every time. The nonce cache is
// the layer that rejects it. Whether the replay then causes double accounting
// depends entirely on the downstream idempotency key, which for the sighting
// path is derived from the transfer itself and for the legacy path is derived
// from (booking_uid, status, channel_ref).
//
// ledger.Service exposes a ready-made one -- svc.WebhookNonceRecorder() -- and
// leaving it nil now produces a one-time warning on the first callback that
// takes the unprotected path (see handleWebhookCallback).
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
	//
	// The nil branch is a degraded mode and says so. Until this warning
	// existed, "replay protection is on" and "replay protection is off" had
	// identical runtime output, and every example and README assembly in this
	// repository produced the second one — the capability was in the library
	// and absent from every path a consumer actually walks, which is the shape
	// this audit round found four times (working-agreements §3).
	//
	// Warned here rather than in newServer because the recorder is late-bound:
	// SetWebhookNonceRecorder may legitimately be called after the Server is
	// constructed. sync.Once because the condition cannot change once the
	// first callback has arrived, so per-request logging would only bury it.
	if s.webhookNonces == nil {
		s.webhookNonceWarnOnce.Do(func() {
			slog.Warn("server: inbound webhook replay cache is NOT configured — a callback replayed inside the signature's ±5 minute window " +
				"passes signature verification and reaches ingestion; whether it double-books depends solely on downstream idempotency. " +
				"Wire it with srv.SetWebhookNonceRecorder(svc.WebhookNonceRecorder())")
		})
	}
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

	// G-m5: and it may not declare a deposit CONFIRMED through this path.
	//
	// I-21 states that a confirmed deposit's accounting comes from exactly one
	// place -- service's postDepositConfirmedJournal, which posts the journal
	// in the same transaction as the transition. That is true of the service
	// layer and was never true of this one: the legacy ParseCallback branch
	// hands ToStatus straight to Booker.Transition, which moves the booking to
	// a terminal `confirmed` and posts nothing. The result is a deposit the
	// holder-facing surfaces call settled with no entries behind it and no
	// journal for reconciliation to find missing.
	//
	// Unreachable through this repository's own adapter (channel/onchain
	// implements sightingParser, so it is routed above and never reaches
	// here), which is exactly why it went unnoticed: the path is only taken by
	// a consumer's own channel.Adapter, and a consumer's adapter is the case
	// the classification confinement two lines up already exists to defend
	// against.
	if core.Status(payload.Status) == depositConfirmedStatus {
		httpx.Error(w, httpx.ErrForbidden(
			"a channel callback may not confirm a deposit: confirmation posts accounting and must go through the deposit sighting path (implement channel.SightingParser), not a bare status transition"))
		return
	}

	// System-event-derived key (api-contract.md §9): this channel adapter's
	// contract (channel/adapter.go's Adapter doc comment) already promises
	// "a repeated transition for the same booking/channel_ref ... resolves
	// to the original result" -- (booking_uid, status, channel_ref) IS this
	// channel's definition of "the same callback", so deriving the key from
	// exactly those three fields matches the behavior the adapter interface
	// already documents, without inventing a new identity concept.
	evt, err := s.booker.Transition(r.Context(), core.TransitionInput{
		BookingUID:     payload.BookingUID,
		ToStatus:       core.Status(payload.Status),
		ChannelRef:     payload.ChannelRef,
		Amount:         payload.ActualAmount,
		Metadata:       payload.Metadata,
		IdempotencyKey: fmt.Sprintf("webhook-%s-%s-%s", payload.BookingUID, payload.Status, payload.ChannelRef),
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
	// IngestDeposit returns (nil, nil) for a sighting this ledger has no
	// business booking -- unregistered address, non-whitelisted token, or an
	// unconfigured chain (its own doc comment). That is a normal "nothing to
	// do" outcome, not an error: respond 200 no-op so the external scanner
	// marks the callback delivered and does not retry it forever. Passing a
	// nil booking into bookingToResponse would panic on its first field
	// dereference (op.UID) -- chi's Recoverer middleware would turn that
	// into a 500, which the scanner treats as a delivery failure and retries
	// indefinitely (M2).
	if booking == nil {
		httpx.OK(w, depositSightingIgnoredResponse{Status: "ignored"})
		return
	}
	httpx.OK(w, bookingToResponse(booking))
}

// depositSightingIgnoredResponse is handleDepositSighting's response body
// when IngestDeposit had nothing to book (see its doc comment) -- distinct
// from bookingResponse so callers can tell "no booking exists for this
// sighting" apart from an actual booking payload.
type depositSightingIgnoredResponse struct {
	Status string `json:"status"`
}
