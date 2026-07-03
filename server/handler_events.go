package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/httpx"
)

func (s *Server) handleGetEvent(w http.ResponseWriter, r *http.Request) {
	uid := chi.URLParam(r, "uid")
	if uid == "" {
		httpx.Error(w, httpx.ErrBadRequest("invalid event uid"))
		return
	}

	evt, err := s.eventReader.GetEvent(r.Context(), uid)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, eventToResponse(evt))
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit := parsePageLimit(r)

	filter := core.EventFilter{
		ClassificationCode: q.Get("classification_code"),
		BookingUID:         q.Get("booking_uid"),
		ToStatus:           q.Get("to_status"),
		Cursor:             q.Get("cursor"),
		Limit:              int(limit),
	}

	events, nextCursor, err := s.eventReader.ListEvents(r.Context(), filter)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	resp := PagedResponse[eventResponse]{
		List:       make([]eventResponse, len(events)),
		NextCursor: nextCursor,
	}
	for i, evt := range events {
		resp.List[i] = eventToResponse(&evt)
	}
	httpx.OK(w, resp)
}
