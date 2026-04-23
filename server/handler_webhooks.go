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

	body, err := io.ReadAll(r.Body)
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

	evt, err := s.operator.Transition(r.Context(), core.TransitionInput{
		OperationID: payload.OperationID,
		ToStatus:    core.Status(payload.Status),
		ChannelRef:  payload.ChannelRef,
		Amount:      payload.ActualAmount,
		Metadata:    payload.Metadata,
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}

	httpx.OK(w, eventToResponse(evt))
}
