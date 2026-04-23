package server

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/httpx"
)

func (s *Server) handleGetEvent(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid event ID"))
		return
	}

	evt, err := s.eventReader.GetEvent(r.Context(), id)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, eventToResponse(evt))
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	classificationCode := q.Get("classification_code")

	var operationID int64
	if o := q.Get("operation_id"); o != "" {
		var err error
		operationID, err = strconv.ParseInt(o, 10, 64)
		if err != nil {
			httpx.Error(w, httpx.ErrBadRequest("operation_id must be a number"))
			return
		}
	}

	toStatus := q.Get("to_status")

	cursor, err := decodeCursor(q.Get("cursor"))
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid cursor value"))
		return
	}

	limit := parsePageLimit(r)

	filter := core.EventFilter{
		ClassificationCode: classificationCode,
		OperationID:        operationID,
		ToStatus:           toStatus,
		Cursor:             cursor,
		Limit:              int(limit),
	}

	events, err := s.eventReader.ListEvents(r.Context(), filter)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	resp := PagedResponse[eventResponse]{
		Data: make([]eventResponse, len(events)),
	}
	for i, evt := range events {
		resp.Data[i] = eventToResponse(&evt)
	}
	if len(events) == int(limit) {
		resp.NextCursor = encodeCursor(events[len(events)-1].ID)
	}
	httpx.OK(w, resp)
}
