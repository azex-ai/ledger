package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/pkg/httpx"
)

type createReservationRequest struct {
	AccountHolder  int64  `json:"account_holder"`
	CurrencyID     int64  `json:"currency_id"`
	Amount         string `json:"amount"`
	IdempotencyKey string `json:"idempotency_key"`
	ExpiresInSec   int64  `json:"expires_in_sec"`
}

type settleReservationRequest struct {
	ActualAmount string `json:"actual_amount"`
}

type reservationResponse struct {
	ID             int64     `json:"id"`
	AccountHolder  int64     `json:"account_holder"`
	CurrencyID     int64     `json:"currency_id"`
	ReservedAmount string    `json:"reserved_amount"`
	SettledAmount  *string   `json:"settled_amount,omitempty"`
	Status         string    `json:"status"`
	JournalID      *int64    `json:"journal_id,omitempty"`
	IdempotencyKey string    `json:"idempotency_key"`
	ExpiresAt      time.Time `json:"expires_at"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func toReservationResponse(r *core.Reservation) reservationResponse {
	resp := reservationResponse{
		ID:             r.ID,
		AccountHolder:  r.AccountHolder,
		CurrencyID:     r.CurrencyID,
		ReservedAmount: r.ReservedAmount.String(),
		Status:         string(r.Status),
		JournalID:      r.JournalID,
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

	amount, err := decimal.NewFromString(req.Amount)
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("amount is not a valid decimal"))
		return
	}

	expiresIn := time.Duration(req.ExpiresInSec) * time.Second

	input := core.ReserveInput{
		AccountHolder:  req.AccountHolder,
		CurrencyID:     req.CurrencyID,
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
	id, err := parseIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid reservation ID"))
		return
	}

	req, err := httpx.Decode[settleReservationRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	amount, err := decimal.NewFromString(req.ActualAmount)
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("actual_amount is not a valid decimal"))
		return
	}

	if err := s.reserver.Settle(r.Context(), id, amount); err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.OK(w, map[string]string{"status": "settled"})
}

func (s *Server) handleReleaseReservation(w http.ResponseWriter, r *http.Request) {
	id, err := parseIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid reservation ID"))
		return
	}

	if err := s.reserver.Release(r.Context(), id); err != nil {
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

	reservations, err := s.queries.ListReservations(r.Context(), holder, status, limit)
	if err != nil {
		httpx.Error(w, err)
		return
	}

	data := make([]reservationResponse, len(reservations))
	for i, r := range reservations {
		data[i] = toReservationResponse(&r)
	}
	httpx.OK(w, data)
}
