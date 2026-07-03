package server

import (
	"net/http"
	"time"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/httpx"
)

// --- JSON request/response types ---

type closePeriodRequest struct {
	CloseBefore string `json:"close_before"`
	Note        string `json:"note"`
	ActorID     int64  `json:"actor_id"`
}

type periodCloseResponse struct {
	UID         string    `json:"uid"`
	CloseBefore time.Time `json:"close_before"`
	Note        string    `json:"note"`
	ActorID     int64     `json:"actor_id"`
	CreatedAt   time.Time `json:"created_at"`
}

func toPeriodCloseResponse(p *core.PeriodClose) periodCloseResponse {
	return periodCloseResponse{
		UID:         p.UID,
		CloseBefore: p.CloseBefore,
		Note:        p.Note,
		ActorID:     p.ActorID,
		CreatedAt:   p.CreatedAt,
	}
}

// --- Handlers ---

func (s *Server) handleClosePeriod(w http.ResponseWriter, r *http.Request) {
	req, err := httpx.Decode[closePeriodRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	closeBefore, err := time.Parse(time.RFC3339, req.CloseBefore)
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("close_before must be RFC3339"))
		return
	}

	pc, err := s.periodCloser.ClosePeriod(r.Context(), core.ClosePeriodInput{
		CloseBefore: closeBefore,
		Note:        req.Note,
		ActorID:     req.ActorID,
	})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.Created(w, toPeriodCloseResponse(pc))
}

func (s *Server) handleListPeriodCloses(w http.ResponseWriter, r *http.Request) {
	limit := int(parsePageLimit(r))

	closes, err := s.periodCloser.ListPeriodCloses(r.Context(), limit)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	resp := make([]periodCloseResponse, len(closes))
	for i, pc := range closes {
		resp[i] = toPeriodCloseResponse(&pc)
	}
	httpx.OK(w, resp)
}
