package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/httpx"
)

type createReservationRequest struct {
	AccountHolder  int64  `json:"account_holder"`
	CurrencyUID    string `json:"currency_uid"`
	Amount         string `json:"amount"`
	IdempotencyKey string `json:"idempotency_key"`
	ExpiresInSec   int64  `json:"expires_in_sec"`
}

type settleReservationRequest struct {
	ActualAmount string `json:"actual_amount"`
}

type settlePartialReservationRequest struct {
	Amount string `json:"amount"`
	// IdempotencyKey is required (I-3): SettlePartial accumulates, so a
	// retried request without a key would double-apply the amount.
	IdempotencyKey string `json:"idempotency_key"`
}

type reservationResponse struct {
	UID            string    `json:"uid"`
	AccountHolder  int64     `json:"account_holder"`
	CurrencyUID    string    `json:"currency_uid"`
	ReservedAmount string    `json:"reserved_amount"`
	SettledAmount  *string   `json:"settled_amount,omitempty"`
	Status         string    `json:"status"`
	JournalUID     string    `json:"journal_uid,omitempty"`
	IdempotencyKey string    `json:"idempotency_key"`
	ExpiresAt      time.Time `json:"expires_at"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func toReservationResponse(r *core.Reservation) reservationResponse {
	resp := reservationResponse{
		UID:            r.UID,
		AccountHolder:  r.AccountHolder,
		CurrencyUID:    r.CurrencyUID,
		ReservedAmount: r.ReservedAmount.String(),
		Status:         string(r.Status),
		JournalUID:     r.JournalUID,
		IdempotencyKey: r.IdempotencyKey,
		ExpiresAt:      r.ExpiresAt,
		CreatedAt:      r.CreatedAt,
		UpdatedAt:      r.UpdatedAt,
	}
	if r.SettledAmount != nil {
		s := r.SettledAmount.String()
		resp.SettledAmount = &s
	}
	return resp
}

func (s *Server) handleCreateReservation(w http.ResponseWriter, r *http.Request) {
	req, err := httpx.Decode[createReservationRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	amount, err := parseWireAmount(req.Amount, "amount")
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("amount is not a valid decimal"))
		return
	}

	expiresIn := time.Duration(req.ExpiresInSec) * time.Second

	input := core.ReserveInput{
		AccountHolder:  req.AccountHolder,
		CurrencyUID:    req.CurrencyUID,
		Amount:         amount,
		IdempotencyKey: req.IdempotencyKey,
		ExpiresIn:      expiresIn,
	}

	reservation, err := s.reserver.Reserve(r.Context(), input)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.Created(w, toReservationResponse(reservation))
}

func (s *Server) handleSettleReservation(w http.ResponseWriter, r *http.Request) {
	uid := chi.URLParam(r, "uid")
	if uid == "" {
		httpx.Error(w, httpx.ErrBadRequest("invalid reservation uid"))
		return
	}

	req, err := httpx.Decode[settleReservationRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	amount, err := parseWireAmount(req.ActualAmount, "actual_amount")
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("actual_amount is not a valid decimal"))
		return
	}

	if err := s.reserver.Settle(r.Context(), core.SettleInput{ReservationUID: uid, Amount: amount}); err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, map[string]string{"status": "settled"})
}

func (s *Server) handleSettlePartialReservation(w http.ResponseWriter, r *http.Request) {
	uid := chi.URLParam(r, "uid")
	if uid == "" {
		httpx.Error(w, httpx.ErrBadRequest("invalid reservation uid"))
		return
	}

	req, err := httpx.Decode[settlePartialReservationRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	amount, err := parseWireAmount(req.Amount, "amount")
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("amount is not a valid decimal"))
		return
	}

	if req.IdempotencyKey == "" {
		httpx.Error(w, httpx.ErrBadRequest("idempotency_key is required"))
		return
	}
	if err := s.reserver.SettlePartial(r.Context(), core.SettlePartialInput{ReservationUID: uid, Amount: amount, IdempotencyKey: req.IdempotencyKey}); err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, map[string]string{"status": "settling"})
}

func (s *Server) handleFinalizeReservationSettlement(w http.ResponseWriter, r *http.Request) {
	uid := chi.URLParam(r, "uid")
	if uid == "" {
		httpx.Error(w, httpx.ErrBadRequest("invalid reservation uid"))
		return
	}

	if err := s.reserver.FinalizeSettlement(r.Context(), uid); err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, map[string]string{"status": "settled"})
}

func (s *Server) handleReleaseReservation(w http.ResponseWriter, r *http.Request) {
	uid := chi.URLParam(r, "uid")
	if uid == "" {
		httpx.Error(w, httpx.ErrBadRequest("invalid reservation uid"))
		return
	}

	if err := s.reserver.Release(r.Context(), uid); err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, map[string]string{"status": "released"})
}

func (s *Server) handleListReservations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	var holder int64
	if h := q.Get("holder"); h != "" {
		var err error
		holder, err = strconv.ParseInt(h, 10, 64)
		if err != nil {
			httpx.Error(w, httpx.ErrBadRequest("holder must be a number"))
			return
		}
	}
	status := q.Get("status")
	limit := parsePageLimit(r)
	cursor := q.Get("cursor")

	reservations, nextCursor, err := s.queries.ListReservations(r.Context(), holder, status, cursor, limit)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	data := make([]reservationResponse, len(reservations))
	for i, r := range reservations {
		data[i] = toReservationResponse(&r)
	}
	httpx.OK(w, PagedResponse[reservationResponse]{List: data, NextCursor: nextCursor})
}
