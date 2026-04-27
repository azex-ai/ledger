package server

import (
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/httpx"
)

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
